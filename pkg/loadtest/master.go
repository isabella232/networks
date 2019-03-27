package loadtest

import (
	"fmt"
	"path"
	"sync"
	"time"

	"github.com/tendermint/networks/internal/logging"

	"github.com/AsynkronIT/protoactor-go/actor"
	"github.com/AsynkronIT/protoactor-go/remote"
	"github.com/tendermint/networks/pkg/loadtest/messages"
)

// Master is an actor that coordinates and collects information from the slaves,
// which are responsible for the actual load testing.
type Master struct {
	cfg    *Config
	probe  Probe
	logger logging.Logger

	slaves *actor.PIDSet
	istats *messages.CombinedStats // Interaction/request-derived statistics
	pstats *PrometheusStats        // Stats from Prometheus endpoints
	mtx    *sync.Mutex

	lastCheckin            map[string]time.Time // The last time we received a "load testing underway" message from each slave (ID -> last checkin time)
	checkinTicker          *time.Ticker
	stopSlaveHealthChecker chan bool

	statsShutdownc chan bool
	statsDonec     chan bool
}

// Master implements actor.Actor
var _ actor.Actor = (*Master)(nil)

// NewMaster will instantiate a new master node. On success, returns an actor
// PID with which one can interact with the master node. On failure, returns an
// error.
func NewMaster(cfg *Config, probe Probe) (*actor.PID, *actor.RootContext, error) {
	clientFactory := GetClientFactory(cfg.Clients.Type)
	remote.Start(cfg.Master.Bind)
	ctx := actor.EmptyRootContext
	props := actor.PropsFromProducer(func() actor.Actor {
		return &Master{
			cfg:    cfg,
			probe:  probe,
			logger: logging.NewLogrusLogger("master"),
			slaves: actor.NewPIDSet(),
			istats: clientFactory.NewStats(ClientParams{
				TargetNodes:        cfg.TestNetwork.GetTargetRPCURLs(),
				InteractionTimeout: time.Duration(cfg.Clients.InteractionTimeout),
				RequestTimeout:     time.Duration(cfg.Clients.RequestTimeout),
				RequestWaitMin:     time.Duration(cfg.Clients.RequestWaitMin),
				RequestWaitMax:     time.Duration(cfg.Clients.RequestWaitMax),
			}),
			pstats: &PrometheusStats{
				TargetNodeStats: make(map[string][]*NodePrometheusStats),
			},
			lastCheckin:            make(map[string]time.Time),
			checkinTicker:          nil,
			stopSlaveHealthChecker: make(chan bool, 1),
			mtx:                    &sync.Mutex{},
			statsShutdownc:         make(chan bool, 1),
			statsDonec:             make(chan bool, 1),
		}
	})
	pid, err := ctx.SpawnNamed(props, "master")
	if err != nil {
		return nil, nil, NewError(ErrFailedToCreateActor, err)
	}
	return pid, ctx, nil
}

// Receive handles incoming messages to the master node.
func (m *Master) Receive(ctx actor.Context) {
	switch msg := ctx.Message().(type) {
	case *actor.Started:
		m.onStartup(ctx)

	case *actor.Stopped:
		m.onStopped(ctx)

	case *messages.CheckAllSlavesConnected:
		m.checkAllSlavesConnected(ctx, msg)

	case *messages.SlaveReady:
		m.slaveReady(ctx, msg)

	case *messages.SlaveFailed:
		m.slaveFailed(ctx, msg)

	case *messages.LoadTestUnderway:
		m.trackSlaveCheckin(ctx.Sender().GetId())

	case *messages.SlaveFinished:
		m.slaveFinished(ctx, msg)

	case *messages.MasterFailed:
		m.masterFailed(ctx, msg)

	case *messages.Kill:
		m.kill(ctx)
	}
}

func (m *Master) onStartup(ctx actor.Context) {
	m.logger.Info("Starting up master node", "addr", ctx.Self().String())
	go func(ctx_ actor.Context) {
		time.Sleep(time.Duration(m.cfg.Master.ExpectSlavesWithin))
		ctx_.Send(ctx_.Self(), &messages.CheckAllSlavesConnected{})
	}(ctx)
	// fire up our Prometheus collector routine
	go func() {
		m.pstats.RunCollectors(m.cfg, m.statsShutdownc, m.statsDonec, m.logger)
	}()
	if m.probe != nil {
		m.probe.OnStartup(ctx)
	}
}

func (m *Master) onStopped(ctx actor.Context) {
	m.logger.Info("Master node stopped")
	if m.probe != nil {
		m.probe.OnStopped(ctx)
	}
}

func (m *Master) checkAllSlavesConnected(ctx actor.Context, msg *messages.CheckAllSlavesConnected) {
	if m.slaves.Len() != m.cfg.Master.ExpectSlaves {
		m.logger.Error("Timed out waiting for all slaves to connect", "slaveCount", m.slaves.Len(), "expected", m.cfg.Master.ExpectSlaves)
		m.broadcast(ctx, &messages.MasterFailed{
			Sender: ctx.Self(),
			Reason: "Timed out waiting for all slaves to connect",
		})
		m.shutdown(ctx, NewError(ErrTimedOutWaitingForSlaves, nil))
	} else {
		m.logger.Debug("All slaves connected within timeout limit - no need to terminate master")
	}
}

