package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestHelperProcess is the worker process used by these tests. It is launched by the
// real supervisor code (same argv shape, same process group), and behaves according to
// GOHORIZON_TEST_MODE.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv("GOHORIZON_TEST_MODE")
	if mode == "" {
		t.Skip("helper process only; launched by the supervisor tests")
	}

	signalReady := func() {
		if path := os.Getenv("GOHORIZON_TEST_READY_FILE"); path != "" {
			_ = os.WriteFile(path, []byte("ready"), 0o600)
		}
	}

	switch mode {
	case "run":
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM)
		signalReady()
		<-ch
		os.Exit(0) // Laravel exits 0 when interrupted
	case "ignore-term":
		signal.Ignore(syscall.SIGTERM)
		signalReady()
		for {
			time.Sleep(time.Hour)
		}
	case "exit0":
		os.Exit(0)
	case "exit12":
		os.Exit(12)
	case "exit1":
		os.Exit(1)
	default:
		fmt.Fprintln(os.Stderr, "unknown GOHORIZON_TEST_MODE", mode)
		os.Exit(2)
	}
}

func helperSpec(t *testing.T, mode string, desired int, grace, backoff time.Duration) Spec {
	t.Helper()
	return helperSpecEnv(t, mode, desired, grace, backoff, nil)
}

func helperSpecEnv(t *testing.T, mode string, desired int, grace, backoff time.Duration, extraEnv []string) Spec {
	t.Helper()
	env := append([]string{"GOHORIZON_TEST_MODE=" + mode}, extraEnv...)
	return Spec{
		ApplicationID: "production",
		Environment:   "production",
		SupervisorID:  "default-supervisor",
		Launch: LaunchSpec{
			PHP:        os.Args[0],
			Artisan:    "-test.run=TestHelperProcess",
			Dir:        t.TempDir(),
			Connection: "redis",
			Sleep:      1,
			Tries:      1,
			Timeout:    60,
			Memory:     "128",
			Env:        env,
		},
		Queues:         []string{"default"},
		Desired:        desired,
		ShutdownGrace:  grace,
		RestartBackoff: backoff,
		StatePath:      filepath.Join(t.TempDir(), "workers.json"),
		Logger:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		Tick:           10 * time.Millisecond,
	}
}

func startManager(t *testing.T, spec Spec) (*Manager, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	manager, err := New(spec)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := manager.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("manager did not stop within 10s")
		}
		for _, w := range manager.Snapshot().Workers {
			if w.PID > 0 && w.PID != os.Getpid() {
				_ = signalKillGroup(w.PID)
			}
		}
	})
	return manager, cancel, done
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, description)
}

func TestManagerStartsAndRunsOneWorkerPerSlot(t *testing.T) {
	spec := helperSpec(t, "run", 2, 2*time.Second, 50*time.Millisecond)
	spec.Queues = []string{"default", "emails"}
	manager, _, _ := startManager(t, spec)

	waitFor(t, 5*time.Second, func() bool {
		return manager.Snapshot().Running == 2
	}, "two running workers")

	stats := manager.Snapshot()
	if stats.Desired != 2 || len(stats.Workers) != 2 {
		t.Fatalf("unexpected snapshot: %+v", stats)
	}
	queues := map[string]bool{}
	for _, w := range stats.Workers {
		queues[w.Queue] = true
		if w.PID <= 0 {
			t.Errorf("worker %s has no pid", w.UUID)
		}
		if w.State != StateRunning {
			t.Errorf("worker %s state = %s, want running", w.UUID, w.State)
		}
		if _, err := Probe(w.PID); err != nil {
			t.Errorf("worker %s pid %d not alive: %v", w.UUID, w.PID, err)
		}
	}
	// ADR 0005: one worker per queue, never one worker on both queues.
	if !queues["default"] || !queues["emails"] {
		t.Errorf("expected one worker per queue, got %v", queues)
	}
}

func TestManagerGracefulStopSendsSigtermAndWaits(t *testing.T) {
	spec := helperSpec(t, "run", 1, 2*time.Second, 50*time.Millisecond)
	manager, cancel, done := startManager(t, spec)

	waitFor(t, 5*time.Second, func() bool { return manager.Snapshot().Running == 1 }, "running worker")
	worker := manager.Snapshot().Workers[0]

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("manager did not stop")
	}

	stats := manager.Snapshot()
	if stats.Escalations != 0 {
		t.Errorf("escalations = %d, want 0 for a worker that honours SIGTERM", stats.Escalations)
	}
	if stats.Workers[0].State != StateStopped {
		t.Errorf("final state = %s, want stopped", stats.Workers[0].State)
	}
	if _, err := Probe(worker.PID); !errors.Is(err, ErrProcessGone) {
		t.Errorf("worker pid %d still exists after graceful stop: %v", worker.PID, err)
	}
}

