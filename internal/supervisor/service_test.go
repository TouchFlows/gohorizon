package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gohorizon/gohorizon/internal/adapter"
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

// stubInspector answers the queue-depth probe without an application and counts how
// often it was asked.
type stubInspector struct {
	inspection adapter.Inspection
	err        error
	calls      atomic.Int32
}

func (s *stubInspector) Queues(context.Context, string, []string) (adapter.Inspection, error) {
	s.calls.Add(1)
	if s.err != nil {
		return adapter.Inspection{}, s.err
	}
	return s.inspection, nil
}

func queueTestService(t *testing.T, inspector queueInspector) *Service {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := &Service{
		cfg: preflightConfig(t, 60),
		log: logger,
		// Report reads the consumer; a zero-value consumer has zero counters.
		consumer:  &telemetry.Consumer{},
		inspector: inspector,
		queues:    []string{"default"},
		started:   time.Now(),
	}
	// Isolate the queue signal from the ownership-lock one.
	service.lockHeld = true
	return service
}

func TestHealthReportsQueueDepthFromTheAdapter(t *testing.T) {
	pending, reserved, delayed := 12, 1, 3
	oldest := int64(1763600000)
	oldestErr := "RedisException"
	service := queueTestService(t, &stubInspector{inspection: adapter.Inspection{
		Connection: "redis",
		Queues: []adapter.QueueDepth{
			{Queue: "default", Pending: &pending, Reserved: &reserved, Delayed: &delayed, OldestPendingAt: &oldest},
			{Queue: "stalled", OldestPendingError: &oldestErr},
		},
	}})

	service.sampleQueues(context.Background())

	report := service.Report()
	if report.Status != "ok" {
		t.Errorf("status = %q, want ok", report.Status)
	}
	if report.Queues.Connection != "redis" || report.Queues.Error != "" {
		t.Errorf("queues = %+v, want the adapter's connection and no error", report.Queues)
	}
	if len(report.Queues.Depths) != 2 {
		t.Fatalf("depths = %+v, want both queues", report.Queues.Depths)
	}
	depth := report.Queues.Depths[0]
	if depth.Queue != "default" || depth.Pending == nil || *depth.Pending != 12 ||
		depth.Reserved == nil || *depth.Reserved != 1 ||
		depth.Delayed == nil || *depth.Delayed != 3 {
		t.Errorf("depth = %+v, want the counts the adapter reported", depth)
	}
	if depth.OldestPendingAt == nil || *depth.OldestPendingAt != oldest || depth.OldestPendingError != "" {
		t.Errorf("depth = %+v, want the backlog age the adapter reported", depth)
	}
	// An age that could not be read reaches health as an error, not as a timestamp.
	stalled := report.Queues.Depths[1]
	if stalled.OldestPendingAt != nil || stalled.OldestPendingError != oldestErr {
		t.Errorf("stalled = %+v, want the failed age without a timestamp", stalled)
	}
}

func TestQueueInspectionFailureLeavesTheStatusAlone(t *testing.T) {
	inspector := &stubInspector{err: errors.New("gohorizon:inspect produced no output")}
	service := queueTestService(t, inspector)

	service.sampleQueues(context.Background())

	report := service.Report()
	// The package is missing or the command failed: the body says so, the status does
	// not, so a health check cannot restart supervisors whose workers are fine.
	if report.Status != "ok" {
		t.Errorf("status = %q, want ok while only inspection is failing", report.Status)
	}
	if report.Queues.Error == "" {
		t.Fatal("queues.error is empty, want the inspection failure in the body")
	}
	if len(report.Queues.Depths) != 0 {
		t.Errorf("depths = %+v, want none for a failed inspection", report.Queues.Depths)
	}

	// The next sample recovers without restarting anything.
	inspector.err = nil
	inspector.inspection = adapter.Inspection{
		Connection: "redis",
		Queues:     []adapter.QueueDepth{{Queue: "default"}},
	}
	service.sampleQueues(context.Background())

	report = service.Report()
	if report.Status != "ok" || report.Queues.Error != "" {
		t.Errorf("report = %+v, want the recovered inspection with no error", report.Queues)
	}
	if len(report.Queues.Depths) != 1 || report.Queues.Depths[0].Pending != nil {
		t.Errorf("depths = %+v, want the queue with its unreported counts left nil", report.Queues.Depths)
	}
}

