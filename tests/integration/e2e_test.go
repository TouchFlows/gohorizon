//go:build linux

// Package integration_test proves Milestone 1's acceptance criteria against a real
// Laravel 13 application, a real Redis and real queue workers: GoHorizon starts a
// worker, the worker runs jobs, telemetry reaches the API, a crash is recovered, and a
// supervisor restart adopts the running worker instead of spawning a duplicate.
//
// Run it with GOHORIZON_E2E=1 after installing the fixture (see README). Without the
// variable every test skips, so `go test ./...` stays fast and hermetic.
package integration_test

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func awaitRunning(h *harness, base string) report {
	h.t.Helper()

	var latest report
	eventually(h.t, 40*time.Second, "one running worker reported as healthy", func() error {
		current, err := h.fetchReport(base)
		if err != nil {
			return err
		}
		latest = current
		if current.Status != "ok" {
			return fmt.Errorf("status is %q", current.Status)
		}
		sup := current.supervisor(h.t)
		if sup.Running != 1 {
			return fmt.Errorf("running=%d, desired=%d", sup.Running, sup.Desired)
		}
		if len(sup.Workers) != 1 || sup.Workers[0].State != "running" {
			return fmt.Errorf("workers=%+v", sup.Workers)
		}
		return nil
	})
	return latest
}

func requireJobRun(t *testing.T, h *harness, marker string) {
	t.Helper()
	if err := h.dispatch("marker", marker); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "the worker to execute the dispatched job", func() error {
		if _, err := os.Stat(marker); err != nil {
			return err
		}
		return nil
	})
}

func TestRealWorkerRunsJobsAndReportsThemOnTheAPI(t *testing.T) {
	h := newHarness(t)
	cfg, _ := h.writeConfig("127.0.0.1:0")
	base, _ := h.startService(cfg)
	awaitRunning(h, base)

	marker := filepath.Join(h.dir, "marker.txt")
	requireJobRun(t, h, marker)

	// The consumer's counters prove the worker's events crossed the Redis Stream; the
	// metrics endpoint proves they were persisted.
	eventually(t, 30*time.Second, "the job event in /health", func() error {
		current, err := h.fetchReport(base)
		if err != nil {
			return err
		}
		if current.Telemetry.Events < 1 {
			return fmt.Errorf("events=%d invalid=%d errors=%d", current.Telemetry.Events,
				current.Telemetry.Invalid, current.Telemetry.Errors)
		}
		if current.Telemetry.Invalid != 0 || current.Telemetry.Errors != 0 {
			return fmt.Errorf("telemetry degraded: invalid=%d errors=%d", current.Telemetry.Invalid, current.Telemetry.Errors)
		}
		return nil
	})

	eventually(t, 30*time.Second, "the processed job in /metrics", func() error {
		saved, err := h.fetchHistory(base)
		if err != nil {
			return err
		}
		if saved.total(true) < 1 {
			return fmt.Errorf("processed=%d failed=%d", saved.total(true), saved.total(false))
		}
		return nil
	})

	if err := h.dispatch("fail", ""); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "the failed job in /metrics", func() error {
		saved, err := h.fetchHistory(base)
		if err != nil {
			return err
		}
		if saved.total(false) < 1 {
			return fmt.Errorf("failed=%d", saved.total(false))
		}
		return nil
	})
}

