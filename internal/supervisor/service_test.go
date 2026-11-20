package supervisor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gohorizon/gohorizon/internal/config"
	"github.com/gohorizon/gohorizon/internal/redis"
	"github.com/gohorizon/gohorizon/internal/telemetry"
)

// TestCapabilitiesHelperProcess stands in for the application's `php artisan`. When it
// is asked for queue:work it records the spawn attempt, so a test can prove the
// preflight refused before any worker existed.
func TestCapabilitiesHelperProcess(t *testing.T) {
	payload, ok := os.LookupEnv("GOHORIZON_TEST_CAPABILITIES")
	if !ok {
		t.Skip("helper process only; launched by the supervisor preflight tests")
	}
	for _, arg := range os.Args[1:] {
		if arg == "queue:work" {
			if path := os.Getenv("GOHORIZON_TEST_SPAWN_FILE"); path != "" {
				_ = os.WriteFile(path, []byte(strings.Join(os.Args, " ")), 0o600)
			}
			os.Exit(0)
		}
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
	if report.Status != "degraded" {
		t.Errorf("status = %q, want degraded while ingestion is failing", report.Status)
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
