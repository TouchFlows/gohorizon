package process

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Spec configures one supervisor's process pool.
type Spec struct {
	ApplicationID string
	Environment   string
	SupervisorID  string

	// Launch is the template; Queue and Name are set per slot.
	Launch LaunchSpec
	// Queues is the allocation order: slot N gets Queues[N % len(Queues)].
	Queues []string
	// Desired is the fixed pool size for Milestone 1 (workers.min).
	Desired int

	ShutdownGrace  time.Duration
	RestartBackoff time.Duration
	StatePath      string

	Logger *slog.Logger
	Now    func() time.Time
	Tick   time.Duration

	// Probe and Scan are injectable for tests; they default to the platform probe.
	Probe func(int) (ProcInfo, error)
	Scan  func() ([]ProcInfo, error)
}

// Stats is a point-in-time view used by the status API and tests.
type Stats struct {
	SupervisorID string   `json:"supervisor_id"`
	Desired      int      `json:"desired"`
	Running      int      `json:"running"`
	Stopping     int      `json:"stopping"`
	Crashed      int      `json:"crashed"`
	Adopted      int      `json:"adopted"`
	Escalations  int      `json:"escalations"`
	Workers      []Record `json:"workers"`
}

type instance struct {
	rec Record

	cmd     *exec.Cmd
	done    chan struct{}
	waitErr error

	exited        bool
	nextStart     time.Time
	stoppingSince time.Time
	escalated     bool
}

// Manager owns the worker processes for one supervisor.
type Manager struct {
	spec Spec
	log  *slog.Logger

	now   func() time.Time
	probe func(int) (ProcInfo, error)
	scan  func() ([]ProcInfo, error)
	tick  time.Duration

	mu          sync.Mutex
	slots       []*instance
	overflow    []*instance // adopted processes beyond the desired pool
	stopping    bool
	started     bool
	adopted     int
	escalations int
}

func New(spec Spec) (*Manager, error) {
	if len(spec.Queues) == 0 {
		return nil, errors.New("supervisor requires at least one queue")
	}
	if spec.Desired < 0 {
		return nil, fmt.Errorf("desired worker count must be zero or positive, got %d", spec.Desired)
	}
	if spec.ShutdownGrace <= 0 {
		return nil, errors.New("shutdown grace must be positive")
	}
	if spec.RestartBackoff <= 0 {
		spec.RestartBackoff = time.Second
	}
	if spec.Tick <= 0 {
		spec.Tick = time.Second
	}
	if spec.Tick > spec.ShutdownGrace/2 {
		spec.Tick = spec.ShutdownGrace / 2
	}
	if spec.Logger == nil {
		spec.Logger = slog.Default()
	}
	if spec.Now == nil {
		spec.Now = time.Now
	}
	if spec.Probe == nil {
		spec.Probe = Probe
	}
	if spec.Scan == nil {
		spec.Scan = Scan
	}

	// Validate the template now rather than discovering a bad command on the first
	// crash-restart, when there is no worker left to report it.
	template := spec.Launch
	template.Queue = spec.Queues[0]
	template.Name = WorkerName(spec.ApplicationID, spec.Environment, spec.SupervisorID, "00000000")
	if err := template.Validate(); err != nil {
		return nil, err
	}

	return &Manager{
		spec:  spec,
		log:   spec.Logger.With("supervisor", spec.SupervisorID, "application", spec.ApplicationID),
		now:   spec.Now,
		probe: spec.Probe,
		scan:  spec.Scan,
		tick:  spec.Tick,
		slots: make([]*instance, spec.Desired),
	}, nil
}

// Run adopts existing workers, then reconciles until the context is cancelled.
func (m *Manager) Run(ctx context.Context) error {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return errors.New("supervisor manager already started")
	}
	m.started = true
	m.mu.Unlock()

	m.adopt()

	ticker := time.NewTicker(m.tick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.shutdown()
			return nil
		case <-ticker.C:
			m.tickOnce()
		}
	}
}

