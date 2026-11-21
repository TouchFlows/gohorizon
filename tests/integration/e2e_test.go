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

// awaitRunning waits until the configured pool is at its desired size and healthy, so the
// single-worker tests and the two-queue pool share one definition of ready.
func awaitRunning(h *harness, base string) report {
	h.t.Helper()

	var latest report
	eventually(h.t, 40*time.Second, "the desired pool running and reported as healthy", func() error {
		current, err := h.fetchReport(base)
		if err != nil {
			return err
		}
		latest = current
		if current.Status != "ok" {
			return fmt.Errorf("status is %q", current.Status)
		}
		sup := current.supervisor(h.t)
		if sup.Running != sup.Desired {
			return fmt.Errorf("running=%d, desired=%d", sup.Running, sup.Desired)
		}
		if len(sup.Workers) != sup.Desired {
			return fmt.Errorf("workers=%+v, want %d", sup.Workers, sup.Desired)
		}
		for _, worker := range sup.Workers {
			if worker.State != "running" {
				return fmt.Errorf("worker %s is %s", worker.UUID, worker.State)
			}
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

	// The rows, not just the counters: history must name a real job's class and carry a
	// real failure's exception, which is what proves the decoded identity is stored.
	eventually(t, 30*time.Second, "the real jobs in the jobs table", func() error {
		stored, err := h.storedJobs()
		if err != nil {
			return err
		}
		var markers, failures int
		for _, job := range stored {
			switch {
			case job.Outcome == "processed" && job.Class == `App\Jobs\RecordMarker` && job.Queue == testQueue:
				markers++
			case job.Outcome == "failed" && job.Class == `App\Jobs\AlwaysFails` && job.ExceptionClass == "RuntimeException":
				failures++
			}
		}
		if markers < 1 || failures < 1 {
			return fmt.Errorf("processed RecordMarker rows=%d, failed AlwaysFails rows=%d of %d", markers, failures, len(stored))
		}
		return nil
	})
}

// TestOneWorkerPerQueueRunsItsOwnJobs proves the shape people deploy: one supervisor over
// two queues with one worker each. Allocation is round-robin per slot, so a slip would
// leave one queue with two workers and the other with none — and the per-queue history
// would then attribute both jobs to the same queue.
func TestOneWorkerPerQueueRunsItsOwnJobs(t *testing.T) {
	first, second := testQueue+"-a", testQueue+"-b"

	h := newHarness(t)
	configPath, base := h.writeConfigFor(testRedisAddr, []string{first, second}, 2)
	h.startBinary(h.buildBinary(), configPath)
	pool := awaitRunning(h, base).supervisor(t)

	workers := map[string]int{}
	for _, worker := range pool.Workers {
		workers[worker.Queue]++
	}
	if workers[first] != 1 || workers[second] != 1 {
		t.Fatalf("worker queues = %v, want one worker on each of %s and %s", workers, first, second)
	}

	// Both jobs are dispatched before either is awaited, so the assertions hold while the
	// two workers are running side by side.
	markers := map[string]string{
		first:  filepath.Join(h.dir, "marker-a.txt"),
		second: filepath.Join(h.dir, "marker-b.txt"),
	}
	for queue, marker := range markers {
		if err := h.dispatchTo(queue, "marker", marker); err != nil {
			t.Fatal(err)
		}
	}
	// The marker proves the PHP job itself ran; the history below proves exactly how many
	// jobs each queue accounted for, so a second run could not hide here.
	for queue, marker := range markers {
		eventually(t, 30*time.Second, "the job dispatched to "+queue+" to run", func() error {
			_, err := os.Stat(marker)
			return err
		})
	}

	// Each queue's history holds exactly its own job: a shared bucket would let a job run
	// by the wrong worker hide.
	for queue := range markers {
		eventually(t, 30*time.Second, "the processed job for "+queue+" in /metrics", func() error {
			saved, err := h.fetchHistoryFor(base, queue)
			if err != nil {
				return err
			}
			processed, failed := saved.totals()
			if processed != 1 || failed != 0 {
				return fmt.Errorf("processed=%d failed=%d, want exactly its own job", processed, failed)
			}
			return nil
		})
	}
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

// TestControlPlaneRedisOutageKeepsJobsRunningAndRecovers proves handoff §2.11 against the
// real binary. GoHorizon's own Redis goes away while Laravel keeps its connection, so jobs
// keep running and the worker is never touched; health reports the loss as 503 and returns
// to 200 without a restart once Redis is back, with the same worker pid throughout.
func TestControlPlaneRedisOutageKeepsJobsRunningAndRecovers(t *testing.T) {
	h := newHarness(t)
	proxy := newRedisProxy(t, testRedisAddr)
	configPath, base := h.writeConfigAt(proxy.Addr())
	instance := h.startBinary(h.buildBinary(), configPath)

	before := awaitRunning(h, base)
	pid := before.supervisor(t).Workers[0].PID
	if pid <= 0 {
		t.Fatalf("worker pid = %d", pid)
	}

	// Only the control plane's Redis is cut. The outage stays well under the lock's 30s TTL:
	// losing the lock is a different property, and here this instance must keep it.
	proxy.Pause()

	// A job dispatched while the control plane is dark still runs, because the worker's
	// connection to Redis is its own.
	marker := filepath.Join(h.dir, "outage.txt")
	requireJobRun(t, h, marker)

	eventually(t, 30*time.Second, "health to report the control-plane outage", func() error {
		code, current, err := h.fetchReportCode(base)
		if err != nil {
			return err
		}
		if code != 503 || current.Status != "degraded" {
			return fmt.Errorf("health is %d %q, want 503 degraded", code, current.Status)
		}
		if current.Lock.Held || !current.Lock.Degraded || current.Lock.LastError == "" {
			return fmt.Errorf("lock = %+v, want the renewal failure reported", current.Lock)
		}
		if !current.Telemetry.Failing || current.Telemetry.LastError == "" {
			return fmt.Errorf("telemetry = %+v, want the ingestion failure reported", current.Telemetry)
		}
		sup := current.supervisor(t)
		if sup.Running != 1 || len(sup.Workers) != 1 || sup.Workers[0].PID != pid {
			return fmt.Errorf("running=%d workers=%+v, want pid %d still supervising", sup.Running, sup.Workers, pid)
		}
		return nil
	})

	proxy.Resume()

	eventually(t, 45*time.Second, "health to recover without restarting the worker", func() error {
		code, current, err := h.fetchReportCode(base)
		if err != nil {
			return err
		}
		if code != 200 || current.Status != "ok" {
			return fmt.Errorf("health is %d %q, want 200 ok", code, current.Status)
		}
		if !current.Lock.Held || current.Lock.Degraded {
			return fmt.Errorf("lock = %+v, want ownership confirmed again", current.Lock)
		}
		sup := current.supervisor(t)
		if sup.Running != 1 || len(sup.Workers) != 1 || sup.Workers[0].PID != pid || sup.Workers[0].Restarts != 0 {
			return fmt.Errorf("running=%d workers=%+v, want pid %d never restarted", sup.Running, sup.Workers, pid)
		}
		return nil
	})

	// Ingestion is running again: a job dispatched after recovery is recorded as before, and
	// the job from the outage was not lost either.
	requireJobRun(t, h, filepath.Join(h.dir, "after-outage.txt"))
	eventually(t, 30*time.Second, "telemetry to be consumed again after recovery", func() error {
		saved, err := h.fetchHistory(base)
		if err != nil {
			return err
		}
		if processed, _ := saved.totals(); processed < 2 {
			return fmt.Errorf("processed=%d, want both jobs recorded", processed)
		}
		return nil
	})

	// A normal stop still works after the outage, and the job ran exactly once.
	instance.terminate(t)
	lines, err := markerLines(marker)
	if err != nil {
		t.Fatalf("read %s: %v", marker, err)
	}
	if lines["ran"] != 1 || len(lines) != 1 {
		t.Fatalf("%s: %v, want the job to have run exactly once", marker, lines)
	}
}

// TestSilentRedisFencesTheSupervisor proves the complement of the outage test: silence that
// outlives the lock's TTL is a loss even though Redis never answers. The key expires while
// the proxy is paused, so another instance could take the pool; this one must stop its
// worker instead of supervising a lock it cannot hold.
func TestSilentRedisFencesTheSupervisor(t *testing.T) {
	h := newHarness(t)
	proxy := newRedisProxy(t, testRedisAddr)
	configPath, base := h.writeConfigAt(proxy.Addr())
	loser := h.startBinary(h.buildBinary(), configPath)
	awaitRunning(h, base)

	proxy.Pause()

	// The fence lands at one TTL plus half a TTL, and the renewal tick, the graceful stop and
	// the bounded drain add a little; allow a generous bound instead of a tight one.
	if err := loser.wait(90 * time.Second); err == nil {
		t.Fatal("the instance exited successfully after its lock silently expired; it must report the loss")
	}

	data, err := os.ReadFile(loser.logPath)
	if err != nil {
		t.Fatalf("read the loser's log: %v", err)
	}
	if !strings.Contains(string(data), "not confirmed ownership") {
		t.Errorf("the loser's log does not name the unconfirmed lock:\n%s", tail(data, 4000))
	}

	// The premise: without renewals the key expired while Redis was unreachable, so a second
	// instance could have taken the pool during the silence.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exists, err := h.redis.Exists(ctx, h.supervisorLockKey()).Result()
	if err != nil {
		t.Fatalf("read the supervisor lock: %v", err)
	}
	if exists != 0 {
		t.Fatal("the lock key still exists, so this run did not test fencing against an expired lock")
	}

	// The worker is released exactly as it is for an answered loss.
	pids, err := h.workerPIDs()
	if err != nil {
		t.Fatalf("scan worker processes: %v", err)
	}
	if len(pids) != 0 {
		t.Fatalf("worker pids %v are still running after the silent lock loss", pids)
	}
}

// TestReleasedAndTimedOutJobsReachTheHistory proves the two outcomes no other test produces.
// Both are pinned in tests/fixtures/telemetry-outcomes.json, but only `processed` and
// `failed` have ever come from a real worker, so a mapping slip on either of these would
// have gone unnoticed until the dashboard.
func TestReleasedAndTimedOutJobsReachTheHistory(t *testing.T) {
	h := newHarness(t)
	configPath, base := h.writeConfig()
	h.startBinary(h.buildBinary(), configPath)
	awaitRunning(h, base)

	// The job declares two tries: the first attempt is released for retry, the second
	// exceeds the limit and fails, so one job accounts for both outcomes.
	if err := h.dispatch("release", ""); err != nil {
		t.Fatal(err)
	}
	eventually(t, 45*time.Second, "the released attempt and its final failure", func() error {
		saved, err := h.fetchHistory(base)
		if err != nil {
			return err
		}
		outcomes := saved.outcomes()
		if outcomes.released != 1 || outcomes.failed != 1 {
			return fmt.Errorf("released=%d failed=%d, want one of each", outcomes.released, outcomes.failed)
		}
		return nil
	})

	// The worker's timeout fires while the job is still running, and Laravel reports it
	// before killing the worker for it.
	if err := h.dispatch("hang", ""); err != nil {
		t.Fatal(err)
	}
	eventually(t, 60*time.Second, "the timed-out job", func() error {
		saved, err := h.fetchHistory(base)
		if err != nil {
			return err
		}
		if outcomes := saved.outcomes(); outcomes.timedOut != 1 {
			return fmt.Errorf("timed_out=%d (processed=%d released=%d failed=%d)",
				outcomes.timedOut, outcomes.processed, outcomes.released, outcomes.failed)
		}
		return nil
	})
}