func TestCrashedWorkerIsRestartedAndStillRunsJobs(t *testing.T) {
	h := newHarness(t)
	cfg, _ := h.writeConfig("127.0.0.1:0")
	base, _ := h.startService(cfg)

	before := awaitRunning(h, base)
	crashedPID := before.supervisor(t).Workers[0].PID
	if crashedPID <= 0 {
		t.Fatalf("worker pid = %d", crashedPID)
	}
	if err := syscall.Kill(crashedPID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill worker %d: %v", crashedPID, err)
	}

	eventually(t, 45*time.Second, "the supervisor to replace the crashed worker", func() error {
		current, err := h.fetchReport(base)
		if err != nil {
			return err
		}
		sup := current.supervisor(t)
		if sup.Running != 1 {
			return fmt.Errorf("running=%d crashed=%d", sup.Running, sup.Crashed)
		}
		worker := sup.Workers[0]
		if worker.PID == crashedPID {
			return fmt.Errorf("pid %d was never replaced", crashedPID)
		}
		if worker.Restarts < 1 {
			return fmt.Errorf("restarts=%d, want the crash recorded", worker.Restarts)
		}
		if worker.State != "running" {
			return fmt.Errorf("replacement state is %q", worker.State)
		}
		return nil
	})

	// A restarted process is only useful if it can still be handed work.
	requireJobRun(t, h, filepath.Join(h.dir, "marker-after-crash.txt"))
}

func TestServiceRestartAdoptsTheWorkerWithoutDuplicatingIt(t *testing.T) {
	h := newHarness(t)
	binary := h.buildBinary()

	port := freePort(t)
	listen := fmt.Sprintf("127.0.0.1:%d", port)
	_, configPath := h.writeConfig(listen)
	base := fmt.Sprintf("http://%s", listen)

	first := h.startBinary(binary, configPath)
	before := awaitRunning(h, base)
	adoptedPID := before.supervisor(t).Workers[0].PID

	// The adoption journal must exist for the next supervisor to trust the pid.
	if _, err := os.Stat(filepath.Join(h.dir, "workers-e2e.json")); err != nil {
		t.Fatalf("worker journal: %v", err)
	}

	// Kill the supervisor itself, not its worker: SIGKILL never reaches the child's
	// process group, so the Laravel worker is orphaned but healthy.
	first.kill(t)
	if err := syscall.Kill(adoptedPID, 0); err != nil {
		t.Fatalf("worker %d did not survive the supervisor's death: %v", adoptedPID, err)
	}

	// The dead instance cannot renew the ownership lock; drop the key rather than wait
	// out its 30-second TTL.
	ctx, cancel := testContext()
	defer cancel()
	if err := h.redis.Del(ctx, "gohorizon:"+testApplication+":"+testEnvironment+":coord:lock:supervisor").Err(); err != nil {
		t.Fatalf("release the stale lock: %v", err)
	}

	second := h.startBinary(binary, configPath)
	eventually(t, 45*time.Second, "the new supervisor to adopt the running worker", func() error {
		current, err := h.fetchReport(base)
		if err != nil {
			return err
		}
		sup := current.supervisor(t)
		if sup.Adopted != 1 || sup.Running != 1 {
			return fmt.Errorf("adopted=%d running=%d desired=%d", sup.Adopted, sup.Running, sup.Desired)
		}
		if len(sup.Workers) != 1 {
			return fmt.Errorf("workers=%d, want exactly the one adopted process", len(sup.Workers))
		}
		worker := sup.Workers[0]
		if worker.PID != adoptedPID {
			return fmt.Errorf("worker pid=%d, want the adopted %d", worker.PID, adoptedPID)
		}
		if !worker.Adopted {
			return fmt.Errorf("worker %d is not marked adopted", worker.PID)
		}
		return nil
	})

	// Adoption is only real if the adopted process still executes jobs and publishes
	// telemetry under the new supervisor.
	requireJobRun(t, h, filepath.Join(h.dir, "marker-after-adoption.txt"))
	eventually(t, 30*time.Second, "telemetry from the adopted worker", func() error {
		saved, err := h.fetchHistory(base)
		if err != nil {
			return err
		}
		if saved.total(true) < 1 {
			return fmt.Errorf("processed=%d", saved.total(true))
		}
		return nil
	})

	second.terminate(t)
	eventually(t, 30*time.Second, "the adopted worker to stop with its supervisor", func() error {
		if err := syscall.Kill(adoptedPID, 0); err == nil {
			return fmt.Errorf("worker %d is still running", adoptedPID)
		}
		return nil
	})
}