// Snapshot returns a copy-safe view of the pool.
func (m *Manager) Snapshot() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()

	stats := Stats{
		SupervisorID: m.spec.SupervisorID,
		Desired:      m.spec.Desired,
		Adopted:      m.adopted,
		Escalations:  m.escalations,
	}
	for _, inst := range append(append([]*instance{}, m.slots...), m.overflow...) {
		if inst == nil {
			continue
		}
		rec := inst.rec
		rec.Transitions = append([]Transition{}, inst.rec.Transitions...)
		stats.Workers = append(stats.Workers, rec)
		switch rec.State {
		case StateRunning:
			stats.Running++
		case StateStopping:
			stats.Stopping++
		case StateCrashed:
			stats.Crashed++
		}
	}
	return stats
}

// StopWorker gracefully stops one worker by UUID, leaving the rest untouched.
func (m *Manager) StopWorker(uuid string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, inst := range append(append([]*instance{}, m.slots...), m.overflow...) {
		if inst == nil || inst.rec.UUID != uuid {
			continue
		}
		if inst.exited {
			return fmt.Errorf("worker %s is not running", uuid)
		}
		m.stopLocked(inst)
		return nil
	}
	return fmt.Errorf("worker %s not found", uuid)
}

func (m *Manager) tickOnce() {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()

	for _, inst := range append(append([]*instance{}, m.slots...), m.overflow...) {
		if inst == nil || inst.exited {
			continue
		}
		m.observeLocked(inst)
		if inst.exited {
			m.afterExitLocked(inst, now)
		}
	}

	if !m.stopping {
		for slot := range m.slots {
			inst := m.slots[slot]
			if inst == nil || (inst.exited && !now.Before(inst.nextStart)) {
				m.startLocked(slot, now)
			}
		}
	}

	m.persistLocked()
}

// observeLocked finds out whether a worker died and, for a stopping worker, whether
// the grace period has expired.
func (m *Manager) observeLocked(inst *instance) {
	now := m.now()

	if inst.cmd != nil {
		select {
		case <-inst.done:
			inst.exited = true
			inst.rec.LastExitCode, inst.rec.LastExitReason = exitStatus(inst.waitErr)
		default:
		}
	} else if inst.rec.PID > 0 {
		// Adopted process: we are not its parent, so there is no wait status. Identity
		// (pid + kernel start time) decides whether it is still the process we adopted.
		if _, err := m.probe(inst.rec.PID); err != nil {
			inst.exited = true
			inst.rec.LastExitCode = -1
			inst.rec.LastExitReason = "process disappeared (adopted worker, exit status unknown)"
		}
	}

	if inst.exited || inst.rec.State != StateStopping {
		return
	}

	if now.Sub(inst.stoppingSince) >= m.spec.ShutdownGrace && !inst.escalated {
		inst.escalated = true
		m.escalations++
		m.log.Warn("worker did not stop within the shutdown grace, escalating to SIGKILL",
			"pid", inst.rec.PID, "uuid", inst.rec.UUID, "grace", m.spec.ShutdownGrace.String())
		if err := signalKillGroup(inst.rec.PID); err != nil {
			m.log.Error("SIGKILL escalation failed", "pid", inst.rec.PID, "error", err)
		}
	}
}

