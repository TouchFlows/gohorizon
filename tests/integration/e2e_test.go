//go:build linux

// Package integration_test proves Milestone 1's acceptance criteria against a real
// Laravel 13 application, a real Redis and real queue workers: GoHorizon starts a
// worker, the worker runs jobs, telemetry reaches the API, a job in flight survives a
// graceful stop and is retried after a hard kill, a crash is recovered, and a supervisor
// restart adopts the running worker instead of spawning a duplicate.
//
// Run it with GOHORIZON_E2E=1 after installing the fixture (see README). Without the
// variable every test skips, so `go test ./...` stays fast and hermetic.
package integration_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	configPath, base := h.writeConfig()
	h.startBinary(h.buildBinary(), configPath)
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
		if processed, failed := saved.totals(); processed < 1 {
			return fmt.Errorf("processed=%d failed=%d", processed, failed)
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
		if _, failed := saved.totals(); failed < 1 {
			return fmt.Errorf("failed=%d", failed)
		}
		return nil
	})
}

// TestGracefulStopFinishesTheInFlightJobExactlyOnce proves the promise a graceful stop
// makes: a job that is already running is not abandoned, it is not run again, and it is
// accounted for by the process that ran it. History is read from the database the
// service wrote, because the service is gone by the time the job's event is due.
func TestGracefulStopFinishesTheInFlightJobExactlyOnce(t *testing.T) {
	h := newHarness(t)
	configPath, base := h.writeConfig()
	instance := h.startBinary(h.buildBinary(), configPath)
	awaitRunning(h, base)

	marker := filepath.Join(h.dir, "slow-stop.txt")
	if err := h.dispatch("slow", marker); err != nil {
		t.Fatal(err)
	}
	awaitStarted(t, marker)

	sent := time.Now()
	instance.terminate(t)

	info, err := os.Stat(marker)
	if err != nil {
		t.Fatalf("stat the slow job's marker: %v", err)
	}
	if info.ModTime().Before(sent) {
		t.Fatal("the marker was last written before the SIGTERM, so the job had already finished")
	}
	requireRuns(t, marker, 1, 1)
	awaitQueueDepth(t, h, 0, 0, "the finished job to leave nothing behind")

	// An event left unread until the next start would make the record of a completed job
	// depend on restarting the service.
	processed, failed, err := h.persistedHistory(testQueue)
	if err != nil {
		t.Fatalf("read the persisted history: %v", err)
	}
	if processed != 1 || failed != 0 {
		t.Fatalf("processed=%d failed=%d, want the finished job recorded exactly once", processed, failed)
	}
}

// TestKilledWorkerLeavesTheJobForLaravelToRetry proves a hard kill cannot lose a job.
// The reservation Laravel holds is the only record of the in-flight job: the test shows
// it survives the kill, expires, and is executed once by the replacement worker.
func TestKilledWorkerLeavesTheJobForLaravelToRetry(t *testing.T) {
	h := newHarness(t)
	configPath, base := h.writeConfig()
	h.startBinary(h.buildBinary(), configPath)

	before := awaitRunning(h, base)
	workerPID := before.supervisor(t).Workers[0].PID
	if workerPID <= 0 {
		t.Fatalf("worker pid = %d", workerPID)
	}

	marker := filepath.Join(h.dir, "slow-kill.txt")
	if err := h.dispatch("slow", marker); err != nil {
		t.Fatal(err)
	}
	awaitStarted(t, marker)

	if err := syscall.Kill(workerPID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill worker %d: %v", workerPID, err)
	}

	// Nothing deletes a reservation when its worker dies; that entry is the only reason
	// the job still exists after the kill.
	awaitQueueDepth(t, h, 0, 1, "the killed worker's job to stay reserved")

	eventually(t, 60*time.Second, "the replacement worker to run the retried job", func() error {
		lines, err := markerLines(marker)
		if err != nil {
			return err
		}
		if lines["ran"] != 1 {
			return fmt.Errorf("ran=%d, want the job completed once", lines["ran"])
		}
		return nil
	})

	// The first attempt died before finishing, so the replay is the only completed run.
	requireRuns(t, marker, 2, 1)
	awaitQueueDepth(t, h, 0, 0, "the retried job to leave nothing behind")
	eventually(t, 30*time.Second, "the retried job accounted for once", func() error {
		saved, err := h.fetchHistory(base)
		if err != nil {
			return err
		}
		if processed, failed := saved.totals(); processed != 1 || failed != 0 {
			return fmt.Errorf("processed=%d failed=%d, want exactly one processed job", processed, failed)
		}
		return nil
	})
}

