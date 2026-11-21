package supervisor

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gohorizon/gohorizon/internal/process"
	"github.com/gohorizon/gohorizon/internal/telemetry"
)

// TestCrashLoopingWorkersLeaveTheHealthStatusOK pins the status rule at the layer that
// decides it: Report degrades on the ownership lock alone, so a worker the pool keeps
// replacing stays body-only and cannot make a health check restart a supervisor whose
// other workers are fine (docs/adr/0004 item 7). The router test asserts the field is
// encoded; this asserts the status behind it.
func TestCrashLoopingWorkersLeaveTheHealthStatusOK(t *testing.T) {
	trigger := filepath.Join(t.TempDir(), "exit")
	if err := os.WriteFile(trigger, nil, 0o600); err != nil {
		t.Fatalf("create the exit trigger: %v", err)
	}
	// The helper exits 1 on every queue:work start, which is a crash, not a recycle.
	t.Setenv("GOHORIZON_TEST_CAPABILITIES", capabilitiesPayload("90"))
	t.Setenv("GOHORIZON_TEST_WORKER_EXIT", trigger)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager, err := process.New(process.Spec{
		ApplicationID: "production",
		Environment:   "production",
		SupervisorID:  "default-supervisor",
		Launch: process.LaunchSpec{
			PHP:        os.Args[0],
			Artisan:    "-test.run=TestCapabilitiesHelperProcess",
			Dir:        t.TempDir(),
			Connection: "redis",
			Timeout:    60,
			Memory:     "128",
		},
		Queues:         []string{"default"},
		Desired:        1,
		ShutdownGrace:  5 * time.Second,
		RestartBackoff: 5 * time.Millisecond,
		Logger:         logger,
	})
	if err != nil {
		t.Fatalf("process.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = manager.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	deadline := time.Now().Add(10 * time.Second)
	for {
		workers := manager.Snapshot().Workers
		if len(workers) == 1 && workers[0].CrashLooping {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker never reached a crash loop: %+v", workers)
		}
		time.Sleep(10 * time.Millisecond)
	}

	service := &Service{
		cfg:      preflightConfig(t, 60),
		log:      logger,
		consumer: &telemetry.Consumer{},
		managers: []*process.Manager{manager},
		started:  time.Now(),
	}
	// The lock is the only condition that may degrade the status.
	service.lockHeld = true

	report := service.Report()
	if report.Status != "ok" {
		t.Errorf("status = %q, want ok while only a worker is crash looping", report.Status)
	}
	if len(report.Supervisors) != 1 || len(report.Supervisors[0].Workers) != 1 {
		t.Fatalf("report = %+v, want one supervisor with one worker", report.Supervisors)
	}
	if !report.Supervisors[0].Workers[0].CrashLooping {
		t.Error("the crashing worker is not reported as crash looping")
	}
}