func (m *Manager) afterExitLocked(inst *instance, now time.Time) {
	rec := &inst.rec
	code, rawReason := rec.LastExitCode, rec.LastExitReason

	if rec.State == StateStopping {
		if err := rec.Transition(StateStopped, "stopped: "+rawReason, now); err != nil {
			m.log.Error("state transition failed", "error", err)
		}
		rec.PID = 0
		return
	}

	if startedAgo(*rec, now) > m.stableWindow() {
		rec.RestartCount = 0
	}

	// Laravel's own recycling paths: exit 0 (max-jobs, max-time, restart signal,
	// queue empty) and exit 12 (memory limit) are not failures.
	if code == 0 || code == 12 {
		rec.LastExitReason = classifyRecycleExit(code, rawReason)
		if err := rec.Transition(StateStopped, "recycled: "+rec.LastExitReason, now); err != nil {
			m.log.Error("state transition failed", "error", err)
		}
		inst.nextStart = now.Add(m.spec.RestartBackoff)
		rec.PID = 0
		return
	}

	rec.RestartCount++
	rec.LastExitReason = classifyCrashExit(code, rawReason)
	if err := rec.Transition(StateCrashed, rec.LastExitReason, now); err != nil {
		m.log.Error("state transition failed", "error", err)
	}
	inst.nextStart = now.Add(m.backoffFor(rec.RestartCount))
	rec.PID = 0
	m.log.Warn("worker crashed", "uuid", rec.UUID, "queue", rec.Queue,
		"exit_code", code, "reason", rec.LastExitReason, "restarts", rec.RestartCount,
		"next_start_in", inst.nextStart.Sub(now).Round(time.Millisecond).String())
}

func (m *Manager) startLocked(slot int, now time.Time) {
	var rec Record
	if prev := m.slots[slot]; prev != nil && prev.rec.UUID != "" {
		rec = prev.rec
		from := rec.State
		if from == StateStopped {
			if err := rec.Transition(StateStarting, "restarting recycled worker", now); err != nil {
				m.log.Error("state transition failed", "error", err)
				return
			}
		} else {
			if err := rec.Transition(StateRestarting, "restarting after crash", now); err != nil {
				m.log.Error("state transition failed", "error", err)
				return
			}
			if err := rec.Transition(StateStarting, "spawning replacement", now); err != nil {
				m.log.Error("state transition failed", "error", err)
				return
			}
		}
	} else {
		rec = Record{
			ApplicationID: m.spec.ApplicationID,
			Environment:   m.spec.Environment,
			SupervisorID:  m.spec.SupervisorID,
			UUID:          newUUID(),
			Slot:          slot,
			Queue:         m.queueForSlot(slot),
			State:         StateStopped,
		}
		if err := rec.Transition(StateStarting, "spawning", now); err != nil {
			m.log.Error("state transition failed", "error", err)
			return
		}
	}

	launch := m.spec.Launch
	launch.Queue = rec.Queue
	launch.Name = WorkerName(m.spec.ApplicationID, m.spec.Environment, m.spec.SupervisorID, rec.UUID)
	rec.Name = launch.Name
	rec.Slot = slot
	rec.StartedAt = now
	rec.StoppedAt = nil

	if err := launch.Validate(); err != nil {
		rec.LastExitCode = -1
		rec.LastExitReason = "spawn refused: " + err.Error()
		_ = rec.Transition(StateCrashed, rec.LastExitReason, now)
		m.slots[slot] = &instance{rec: rec, exited: true, nextStart: now.Add(m.backoffFor(rec.RestartCount + 1))}
		return
	}

	cmd := launch.Command()
	if err := cmd.Start(); err != nil {
		rec.LastExitCode = -1
		rec.LastExitReason = "spawn failed: " + err.Error()
		_ = rec.Transition(StateCrashed, rec.LastExitReason, now)
		m.slots[slot] = &instance{rec: rec, exited: true, nextStart: now.Add(m.backoffFor(rec.RestartCount + 1))}
		m.log.Error("worker failed to start", "queue", rec.Queue, "error", err)
		return
	}

	inst := &instance{rec: rec, cmd: cmd, done: make(chan struct{})}
	inst.rec.PID = cmd.Process.Pid
	if info, err := m.probe(cmd.Process.Pid); err == nil {
		inst.rec.StartTicks = info.StartTicks
	}
	go func() {
		inst.waitErr = cmd.Wait()
		close(inst.done)
	}()

	if err := inst.rec.Transition(StateRunning, "process started", now); err != nil {
		m.log.Error("state transition failed", "error", err)
	}
	m.slots[slot] = inst
	m.log.Info("worker started", "uuid", inst.rec.UUID, "queue", inst.rec.Queue, "pid", inst.rec.PID)
}