func TestManagerEscalatesToSigkillAfterGrace(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	spec := helperSpecEnv(t, "ignore-term", 1, 150*time.Millisecond, 20*time.Millisecond,
		[]string{"GOHORIZON_TEST_READY_FILE=" + ready})
	manager, cancel, done := startManager(t, spec)

	waitFor(t, 5*time.Second, func() bool { return manager.Snapshot().Running == 1 }, "running worker")
	// The worker must have installed its SIGTERM handler before we stop it, otherwise
	// the default disposition kills it and the escalation path is never exercised.
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(ready)
		return err == nil
	}, "worker to finish installing its signal handler")
	pid := manager.Snapshot().Workers[0].PID

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("manager did not stop")
	}

	stats := manager.Snapshot()
	if stats.Escalations != 1 {
		t.Errorf("escalations = %d, want 1", stats.Escalations)
	}
	if stats.Workers[0].State != StateStopped {
		t.Errorf("final state = %s, want stopped", stats.Workers[0].State)
	}
	waitFor(t, 2*time.Second, func() bool {
		_, err := Probe(pid)
		return errors.Is(err, ErrProcessGone)
	}, "SIGKILLed worker to disappear")
}

func TestManagerRestartsCrashedWorkerWithBackoff(t *testing.T) {
	spec := helperSpec(t, "exit1", 1, 2*time.Second, 20*time.Millisecond)
	manager, _, _ := startManager(t, spec)

	waitFor(t, 5*time.Second, func() bool {
		stats := manager.Snapshot()
		return len(stats.Workers) == 1 && stats.Workers[0].RestartCount >= 2
	}, "worker to be restarted at least twice after crashes")

	worker := manager.Snapshot().Workers[0]
	if worker.LastExitCode != 1 {
		t.Errorf("last exit code = %d, want 1", worker.LastExitCode)
	}
	if worker.LastExitReason == "" {
		t.Error("crash reason was not recorded")
	}
	if worker.UUID == "" {
		t.Error("worker lost its uuid across restarts")
	}
}

// syncBuffer collects the manager's logs while its goroutine is still writing them.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestManagerReportsACrashLoopAfterRepeatedPrematureCrashes(t *testing.T) {
	logs := &syncBuffer{}
	spec := helperSpec(t, "exit1", 1, 2*time.Second, 20*time.Millisecond)
	spec.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	manager, _, _ := startManager(t, spec)

	waitFor(t, 5*time.Second, func() bool {
		stats := manager.Snapshot()
		return len(stats.Workers) == 1 && stats.Workers[0].CrashLooping
	}, "the worker to be reported as a crash loop")

	if restarts := manager.Snapshot().Workers[0].RestartCount; restarts < crashLoopThreshold {
		t.Errorf("restarts = %d, want at least %d before the loop is reported", restarts, crashLoopThreshold)
	}

	// It keeps crashing; the escalation must be one line per streak, not one per crash.
	waitFor(t, 5*time.Second, func() bool {
		return manager.Snapshot().Workers[0].RestartCount >= crashLoopThreshold+3
	}, "three more crashes after the loop was reported")
	if got := strings.Count(logs.String(), "crash looping"); got != 1 {
		t.Errorf("crash-loop escalations = %d, want exactly one per streak; logs:\n%s", got, logs.String())
	}
}