func TestMetricsRetentionMapsTheConfiguredWindows(t *testing.T) {
	cfg := &config.Config{Storage: config.Storage{Retention: config.Retention{
		RawHours: 48, MinuteDays: 3, HourlyDays: 10, DailyDays: 90,
	}}}

	got := metricsRetention(cfg)
	want := metrics.Retention{
		Raw:    48 * time.Hour,
		Minute: 3 * 24 * time.Hour,
		Hourly: 10 * 24 * time.Hour,
		Daily:  90 * 24 * time.Hour,
	}
	if got != want {
		t.Errorf("metricsRetention = %+v, want %+v", got, want)
	}
}

func TestBucketsReadsPersistedHistory(t *testing.T) {
	store, err := metrics.Open(filepath.Join(t.TempDir(), "metrics.db"), metrics.DefaultRetention())
	if err != nil {
		t.Fatalf("open metrics store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	at := time.Now().Add(-2 * time.Minute)
	if err := store.RecordJob(context.Background(), job("event-1", "emails", "processed", 120, at)); err != nil {
		t.Fatalf("seed metric: %v", err)
	}

	service := &Service{store: store}
	buckets, err := service.Buckets(context.Background(), metrics.ResolutionMinute, "emails", at.Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatalf("Buckets: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Processed != 1 || buckets[0].DurationMS != 120 {
		t.Fatalf("buckets = %+v, want the recorded job", buckets)
	}

	// The resolution reaches the store: a closed hour is readable as hourly history.
	closed := at.Add(-time.Hour)
	if err := store.RecordJob(context.Background(), job("event-2", "emails", "failed", 80, closed)); err != nil {
		t.Fatalf("seed closed-hour metric: %v", err)
	}
	if err := store.Rollup(context.Background(), time.Now()); err != nil {
		t.Fatalf("Rollup: %v", err)
	}
	hours, err := service.Buckets(context.Background(), metrics.ResolutionHourly, "emails", closed.Add(-24*time.Hour), time.Now())
	if err != nil {
		t.Fatalf("hourly Buckets: %v", err)
	}
	// Exactly which hours exist depends on the wall clock: a run in the first two minutes
	// of an hour puts the processed job in the previous, now-closed hour as well. The
	// property is that the failed job is readable in its own closed hour.
	closedHour := closed.Truncate(time.Hour)
	var folded *metrics.Bucket
	for i := range hours {
		if hours[i].Start.Equal(closedHour) {
			folded = &hours[i]
		}
	}
	if folded == nil || folded.Failed != 1 || folded.DurationMS != 80 {
		t.Errorf("hours = %+v, want the closed-hour job in %s", hours, closedHour)
	}

	// A service whose store never opened says so instead of answering "no jobs".
	if _, err := (&Service{}).Buckets(context.Background(), metrics.ResolutionMinute, "emails", time.Now().Add(-time.Hour), time.Now()); !errors.Is(err, metrics.ErrDisabled) {
		t.Errorf("error = %v, want ErrDisabled", err)
	}
}

func TestQueueLoopSamplesOnItsInterval(t *testing.T) {
	inspector := &stubInspector{inspection: adapter.Inspection{
		Connection: "redis",
		Queues:     []adapter.QueueDepth{{Queue: "default"}},
	}}
	service := queueTestService(t, inspector)
	service.queueInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.queueLoop(ctx)
	}()

	waitUntil(t, 2*time.Second, func() bool { return inspector.calls.Load() >= 2 }, "the queue loop to sample twice")

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("queueLoop did not return after cancellation")
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

// runningService is the real service under test: one worker is up, the consumer runs and
// the metrics store is open.
type runningService struct {
	ctx         context.Context
	service     *Service
	raw         *goredis.Client
	stream      string
	application string
	exit        string
	manager     *process.Manager
	worker      process.Record

	// runDone closes when Run returns; runErr is only read after it closes.
	runDone chan struct{}
	runErr  error
}

// waitRun waits for Run to return and reports what it returned.
func (r *runningService) waitRun(t *testing.T, timeout time.Duration) error {
	t.Helper()
	select {
	case <-r.runDone:
		return r.runErr
	case <-time.After(timeout):
		t.Fatalf("the service was still running %s after the lock was taken", timeout)
		return nil
	}
}

// startService starts the service against Redis with its worker held up until the exit
// file is written. prepare runs before Run, so a test can seed state the service has to
// deal with on startup. It skips when Redis is unreachable.
func startService(t *testing.T, prepare func(*Service)) *runningService {
	t.Helper()
	const address = "127.0.0.1:6379"

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	raw := goredis.NewClient(&goredis.Options{Addr: address})
	if err := raw.Ping(ctx).Err(); err != nil {
		_ = raw.Close()
		t.Skipf("redis at %s is unavailable: %v", address, err)
	}

	exit := filepath.Join(t.TempDir(), "exit")
	t.Setenv("GOHORIZON_TEST_WORKER_EXIT", exit)
	t.Setenv("GOHORIZON_TEST_CAPABILITIES", capabilitiesPayload("90"))

	cfg := preflightConfig(t, 60)
	cfg.Redis.Address = address
	// A namespace of its own keeps the stream, consumer group and lock isolated.
	cfg.Applications[0].ID = "e2e" + strconv.FormatInt(time.Now().UnixNano(), 36)
	// The replacement must stay away long enough for a classification to be readable.
	cfg.Applications[0].Supervisors[0].RestartBackoff = 30

	service, err := New(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		_ = raw.Close()
		t.Fatalf("New: %v", err)
	}
	if service.store == nil {
		_ = raw.Close()
		t.Fatal("metrics history was not opened")
	}
	if prepare != nil {
		prepare(service)
	}

	running := &runningService{
		ctx:         ctx,
		service:     service,
		raw:         raw,
		stream:      service.client.Keys().Events(),
		application: cfg.Applications[0].ID,
		exit:        exit,
		manager:     service.managers[0],
		runDone:     make(chan struct{}),
	}

	runCtx, stop := context.WithCancel(ctx)
	go func() {
		running.runErr = service.Run(runCtx)
		close(running.runDone)
	}()
	t.Cleanup(func() {
		stop()
		select {
		case <-running.runDone:
		case <-time.After(15 * time.Second):
			t.Error("the service did not stop within 15s")
		}
		if err := service.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		cleanupCtx, cleanup := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanup()
		_ = raw.Del(cleanupCtx, service.client.Keys().Events(), service.client.Keys().SupervisorLock()).Err()
		_ = raw.Close()
	})

	waitUntil(t, 10*time.Second, func() bool {
		return running.manager.Snapshot().Running == 1
	}, "the worker to start")
	running.worker = running.manager.Snapshot().Workers[0]

	return running
}

// TestServiceConsumesTelemetryEndToEnd runs the real service: a worker is spawned, a
// worker.stopped entry classifies its exit by the reported reason, and a job.finished
// entry is persisted into the metrics history the service opened.
func TestServiceConsumesTelemetryEndToEnd(t *testing.T) {
	running := startService(t, nil)

	payload := fmt.Sprintf(
		`{"schema":1,"id":"%s","type":"worker.stopped","application":%q,"environment":"production",`+
			`"worker":%q,"at":%q,"worker_event":{"exit_code":1,"reason":"max_jobs"}}`,
		"e2e-"+strconv.FormatInt(time.Now().UnixNano(), 36),
		running.application, running.worker.Name, time.Now().UTC().Format(time.RFC3339Nano),
	)
	entryID, err := running.raw.XAdd(running.ctx, &goredis.XAddArgs{Stream: running.stream, Values: map[string]any{"payload": payload}}).Result()
	if err != nil {
		t.Fatalf("publish worker.stopped: %v", err)
	}

	// The consumer acknowledges only after it handled the entry, so an acked entry means
	// the reason reached the manager before this worker exits.
	waitUntil(t, 10*time.Second, func() bool {
		groups, err := running.raw.XInfoGroups(context.Background(), running.stream).Result()
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

	if err := os.WriteFile(running.exit, []byte("exit"), 0o600); err != nil {
		t.Fatalf("trigger the worker exit: %v", err)
	}

	waitUntil(t, 10*time.Second, func() bool {
		workers := running.manager.Snapshot().Workers
		return len(workers) == 1 && workers[0].State == process.StateStopped
	}, "the manager to classify the exit")

	classified := running.manager.Snapshot().Workers[0]
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
	jobID := "e2e-job-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	jobPayload := fmt.Sprintf(
		`{"schema":1,"id":"%s","type":"job.finished","application":%q,"environment":"production",`+
			`"worker":%q,"at":%q,"job":{"uuid":%q,"id":"1","class":"App\\Jobs\\Synthetic",`+
			`"connection":"redis","queue":"emails","attempts":1,"outcome":"processed","duration_ms":1500}}`,
		jobID, running.application, running.worker.Name, time.Now().UTC().Format(time.RFC3339Nano), jobID,
	)
	if _, err := running.raw.XAdd(running.ctx, &goredis.XAddArgs{Stream: running.stream, Values: map[string]any{"payload": jobPayload}}).Result(); err != nil {
		t.Fatalf("publish job.finished: %v", err)
	}

	waitUntil(t, 10*time.Second, func() bool {
		buckets, err := running.service.store.Buckets(context.Background(), metrics.ResolutionMinute, "emails",
			time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
		return err == nil && len(buckets) == 1 && buckets[0].Processed == 1
	}, "the job metric to be persisted")

	buckets, err := running.service.store.Buckets(running.ctx, metrics.ResolutionMinute, "emails",
		time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("read metrics history: %v", err)
	}
	if buckets[0].DurationMS != 1500 {
		t.Errorf("duration = %d, want the reported 1500ms", buckets[0].DurationMS)
	}
}

// TestServicePrunesMetricsOnStartup seeds history a previous run left behind and proves
// the startup pass removes it. The schedule is an hour away, so only the startup pass
// can be responsible.
func TestServicePrunesMetricsOnStartup(t *testing.T) {
	old := time.Now().Add(-8 * 24 * time.Hour)
	running := startService(t, func(service *Service) {
		service.pruneInterval = time.Hour
		// One bucket past the 7-day window, one inside it.
		if err := service.store.RecordJob(context.Background(), job("old-event", "emails", "processed", 10, old)); err != nil {
			t.Fatalf("seed old metric: %v", err)
		}
		if err := service.store.RecordJob(context.Background(), job("fresh-event", "emails", "processed", 10, time.Now().Add(-time.Hour))); err != nil {
			t.Fatalf("seed fresh metric: %v", err)
		}
	})

	from := old.Add(-time.Hour)
	waitUntil(t, 10*time.Second, func() bool {
		buckets, err := running.service.store.Buckets(context.Background(), metrics.ResolutionMinute, "emails", from, time.Now())
		return err == nil && len(buckets) == 1
	}, "the startup prune to remove the 8-day-old bucket")

	buckets, err := running.service.store.Buckets(running.ctx, metrics.ResolutionMinute, "emails", from, time.Now())
	if err != nil {
		t.Fatalf("read metrics history: %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("buckets = %+v, want only the bucket inside the retention window", buckets)
	}
	if buckets[0].Start.Equal(old.Truncate(time.Minute)) {
		t.Fatal("the 8-day-old bucket survived the startup prune")
	}
	if buckets[0].Processed != 1 || buckets[0].DurationMS != 10 {
		t.Errorf("bucket = %+v, want the in-window job kept", buckets[0])
	}

	// The rollup runs before the prune, so the expired minute survives as its hour.
	hours, err := running.service.store.Buckets(running.ctx, metrics.ResolutionHourly, "emails", from, old.Add(time.Hour))
	if err != nil {
		t.Fatalf("read hourly history: %v", err)
	}
	if len(hours) != 1 || hours[0].Processed != 1 {
		t.Errorf("hours = %+v, want the expired minute preserved by the startup rollup", hours)
	}
}

// TestServicePrunesMetricsOnSchedule proves the periodic pass: the database is empty at
// startup, so the bucket written afterwards can only be removed by the schedule.
func TestServicePrunesMetricsOnSchedule(t *testing.T) {
	running := startService(t, func(service *Service) {
		service.pruneInterval = 50 * time.Millisecond
	})

	old := time.Now().Add(-9 * 24 * time.Hour)
	if err := running.service.store.RecordJob(running.ctx, job("loop-old", "emails", "processed", 10, old)); err != nil {
		t.Fatalf("seed metric for the scheduled pass: %v", err)
	}

	waitUntil(t, 5*time.Second, func() bool {
		buckets, err := running.service.store.Buckets(context.Background(), metrics.ResolutionMinute, "emails", old.Add(-time.Hour), old.Add(time.Hour))
		return err == nil && len(buckets) == 0
	}, "the scheduled prune to remove a bucket written after startup")
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

// TestLockLossFencesTheService proves the in-process half of the single-owner rule: Redis
// answers that the key belongs to someone else, so the service stops its worker and
// returns ErrLockLost instead of continuing to supervise a pool it no longer owns.
func TestLockLossFencesTheService(t *testing.T) {
	running := startService(t, func(service *Service) {
		service.renewInterval = 50 * time.Millisecond
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	lock := running.service.client.Keys().SupervisorLock()
	if err := running.raw.Set(ctx, lock, "another-instance", 30*time.Second).Err(); err != nil {
		t.Fatalf("take over the lock: %v", err)
	}

	runErr := running.waitRun(t, 20*time.Second)
	if !errors.Is(runErr, ErrLockLost) {
		t.Fatalf("Run returned %v, want ErrLockLost", runErr)
	}
	if stats := running.manager.Snapshot(); stats.Running != 0 {
		t.Errorf("running=%d after the lock loss, want the worker released", stats.Running)
	}
	if _, err := process.Probe(running.worker.PID); err == nil {
		t.Errorf("worker pid %d is still alive after the lock loss", running.worker.PID)
	}

	holder, err := running.raw.Get(ctx, lock).Result()
	if err != nil {
		t.Fatalf("read the lock after the loser stopped: %v", err)
	}
	// Releasing compares the owner token; a blind DEL would delete the new owner's lock.
	if holder != "another-instance" {
		t.Errorf("lock holder = %q, want the new owner's token untouched", holder)
	}
}

// TestShouldFenceAtTheSilenceBoundary pins the fence threshold: silence is tolerated for one
// TTL, the moment any other instance could take the key, plus slack for a renewal that was in
// flight. Fencing earlier would stop workers whose lock is provably still theirs.
func TestShouldFenceAtTheSilenceBoundary(t *testing.T) {
	if lockSilenceAllowance <= lockTTL {
		t.Fatalf("lockSilenceAllowance = %s, must outlive lockTTL = %s", lockSilenceAllowance, lockTTL)
	}

	allowance := lockSilenceAllowance
	confirmed := time.Unix(1_700_000_000, 0)
	cases := []struct {
		name    string
		elapsed time.Duration
		want    bool
	}{
		{"just confirmed", 0, false},
		{"one TTL later", lockTTL, false},
		{"one millisecond before the allowance", allowance - time.Millisecond, false},
		{"exactly at the allowance", allowance, true},
		{"well past the allowance", allowance + 5*time.Second, true},
	}
	for _, tc := range cases {
		if got := shouldFence(confirmed, confirmed.Add(tc.elapsed), allowance); got != tc.want {
			t.Errorf("%s (%s): shouldFence = %v, want %v", tc.name, tc.elapsed, got, tc.want)
		}
	}
}

// TestShortPoolNamesTheQueuesItCannotServe pins the visibility rule for a pool shorter
// than its queue list: round-robin slots 0..min-1 leave the tail of the list unserved, so
// startup says which queues those are instead of leaving it to be discovered. An equal or
// larger pool is silent, because nothing is missing.
// job builds the record a consumer would hand the store for one finished attempt.
func job(id, queue, outcome string, durationMS int64, at time.Time) metrics.JobRecord {
	return metrics.JobRecord{
		EventID:    id,
		JobID:      "job-" + id,
		UUID:       "uuid-" + id,
		Class:      `App\Jobs\Synthetic`,
		Queue:      queue,
		Connection: "redis",
		Outcome:    outcome,
		Attempts:   1,
		DurationMS: durationMS,
		FinishedAt: at,
	}
}

func TestShortPoolNamesTheQueuesItCannotServe(t *testing.T) {
	tests := []struct {
		name    string
		queues  []string
		workers int
		want    []string
	}{
		{"one worker, three queues", []string{"default", "emails", "notifications"}, 1, []string{"emails", "notifications"}},
		{"one worker per queue", []string{"default", "emails", "notifications"}, 3, nil},
		{"more workers than queues", []string{"default", "emails"}, 5, nil},
		{"no workers at all", []string{"default", "emails"}, 0, []string{"default", "emails"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			sup := &config.Supervisor{
				ID:      "default-supervisor",
				Queues:  tc.queues,
				Workers: config.Workers{Min: tc.workers, Max: tc.workers},
			}
			logger := slog.New(slog.NewTextHandler(&logs, nil))

			if got := unservedQueues(tc.queues, tc.workers); strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("unserved = %v, want %v", got, tc.want)
			}
			warnIfShortPool(logger, sup)
			if tc.want == nil {
				if logs.Len() != 0 {
					t.Errorf("logged %q for a pool that serves every queue", logs.String())
				}
				return
			}
			line := logs.String()
			if !strings.Contains(line, "shorter than its queue list") {
				t.Errorf("log = %q, want one warning naming the short pool", line)
			}
			for _, queue := range tc.want {
				if !strings.Contains(line, queue) {
					t.Errorf("log = %q, want the unserved queue %q named", line, queue)
				}
			}
		})
	}
}

// TestLockSilenceFencesTheService proves the silent path end to end inside the process: a
// client that never confirms a renewal is treated as a lost lock once the allowance passes,
// so the worker is released instead of supervising a pool this instance cannot hold.
func TestLockSilenceFencesTheService(t *testing.T) {
	// The allowance has to outlive the first manager tick, otherwise the service fences
	// before a worker exists and this proves nothing about releasing one.
	running := startService(t, func(service *Service) {
		service.renewLock = func(context.Context, string, time.Duration) (bool, error) {
			return false, errors.New("redis is unreachable")
		}
		service.renewInterval = 20 * time.Millisecond
		service.fenceAfter = 3 * time.Second
	})

	runErr := running.waitRun(t, 20*time.Second)
	if !errors.Is(runErr, ErrLockLost) {
		t.Fatalf("Run returned %v, want ErrLockLost", runErr)
	}
	if !strings.Contains(runErr.Error(), "not confirmed ownership") {
		t.Errorf("error = %q, want the unconfirmed renewals named as the reason", runErr)
	}
	if stats := running.manager.Snapshot(); stats.Running != 0 {
		t.Errorf("running=%d after the lock was presumed gone, want the worker released", stats.Running)
	}
	if _, err := process.Probe(running.worker.PID); err == nil {
		t.Errorf("worker pid %d is still alive after the lock was presumed gone", running.worker.PID)
	}
}