func (m *Manager) stopLocked(inst *instance) {
	now := m.now()
	if err := inst.rec.Transition(StateStopping, "stop requested", now); err != nil {
		m.log.Error("state transition failed", "error", err)
		return
	}
	inst.stoppingSince = now
	if err := signalTerminate(inst.rec.PID); err != nil {
		m.log.Warn("SIGTERM failed", "pid", inst.rec.PID, "error", err)
	}
}

func (m *Manager) shutdown() {
	m.mu.Lock()
	m.stopping = true
	for _, inst := range append(append([]*instance{}, m.slots...), m.overflow...) {
		if inst == nil || inst.exited {
			continue
		}
		if inst.rec.State.Alive() {
			m.stopLocked(inst)
		}
	}
	m.mu.Unlock()

	deadline := m.now().Add(m.spec.ShutdownGrace + 5*time.Second)
	for m.now().Before(deadline) {
		m.tickOnce()
		if m.allStopped() {
			break
		}
		sleep := m.tick
		if sleep > 50*time.Millisecond {
			sleep = 50 * time.Millisecond
		}
		time.Sleep(sleep)
	}

	m.mu.Lock()
	for _, inst := range append(m.slots, m.overflow...) {
		if inst == nil || inst.rec.State == StateStopped {
			continue
		}
		inst.rec.LastExitReason = "supervisor shutdown before the worker confirmed exit"
		_ = inst.rec.Transition(StateStopped, inst.rec.LastExitReason, m.now())
		inst.rec.PID = 0
	}
	m.persistLocked()
	m.mu.Unlock()

	stats := m.Snapshot()
	m.log.Info("supervisor stopped", "running", stats.Running, "escalations", stats.Escalations)
	if stats.Running > 0 {
		m.log.Error("workers still running after shutdown deadline", "count", stats.Running)
	}
}

func (m *Manager) allStopped() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, inst := range append(append([]*instance{}, m.slots...), m.overflow...) {
		if inst == nil {
			continue
		}
		if inst.rec.State.Alive() || !inst.exited {
			return false
		}
	}
	return true
}

func (m *Manager) queueForSlot(slot int) string {
	return m.spec.Queues[slot%len(m.spec.Queues)]
}

func (m *Manager) backoffFor(streak int) time.Duration {
	if streak <= 1 {
		return m.spec.RestartBackoff
	}
	d := m.spec.RestartBackoff
	for i := 1; i < streak; i++ {
		d *= 2
		if d >= time.Minute {
			return time.Minute
		}
	}
	return d
}

func (m *Manager) stableWindow() time.Duration {
	return 2*m.spec.RestartBackoff + 30*time.Second
}

func (m *Manager) persistLocked() {
	if m.spec.StatePath == "" {
		return
	}
	var records []Record
	for _, inst := range append(append([]*instance{}, m.slots...), m.overflow...) {
		if inst == nil {
			continue
		}
		if inst.rec.State == StateStopped {
			continue
		}
		records = append(records, inst.rec)
	}
	if err := WriteStateFile(m.spec.StatePath, records); err != nil {
		m.log.Warn("could not persist worker state", "path", m.spec.StatePath, "error", err)
	}
}