func (m *Master) slaveReady(ctx actor.Context, msg *messages.SlaveReady) {
	slave := msg.Sender
	slaveID := slave.String()
	m.logger.Info("Got SlaveReady message", "id", slaveID)
	// keep track of this new incoming slave
	if m.slaves.Contains(slave) {
		m.logger.Error("Already seen slave before - rejecting", "id", slaveID)
		ctx.Send(slave, &messages.SlaveRejected{
			Sender: ctx.Self(),
			Reason: "Already seen slave",
		})
	} else {
		// keep track of the slave
		m.slaves.Add(slave)
		m.logger.Info("Added incoming slave", "slaveCount", m.slaves.Len(), "expected", m.cfg.Master.ExpectSlaves)
		m.trackSlaveCheckin(slave.GetId())
		// tell the slave it's got the go-ahead
		ctx.Send(slave, &messages.SlaveAccepted{Sender: ctx.Self()})
		// if we have enough slaves to start the load testing
		if m.slaves.Len() == m.cfg.Master.ExpectSlaves {
			m.startLoadTest(ctx)
		}
	}
}

func (m *Master) startLoadTest(ctx actor.Context) {
	m.logger.Info("Accepted enough connected slaves - starting load test", "slaveCount", m.slaves.Len())
	m.broadcast(ctx, &messages.StartLoadTest{Sender: ctx.Self()})
	m.checkinTicker = time.NewTicker(DefaultHealthCheckInterval)
	go m.slaveHealthChecker(ctx)
}

func (m *Master) slaveHealthChecker(ctx actor.Context) {
loop:
	for {
		select {
		case <-m.checkinTicker.C:
			m.checkSlaveHealth(ctx)

		case <-m.stopSlaveHealthChecker:
			break loop
		}
	}
}

func (m *Master) checkSlaveHealth(ctx actor.Context) {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	for slaveID, lastSeen := range m.lastCheckin {
		if time.Since(lastSeen) >= DefaultMaxMissedHealthCheckPeriod {
			m.logger.Error("Failed to see slave recently enough", "slaveID", slaveID, "lastSeen", lastSeen.String())
			ctx.Send(ctx.Self(), &messages.MasterFailed{
				Sender: ctx.Self(),
				Reason: fmt.Sprintf("Failed to see slave %s within %s", slaveID, DefaultMaxMissedHealthCheckPeriod.String()),
			})
			// don't bother checking any of the other slaves (plus this will
			// most likely result in bombardment of the slaves with MasterFailed
			// messages)
			return
		}
	}
}

func (m *Master) masterFailed(ctx actor.Context, msg *messages.MasterFailed) {
	// rebroadcast this message to the slaves
	m.broadcast(ctx, msg)
	// we're done here
	m.shutdown(ctx, NewError(ErrSlaveFailed, nil))
}

func (m *Master) trackSlaveCheckin(slaveID string) {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	m.lastCheckin[slaveID] = time.Now()
	m.logger.Debug("Tracking slave check-in", "slaveID", slaveID, "time", m.lastCheckin[slaveID])
}

func (m *Master) broadcast(ctx actor.Context, msg interface{}) {
	m.logger.Debug("Broadcasting message to all slaves", "msg", msg)
	m.slaves.ForEach(func(i int, pid actor.PID) {
		m.logger.Debug("Broadcasting message to slave", "pid", pid)
		ctx.Send(&pid, msg)
	})
}

func (m *Master) slaveFailed(ctx actor.Context, msg *messages.SlaveFailed) {
	slave := msg.Sender
	slaveID := slave.String()
	m.logger.Error("Slave failed", "id", slaveID, "reason", msg.Reason)
	m.slaves.Remove(slave)
	m.broadcast(ctx, &messages.MasterFailed{Sender: ctx.Self(), Reason: "One other attached slave failed"})
	m.shutdown(ctx, NewError(ErrSlaveFailed, nil))
}

func (m *Master) slaveFinished(ctx actor.Context, msg *messages.SlaveFinished) {
	slave := msg.Sender
	slaveID := slave.String()
	m.logger.Info("Slave finished", "id", slaveID)
	m.updateStats(msg.Stats)
	m.slaves.Remove(slave)
	// if we've heard from all the slaves we accepted
	if m.slaves.Len() == 0 {
		m.logger.Info("All slaves successfully completed their load testing")
		m.shutdown(ctx, nil)
	}
}

func (m *Master) kill(ctx actor.Context) {
	m.logger.Error("Master killed")
	m.broadcast(ctx, &messages.MasterFailed{Sender: ctx.Self(), Reason: "Master killed"})
	m.shutdown(ctx, NewError(ErrKilled, nil))
}

func (m *Master) stopPrometheusCollectors() {
	m.statsShutdownc <- true
	select {
	case <-m.statsDonec:
		m.logger.Debug("Prometheus collectors successfully shut down")
	case <-time.After(30 * time.Second):
		m.logger.Error("Timed out waiting for Prometheus collectors to shut down")
	}
}

func (m *Master) shutdown(ctx actor.Context, err error) {
	// stop checking slave health
	m.stopSlaveHealthChecker <- true
	m.stopPrometheusCollectors()
	m.writeStats()
	if err != nil {
		m.logger.Error("Shutting down master node", "err", err)
	} else {
		m.logger.Info("Shutting down master node")
	}
	if m.probe != nil {
		m.probe.OnShutdown(ctx, err)
	}
	ctx.Self().GracefulStop()
}

func (m *Master) updateStats(stats *messages.CombinedStats) {
	m.mtx.Lock()
	MergeCombinedStats(m.istats, stats)
	m.mtx.Unlock()
}

func (m *Master) writeStats() {
	m.mtx.Lock()
	LogStats(logging.NewLogrusLogger(""), m.istats)
	filename := path.Join(m.cfg.Master.ResultsDir, "summary.csv")
	if err := WriteCombinedStatsToFile(filename, m.istats); err != nil {
		m.logger.Error("Failed to write final statistics to output CSV file", "filename", filename)
	}
	if err := m.pstats.Dump(m.cfg.Master.ResultsDir); err != nil {
		m.logger.Error("Failed to write Prometheus stats to output directory", "err", err)
	}
	m.mtx.Unlock()
}