func TestCrashLoopFlagCoversOnlyTheStreak(t *testing.T) {
	manager, err := New(helperSpec(t, "run", 1, 2*time.Second, 20*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := manager.now()

	tests := []struct {
		name string
		rec  Record
		want bool
	}{
		{"a worker with one crash", Record{State: StateCrashed, RestartCount: 1, StartedAt: now}, false},
		{"the crash that starts the loop", Record{State: StateCrashed, RestartCount: crashLoopThreshold, StartedAt: now.Add(-time.Second)}, true},
		{"its replacement, still on probation", Record{State: StateRunning, RestartCount: crashLoopThreshold, StartedAt: now.Add(-time.Second)}, true},
		{"its replacement, past the stable window", Record{State: StateRunning, RestartCount: crashLoopThreshold, StartedAt: now.Add(-2 * manager.stableWindow())}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			manager.slots[0] = &instance{rec: tc.rec}
			workers := manager.Snapshot().Workers
			if len(workers) != 1 {
				t.Fatalf("workers = %+v, want one", workers)
			}
			if workers[0].CrashLooping != tc.want {
				t.Errorf("crash_looping = %v, want %v (restarts %d, state %s)",
					workers[0].CrashLooping, tc.want, tc.rec.RestartCount, tc.rec.State)
			}
		})
	}
}

func TestManagerRecyclesCleanExitsWithoutCountingCrashes(t *testing.T) {
	spec := helperSpec(t, "exit12", 1, 2*time.Second, 20*time.Millisecond)
	manager, _, _ := startManager(t, spec)

	seen := map[int]bool{}
	waitFor(t, 5*time.Second, func() bool {
		workers := manager.Snapshot().Workers
		if len(workers) == 0 {
			return false
		}
		seen[workers[0].PID] = true
		return len(seen) >= 3 // the pool recycled the exited worker several times
	}, "memory-limit recycles")

	worker := manager.Snapshot().Workers[0]
	if worker.RestartCount != 0 {
		t.Errorf("restart count = %d, want 0: exit 12 is a recycle, not a crash", worker.RestartCount)
	}
	if !strings.Contains(worker.LastExitReason, "memory") {
		t.Errorf("last exit reason = %q, want it to mention memory", worker.LastExitReason)
	}
}

func TestManagerClassifiesExitsByTheReportedStopReason(t *testing.T) {
	manager, err := New(helperSpec(t, "run", 1, 2*time.Second, 20*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	name := WorkerName(manager.spec.ApplicationID, manager.spec.Environment, manager.spec.SupervisorID, "abcd1234")

	tests := []struct {
		name       string
		reason     string
		exitCode   int
		wantState  State
		wantReason string
		wantRestar int
	}{
		// Exit 1 alone says "error"; the reason says it was a timeout, which is the
		// distinction exit-code inference cannot make.
		{"timeout", "timed_out", 1, StateCrashed, "timed out", 1},
		// A reason that names a recycle overrides the crash the exit status implied.
		{"max jobs", "max_jobs", 1, StateStopped, "max_jobs", 0},
		{"memory", "memory", 12, StateStopped, "memory limit", 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inst := &instance{rec: Record{
				UUID: "abcd1234", Name: name, Queue: "default",
				State: StateRunning, LastExitCode: tc.exitCode, LastExitReason: "exited",
				StartedAt: manager.now(),
			}}
			manager.slots[0] = inst

			if !manager.NoteStopReason(name, tc.reason) {
				t.Fatalf("NoteStopReason(%q) found no live worker", tc.reason)
			}
			inst.exited = true
			manager.afterExitLocked(inst, manager.now())

			if inst.rec.State != tc.wantState {
				t.Errorf("state = %s, want %s", inst.rec.State, tc.wantState)
			}
			if inst.rec.RestartCount != tc.wantRestar {
				t.Errorf("restart count = %d, want %d", inst.rec.RestartCount, tc.wantRestar)
			}
			if !strings.Contains(inst.rec.LastExitReason, tc.wantReason) {
				t.Errorf("last exit reason = %q, want it to mention %q", inst.rec.LastExitReason, tc.wantReason)
			}
		})
	}
}

func TestManagerIgnoresStopReasonsItCannotAttribute(t *testing.T) {
	manager, err := New(helperSpec(t, "run", 1, 2*time.Second, 20*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if manager.NoteStopReason(WorkerName("production", "production", "default-supervisor", "0000ffff"), "timed_out") {
		t.Error("NoteStopReason accepted a reason for a worker this manager does not run")
	}

	// A worker that already exited has been classified; a late reason must not rewrite
	// a restart decision that was already taken.
	name := WorkerName(manager.spec.ApplicationID, manager.spec.Environment, manager.spec.SupervisorID, "abcd1234")
	manager.slots[0] = &instance{rec: Record{Name: name, State: StateCrashed}, exited: true}
	if manager.NoteStopReason(name, "timed_out") {
		t.Error("NoteStopReason accepted a reason for a worker that already exited")
	}
}

func TestManagerAdoptsExistingWorkerWithoutDuplicating(t *testing.T) {
	spec := helperSpec(t, "run", 1, 2*time.Second, 20*time.Millisecond)

	// Simulate the process left behind by a previous GoHorizon.
	launch := spec.Launch
	launch.Queue = "default"
	const uuid = "aaaaaaaa"
	launch.Name = WorkerName(spec.ApplicationID, spec.Environment, spec.SupervisorID, uuid)
	cmd := launch.Command()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start pre-existing worker: %v", err)
	}
	pid := cmd.Process.Pid
	reaped := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		_ = signalKillGroup(pid)
		<-reaped
	})

	info, err := Probe(pid)
	if err != nil {
		t.Fatalf("probe pre-existing worker: %v", err)
	}
	err = WriteStateFile(spec.StatePath, []Record{{
		ApplicationID: spec.ApplicationID,
		Environment:   spec.Environment,
		SupervisorID:  spec.SupervisorID,
		UUID:          uuid,
		Slot:          0,
		Queue:         "default",
		Name:          launch.Name,
		PID:           pid,
		StartTicks:    info.StartTicks,
		State:         StateRunning,
		StartedAt:     time.Now(),
	}})
	if err != nil {
		t.Fatalf("write state file: %v", err)
	}

	manager, cancel, done := startManager(t, spec)

	waitFor(t, 5*time.Second, func() bool {
		stats := manager.Snapshot()
		return len(stats.Workers) == 1 && stats.Workers[0].Adopted
	}, "adoption of the existing worker")

	stats := manager.Snapshot()
	if stats.Workers[0].PID != pid {
		t.Fatalf("adopted pid = %d, want the existing process %d", stats.Workers[0].PID, pid)
	}
	if stats.Adopted != 1 {
		t.Errorf("adopted count = %d, want 1", stats.Adopted)
	}

	// Exactly one process may carry our worker name: no duplicate was spawned.
	if count := countWorkersWithName(t, launch.Name); count != 1 {
		t.Fatalf("found %d processes with name %s, want exactly 1", count, launch.Name)
	}

	cancel()
	<-done
	waitFor(t, 3*time.Second, func() bool {
		_, err := Probe(pid)
		return errors.Is(err, ErrProcessGone)
	}, "adopted worker to stop on shutdown")
}

func TestManagerNeverAdoptsUnverifiedPID(t *testing.T) {
	spec := helperSpec(t, "run", 1, 2*time.Second, 20*time.Millisecond)

	// A reused pid from a stale journal: the pid exists (it is this test process) but
	// the kernel start time does not match, so it must not be adopted or signalled.
	name := WorkerName(spec.ApplicationID, spec.Environment, spec.SupervisorID, "bbbbbbbb")
	err := WriteStateFile(spec.StatePath, []Record{{
		ApplicationID: spec.ApplicationID,
		Environment:   spec.Environment,
		SupervisorID:  spec.SupervisorID,
		UUID:          "bbbbbbbb",
		Queue:         "default",
		Name:          name,
		PID:           os.Getpid(),
		StartTicks:    1,
		State:         StateRunning,
	}})
	if err != nil {
		t.Fatal(err)
	}

	manager, _, _ := startManager(t, spec)
	waitFor(t, 5*time.Second, func() bool {
		stats := manager.Snapshot()
		return stats.Running == 1 && len(stats.Workers) == 1
	}, "a freshly spawned worker")

	stats := manager.Snapshot()
	if stats.Adopted != 0 {
		t.Errorf("adopted = %d, want 0: the journal entry was not verifiable", stats.Adopted)
	}
	if stats.Workers[0].PID == os.Getpid() {
		t.Fatalf("the test process was adopted as a worker")
	}
}

func TestManagerStopWorkerLeavesOthersRunning(t *testing.T) {
	spec := helperSpec(t, "run", 2, 2*time.Second, 50*time.Millisecond)
	manager, _, _ := startManager(t, spec)

	waitFor(t, 5*time.Second, func() bool { return manager.Snapshot().Running == 2 }, "two workers")
	workers := manager.Snapshot().Workers
	victim := workers[0]
	survivor := workers[1]

	if err := manager.StopWorker(victim.UUID); err != nil {
		t.Fatalf("StopWorker: %v", err)
	}

	// Restarting the stopped slot is expected; the survivor keeps its pid and is
	// never signalled, so it must still be running with the same pid afterwards.
	waitFor(t, 5*time.Second, func() bool {
		stats := manager.Snapshot()
		for _, w := range stats.Workers {
			if w.UUID == survivor.UUID && w.PID == survivor.PID && w.State == StateRunning {
				return stats.Running == 2
			}
		}
		return false
	}, "survivor untouched while the stopped slot is replaced")
}

func countWorkersWithName(t *testing.T, name string) int {
	t.Helper()
	procs, err := Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	count := 0
	for _, info := range procs {
		if info.Flag("name") == name {
			count++
		}
	}
	return count
}