// adopt reconciles the desired pool with processes a previous GoHorizon left behind.
// It never kills an unverified pid and never spawns a duplicate of a verified one.
func (m *Manager) adopt() {
	if m.spec.Desired == 0 {
		return
	}

	now := m.now()
	saved := ReadStateFile(m.spec.StatePath)
	scanned, err := m.scan()
	if err != nil {
		m.log.Warn("process inspection unavailable, adoption disabled", "error", err)
		return
	}
	byPID := make(map[int]ProcInfo, len(scanned))
	for _, info := range scanned {
		byPID[info.PID] = info
	}

	prefix := workerNamePrefix(m.spec.ApplicationID, m.spec.Environment, m.spec.SupervisorID)
	used := make(map[int]bool)
	type candidate struct {
		rec  Record
		info ProcInfo
	}
	var candidates []candidate

	// 1. Trust the journal only when pid, kernel start time and worker name all match.
	for _, rec := range saved {
		if rec.SupervisorID != m.spec.SupervisorID || rec.PID <= 0 || used[rec.PID] {
			continue
		}
		info, ok := byPID[rec.PID]
		if !ok || info.StartTicks != rec.StartTicks || !info.HasFlag("name", rec.Name) {
			continue
		}
		rec.Adopted = true
		candidates = append(candidates, candidate{rec: rec, info: info})
		used[info.PID] = true
	}

	// 2. Pick up workers the journal missed, matched by our own --name prefix only.
	for _, info := range scanned {
		if used[info.PID] {
			continue
		}
		name := info.Flag("name")
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		uuid := strings.TrimPrefix(name, prefix)
		candidates = append(candidates, candidate{
			rec: Record{
				ApplicationID: m.spec.ApplicationID,
				Environment:   m.spec.Environment,
				SupervisorID:  m.spec.SupervisorID,
				UUID:          uuid,
				Queue:         info.Flag("queue"),
				Name:          name,
				PID:           info.PID,
				StartTicks:    info.StartTicks,
				Adopted:       true,
				State:         StateStopped,
				StartedAt:     now,
			},
			info: info,
		})
		used[info.PID] = true
	}

	if len(candidates) == 0 {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, c := range candidates {
		rec := c.rec
		slot := m.freeSlotForQueue(rec.Queue)
		// The journal records the state at the previous shutdown; adoption re-bases the
		// record on "no process tracked", then re-enters the machine explicitly.
		rec.State = StateStopped
		if err := rec.Transition(StateStarting, "adopted after supervisor restart", now); err != nil {
			m.log.Error("adopted worker state transition failed", "error", err)
			continue
		}
		if err := rec.Transition(StateRunning, "existing process verified", now); err != nil {
			m.log.Error("adopted worker state transition failed", "error", err)
			continue
		}
		inst := &instance{rec: rec, exited: false}

		if slot < 0 {
			// More processes than desired: stop the surplus gracefully instead of
			// killing it or running a pool larger than configured.
			m.log.Warn("adopted worker exceeds the desired pool, stopping it", "pid", rec.PID)
			inst.rec.Transition(StateStopping, "surplus adopted worker", now)
			inst.stoppingSince = now
			_ = signalTerminate(rec.PID)
			m.overflow = append(m.overflow, inst)
			continue
		}

		m.slots[slot] = inst
		m.adopted++
		m.log.Info("adopted existing worker", "uuid", rec.UUID, "queue", rec.Queue, "pid", rec.PID, "slot", slot)
	}
}

func (m *Manager) freeSlotForQueue(queue string) int {
	for slot := range m.slots {
		if m.slots[slot] == nil && m.queueForSlot(slot) == queue {
			return slot
		}
	}
	for slot := range m.slots {
		if m.slots[slot] == nil {
			return slot
		}
	}
	return -1
}

func classifyRecycleExit(code int, raw string) string {
	if code == 12 {
		return "memory limit exceeded (recycled)"
	}
	return "worker exited cleanly (max-jobs, max-time, restart signal or empty queue)"
}

func classifyCrashExit(code int, raw string) string {
	switch code {
	case 1:
		return "worker error, or job timeout (Laravel exits 1 either way; the adapter supplies the exact stop reason)"
	case -1:
		if raw != "" {
			return raw
		}
		return "worker killed by a signal"
	default:
		if raw != "" {
			return raw
		}
		return fmt.Sprintf("worker exited with status %d", code)
	}
}