func TestCrashedWorkerIsRestartedAndStillRunsJobs(t *testing.T) {
	h := newHarness(t)
	configPath, base := h.writeConfig()
	h.startBinary(h.buildBinary(), configPath)

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
	configPath, base := h.writeConfig()

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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.redis.Del(ctx, h.supervisorLockKey()).Err(); err != nil {
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
		if processed, _ := saved.totals(); processed < 1 {
			return fmt.Errorf("processed=%d", processed)
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

// TestLockLossMakesTheLoserReleaseItsWorker proves the single-owner invariant from the
// losing side. Another instance takes the ownership lock while this one supervises; the
// loser notices at its next renewal and stops its worker instead of leaving two services
// owning the same pool. The stop is graceful, so the job in flight still finishes and is
// accounted for, and it never deletes the new owner's lock on the way out.
func TestLockLossMakesTheLoserReleaseItsWorker(t *testing.T) {
	h := newHarness(t)
	configPath, base := h.writeConfig()
	loser := h.startBinary(h.buildBinary(), configPath)
	awaitRunning(h, base)

	marker := filepath.Join(h.dir, "fenced.txt")
	if err := h.dispatch("slow", marker); err != nil {
		t.Fatal(err)
	}
	awaitStarted(t, marker)

	// What a second instance does when the first one stalled: take the key with its own
	// token. The impostor's TTL outlives the whole test, so the loser can only leave it
	// alone by comparing the owner.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lockKey := h.supervisorLockKey()
	if err := h.redis.Set(ctx, lockKey, "another-instance", 30*time.Second).Err(); err != nil {
		t.Fatalf("take over the supervisor lock: %v", err)
	}

	if err := loser.wait(45 * time.Second); err == nil {
		t.Fatal("the instance that lost the lock exited successfully; the loss must be reported")
	}

	data, err := os.ReadFile(loser.logPath)
	if err != nil {
		t.Fatalf("read the loser's log: %v", err)
	}
	if !strings.Contains(string(data), "lock lost") {
		t.Errorf("the loser's log does not report the lock loss:\n%s", tail(data, 4000))
	}

	// The pool is released: no worker of this application is left behind.
	pids, err := h.workerPIDs()
	if err != nil {
		t.Fatalf("scan worker processes: %v", err)
	}
	if len(pids) != 0 {
		t.Fatalf("worker pids %v are still running after the loser exited", pids)
	}

	// Fencing stops through the graceful path, so the job in flight ran and finished once.
	requireRuns(t, marker, 1, 1)
	awaitQueueDepth(t, h, 0, 0, "the finished job to leave nothing behind")
	processed, failed, err := h.persistedHistory(testQueue)
	if err != nil {
		t.Fatalf("read the persisted history: %v", err)
	}
	if processed != 1 || failed != 0 {
		t.Fatalf("processed=%d failed=%d, want the fenced worker's job recorded once", processed, failed)
	}

	// The release compares the owner token, so the loser must not have deleted a lock it
	// no longer owned. The loser outlives the takeover deadline, so read with a fresh one.
	holder, err := h.redis.Get(context.Background(), lockKey).Result()
	if err != nil {
		t.Fatalf("read the lock after the loser exited: %v", err)
	}
	if holder != "another-instance" {
		t.Fatalf("lock holder = %q, want the new owner's token left untouched", holder)
	}
}
