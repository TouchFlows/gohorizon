package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gohorizon/gohorizon/internal/config"
	"github.com/gohorizon/gohorizon/internal/metrics"
	"github.com/gohorizon/gohorizon/internal/process"
	"github.com/gohorizon/gohorizon/internal/redis"
	"github.com/gohorizon/gohorizon/internal/telemetry"
	goredis "github.com/redis/go-redis/v9"
)

// TestCapabilitiesHelperProcess stands in for the application's `php artisan`. It
// answers the capabilities probe, and when it is asked for queue:work it records the
// spawn attempt (so a test can prove the preflight refused before any worker existed)
// or, when a worker exit trigger is configured, acts as the end-to-end test's worker.
func TestCapabilitiesHelperProcess(t *testing.T) {
	payload, ok := os.LookupEnv("GOHORIZON_TEST_CAPABILITIES")
	if !ok {
		t.Skip("helper process only; launched by the supervisor tests")
	}
	for _, arg := range os.Args[1:] {
		if arg != "queue:work" {
			continue
		}
		if path := os.Getenv("GOHORIZON_TEST_SPAWN_FILE"); path != "" {
			_ = os.WriteFile(path, []byte(strings.Join(os.Args, " ")), 0o600)
		}
		// The worker stays up until the test triggers its exit, so the stop reason can
		// arrive while this worker still owns the slot.
		if trigger := os.Getenv("GOHORIZON_TEST_WORKER_EXIT"); trigger != "" {
			deadline := time.Now().Add(30 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := os.Stat(trigger); err == nil {
					os.Exit(1) // the status Laravel uses for both a timeout and an error
				}
				time.Sleep(5 * time.Millisecond)
			}
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Stdout.WriteString(payload)
	os.Exit(0)
}

func preflightConfig(t *testing.T, timeout int) *config.Config {
	t.Helper()
	return &config.Config{
		Version: 1,
		Server:  config.Server{Listen: "127.0.0.1:8090"},
		Storage: config.Storage{Driver: "sqlite", Path: filepath.Join(t.TempDir(), "gohorizon.db")},
		// An unreachable Redis keeps a passing preflight from opening a real
		// connection: New fails at the ping, which is the signal that the preflight
		// let it through.
		Redis: config.Redis{Address: "127.0.0.1:1", PasswordEnv: "GOHORIZON_TEST_REDIS_PASSWORD"},
		Applications: []config.Application{{
			ID:              "production",
			Name:            "Test Application",
			Path:            t.TempDir(),
			PHP:             os.Args[0],
			Artisan:         "-test.run=TestCapabilitiesHelperProcess",
			Environment:     "production",
			RedisConnection: "redis",
			Supervisors: []config.Supervisor{{
				ID:             "default-supervisor",
				Queues:         []string{"default"},
				Workers:        config.Workers{Min: 1, Max: 1},
				Worker:         config.Worker{Sleep: 3, Tries: 1, Timeout: timeout, Memory: "128"},
				ShutdownGrace:  120,
				RestartBackoff: 5,
			}},
		}},
	}
}

func capabilitiesPayload(retryAfter string) string {
	return `{"schema":1,"package":"gohorizon/laravel","queue":{"default_connection":"redis",` +
		`"connections":{"redis":{"driver":"redis","retry_after":` + retryAfter + `}}},` +
		`"runtime":{"pcntl":true,"posix":true}}`
}

func TestNewRefusesToSpawnWithAnUnsafeTimeout(t *testing.T) {
	spawned := filepath.Join(t.TempDir(), "spawned")
	t.Setenv("GOHORIZON_TEST_CAPABILITIES", capabilitiesPayload("60"))
	t.Setenv("GOHORIZON_TEST_SPAWN_FILE", spawned)

	// timeout: 60 against retry_after: 60 — the reservation would expire while the
	// job is still running.
	_, err := New(context.Background(), preflightConfig(t, 60), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("New accepted worker.timeout == retry_after")
	}
	if !strings.Contains(err.Error(), "retry_after") || !strings.Contains(err.Error(), "refusing to start") {
		t.Errorf("error = %q, want a startup refusal naming retry_after", err)
	}
	if _, statErr := os.Stat(spawned); statErr == nil {
		t.Fatal("a queue:work process was spawned despite the refused timeout configuration")
	}
}

func TestNewPassesPreflightForACompatibleLaunch(t *testing.T) {
	t.Setenv("GOHORIZON_TEST_CAPABILITIES", capabilitiesPayload("90"))

	_, err := New(context.Background(), preflightConfig(t, 60), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("New succeeded without Redis; the test expects the redis ping to fail")
	}
	// It must get past the preflight and fail on Redis, not be refused as incompatible.
	if strings.Contains(err.Error(), "refusing to start") {
		t.Fatalf("compatible capabilities were refused: %v", err)
	}
	if !strings.Contains(err.Error(), "connect to redis") {
		t.Fatalf("error = %q, want the redis connection failure", err)
	}
}

// failingStream fails every read, so a running consumer reports a live failure.
type failingStream struct{}

func (failingStream) EnsureEventsGroup(context.Context, string) error { return nil }

func (failingStream) ReadEventsGroup(context.Context, string, string, string, int64, time.Duration) ([]redis.StreamMessage, error) {
	return nil, errors.New("redis is down")
}

func (failingStream) AckEvents(context.Context, string, ...string) error { return nil }

func TestReportSurfacesFailingTelemetryIngestion(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	consumer, err := telemetry.New(failingStream{}, telemetry.Spec{
		Application: "production",
		Environment: "production",
		MinBackoff:  time.Millisecond,
		MaxBackoff:  5 * time.Millisecond,
		Logger:      logger,
	})
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		consumer.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !consumer.Stats().Failing {
		time.Sleep(5 * time.Millisecond)
	}

	service := &Service{
		cfg:      preflightConfig(t, 60),
		log:      logger,
		consumer: consumer,
		started:  time.Now(),
	}
	// Isolate the telemetry signal from the ownership-lock one.
	service.lockHeld = true

	report := service.Report()
	if !report.Telemetry.Failing || report.Telemetry.Errors == 0 || report.Telemetry.LastError == "" {
		t.Fatalf("telemetry = %+v, want the ingestion failure visible", report.Telemetry)
	}
	// Admitting the failure in the body is enough: a failing status here would let a
	// health check restart or kill a supervisor whose workers are healthy.
	if report.Status != "ok" {
		t.Errorf("status = %q, want ok while only ingestion is failing", report.Status)
	}

	// Lock loss is the one condition that still degrades the report.
	service.lockHeld = false
	if status := service.Report().Status; status != "degraded" {
		t.Errorf("status = %q, want degraded once the lock is lost", status)
	}
}

func waitUntil(t *testing.T, timeout time.Duration, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, description)
}

// TestServiceConsumesTelemetryEndToEnd runs the real service: a worker is spawned, a
// worker.stopped entry classifies its exit by the reported reason, and a job.finished
// entry is persisted into the metrics history the service opened.
func TestServiceConsumesTelemetryEndToEnd(t *testing.T) {
	const address = "127.0.0.1:6379"

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	raw := goredis.NewClient(&goredis.Options{Addr: address})
	defer raw.Close()
	if err := raw.Ping(ctx).Err(); err != nil {
		t.Skipf("redis at %s is unavailable: %v", address, err)
	}

	exit := filepath.Join(t.TempDir(), "exit")
	t.Setenv("GOHORIZON_TEST_WORKER_EXIT", exit)
	t.Setenv("GOHORIZON_TEST_CAPABILITIES", capabilitiesPayload("90"))

	cfg := preflightConfig(t, 60)
	// A namespace of its own keeps the stream, consumer group and lock isolated.
	cfg.Applications[0].ID = "e2e" + strconv.FormatInt(time.Now().UnixNano(), 36)
	cfg.Redis.Address = address
	// The replacement must stay away long enough for the classification to be readable.
	cfg.Applications[0].Supervisors[0].RestartBackoff = 30

	service, err := New(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	stream := service.client.Keys().Events()

	runCtx, stop := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(runCtx) }()
	t.Cleanup(func() {
		stop()
		select {
		case <-runDone:
		case <-time.After(15 * time.Second):
			t.Error("the service did not stop within 15s")
		}
		_ = service.Close()
		cleanupCtx, cleanup := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanup()
		_ = raw.Del(cleanupCtx, stream, service.client.Keys().SupervisorLock()).Err()
	})

	manager := service.managers[0]
	waitUntil(t, 10*time.Second, func() bool {
		return manager.Snapshot().Running == 1
	}, "the worker to start")
	worker := manager.Snapshot().Workers[0]

	payload := fmt.Sprintf(
		`{"schema":1,"id":"%s","type":"worker.stopped","application":%q,"environment":"production",`+
			`"worker":%q,"at":%q,"worker_event":{"exit_code":1,"reason":"max_jobs"}}`,
		"e2e-"+strconv.FormatInt(time.Now().UnixNano(), 36),
		cfg.Applications[0].ID, worker.Name, time.Now().UTC().Format(time.RFC3339Nano),
	)
	entryID, err := raw.XAdd(ctx, &goredis.XAddArgs{Stream: stream, Values: map[string]any{"payload": payload}}).Result()
	if err != nil {
		t.Fatalf("publish worker.stopped: %v", err)
	}

	// The consumer acknowledges only after it handled the entry, so an acked entry means
	// the reason reached the manager before this worker exits.
	waitUntil(t, 10*time.Second, func() bool {
		groups, err := raw.XInfoGroups(ctx, stream).Result()
		if err != nil {
			return false
		}
		for _, group := range groups {
			if group.Name == telemetry.DefaultGroup && group.LastDeliveredID == entryID && group.Pending == 0 {
				return true
			}
		}
		return false
	}, "the consumer to acknowledge the stop reason")

	if err := os.WriteFile(exit, []byte("exit"), 0o600); err != nil {
		t.Fatalf("trigger the worker exit: %v", err)
	}

	waitUntil(t, 10*time.Second, func() bool {
		workers := manager.Snapshot().Workers
		return len(workers) == 1 && workers[0].State == process.StateStopped
	}, "the manager to classify the exit")

	classified := manager.Snapshot().Workers[0]
	if classified.LastExitCode != 1 {
		t.Fatalf("last exit code = %d, want 1: without the reason this status means a crash", classified.LastExitCode)
	}
	if classified.RestartCount != 0 {
		t.Errorf("restart count = %d, want 0: max_jobs is a recycle, not a crash", classified.RestartCount)
	}
	if !strings.Contains(classified.LastExitReason, "max_jobs") {
		t.Errorf("last exit reason = %q, want the reason Laravel reported", classified.LastExitReason)
	}

	// The same consumer persists job metrics through the store New opened, so the
	// history exists without the dashboard (docs/adr/0007).
	jobPayload := fmt.Sprintf(
		`{"schema":1,"id":"%s","type":"job.finished","application":%q,"environment":"production",`+
			`"worker":%q,"at":%q,"job":{"queue":"emails","outcome":"processed","duration_ms":1500}}`,
		"e2e-job-"+strconv.FormatInt(time.Now().UnixNano(), 36),
		cfg.Applications[0].ID, worker.Name, time.Now().UTC().Format(time.RFC3339Nano),
	)
	if _, err := raw.XAdd(ctx, &goredis.XAddArgs{Stream: stream, Values: map[string]any{"payload": jobPayload}}).Result(); err != nil {
		t.Fatalf("publish job.finished: %v", err)
	}

	waitUntil(t, 10*time.Second, func() bool {
		buckets, err := service.store.Buckets(context.Background(), "emails",
			time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
		return err == nil && len(buckets) == 1 && buckets[0].Processed == 1
	}, "the job metric to be persisted")

	buckets, err := service.store.Buckets(ctx, "emails", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("read metrics history: %v", err)
	}
	if buckets[0].DurationMS != 1500 {
		t.Errorf("duration = %d, want the reported 1500ms", buckets[0].DurationMS)
	}
}

// TestServicePrunesMetricsOnStartupAndWhileRunning seeds the database a previous run
// would have left behind, then proves the running service removes what is past the
// retention windows on startup and again on its schedule.
func TestServicePrunesMetricsOnStartupAndWhileRunning(t *testing.T) {
	const address = "127.0.0.1:6379"

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	raw := goredis.NewClient(&goredis.Options{Addr: address})
	defer raw.Close()
	if err := raw.Ping(ctx).Err(); err != nil {
		t.Skipf("redis at %s is unavailable: %v", address, err)
	}

	// The worker only has to stay up; this test is about the database.
	t.Setenv("GOHORIZON_TEST_WORKER_EXIT", filepath.Join(t.TempDir(), "never"))
	t.Setenv("GOHORIZON_TEST_CAPABILITIES", capabilitiesPayload("90"))

	cfg := preflightConfig(t, 60)
	cfg.Redis.Address = address
	cfg.Applications[0].ID = "prune" + strconv.FormatInt(time.Now().UnixNano(), 36)
	old := time.Now().Add(-8 * 24 * time.Hour)

	seed, err := metrics.Open(cfg.Storage.Path)
	if err != nil {
		t.Fatalf("metrics.Open: %v", err)
	}
	if err := seed.RecordJob(ctx, "old-event", "emails", "processed", 10, old); err != nil {
		t.Fatalf("seed old metric: %v", err)
	}
	if err := seed.RecordJob(ctx, "fresh-event", "emails", "processed", 10, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("seed fresh metric: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	service, err := New(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if service.store == nil {
		t.Fatal("metrics history was not opened, so pruning cannot run")
	}
	service.pruneInterval = 50 * time.Millisecond
	stream := service.client.Keys().Events()

	runCtx, stop := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(runCtx) }()
	t.Cleanup(func() {
		stop()
		select {
		case <-runDone:
		case <-time.After(15 * time.Second):
			t.Error("the service did not stop within 15s")
		}
		if err := service.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		cleanupCtx, cleanup := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanup()
		_ = raw.Del(cleanupCtx, stream, service.client.Keys().SupervisorLock()).Err()
	})

	from := old.Add(-time.Hour)
	waitUntil(t, 10*time.Second, func() bool {
		buckets, err := service.store.Buckets(context.Background(), "emails", from, time.Now())
		return err == nil && len(buckets) == 1
	}, "the startup prune to remove the 8-day-old bucket")

	buckets, err := service.store.Buckets(ctx, "emails", from, time.Now())
	if err != nil {
		t.Fatalf("read metrics history: %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("buckets = %+v, want only the bucket inside the retention window", buckets)
	}
	if buckets[0].Start.Equal(old.Truncate(time.Minute)) {
		t.Error("the 8-day-old bucket survived the startup prune")
	}
	if buckets[0].Processed != 1 || buckets[0].DurationMS != 10 {
		t.Errorf("bucket = %+v, want the in-window job kept", buckets[0])
	}

	// The startup pass is done, so a bucket written now can only disappear through the
	// scheduled pass.
	loopOld := time.Now().Add(-9 * 24 * time.Hour)
	if err := service.store.RecordJob(ctx, "loop-old", "emails", "processed", 10, loopOld); err != nil {
		t.Fatalf("seed metric for the scheduled pass: %v", err)
	}
	waitUntil(t, 5*time.Second, func() bool {
		buckets, err := service.store.Buckets(context.Background(), "emails", loopOld.Add(-time.Hour), loopOld.Add(time.Hour))
		return err == nil && len(buckets) == 0
	}, "the scheduled prune to remove a bucket written after startup")

	buckets, err = service.store.Buckets(ctx, "emails", from, time.Now())
	if err != nil {
		t.Fatalf("read metrics history: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Processed != 1 {
		t.Errorf("buckets = %+v, want the in-window job untouched by the schedule", buckets)
	}
}

func TestNewDegradesWhenCapabilitiesAreUnavailable(t *testing.T) {
	// No GOHORIZON_TEST_CAPABILITIES: the helper refuses, standing in for an
	// application without the package. Supervision must not be blocked by it.
	_, err := New(context.Background(), preflightConfig(t, 60), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("New succeeded without Redis; the test expects the redis ping to fail")
	}
	if !strings.Contains(err.Error(), "connect to redis") {
		t.Fatalf("a missing adapter blocked startup: %v", err)
	}
}
