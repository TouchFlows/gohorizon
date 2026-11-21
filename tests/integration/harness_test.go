//go:build linux

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/gohorizon/gohorizon/internal/api"
	"github.com/gohorizon/gohorizon/internal/config"
	"github.com/gohorizon/gohorizon/internal/process"
	"github.com/gohorizon/gohorizon/internal/supervisor"
)

const (
	testApplication = "gohorizon-e2e"
	testEnvironment = "e2e"
	testQueue       = "gohorizon-e2e"
	testRedisDB     = 15
	testRedisAddr   = "127.0.0.1:6379"
)

// harness owns the disposable application, the Redis keys and the processes one
// acceptance test uses. Everything it creates is removed when the test ends.
type harness struct {
	t       *testing.T
	root    string
	fixture string
	php     string
	dir     string
	redis   *goredis.Client
}

// newHarness skips unless the operator asked for acceptance coverage, then prepares the
// fixture's environment. The fixture talks to a real Laravel queue on a dedicated Redis
// database, under a namespace this test owns.
func newHarness(t *testing.T) *harness {
	t.Helper()

	if os.Getenv("GOHORIZON_E2E") != "1" {
		t.Skip("acceptance tests need GOHORIZON_E2E=1 (see README)")
	}

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate the test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	fixture := filepath.Join(root, "tests", "fixtures", "laravel-app")
	if _, err := os.Stat(filepath.Join(fixture, "vendor", "autoload.php")); err != nil {
		t.Skipf("fixture dependencies are not installed; run: cd %s && composer install", fixture)
	}

	php, err := exec.LookPath("php")
	if err != nil {
		t.Skipf("php is not on PATH: %v", err)
	}
	php, err = filepath.Abs(php)
	if err != nil {
		t.Fatalf("resolve php path: %v", err)
	}

	// The worker inherits this environment, so the application identity and the queue
	// must match the configuration written below.
	t.Setenv("APP_NAME", testApplication)
	t.Setenv("APP_ENV", testEnvironment)
	t.Setenv("APP_KEY", "base64:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("QUEUE_CONNECTION", "redis")
	t.Setenv("QUEUE_FAILED_DRIVER", "null")
	t.Setenv("REDIS_CLIENT", "predis")
	t.Setenv("REDIS_HOST", "127.0.0.1")
	t.Setenv("REDIS_PORT", "6379")
	t.Setenv("REDIS_DB", fmt.Sprint(testRedisDB))
	t.Setenv("CACHE_STORE", "file")
	t.Setenv("SESSION_DRIVER", "file")
	t.Setenv("LOG_CHANNEL", "errorlog")
	t.Setenv("GOHORIZON_APPLICATION", testApplication)
	t.Setenv("GOHORIZON_ENVIRONMENT", testEnvironment)
	t.Setenv("GOHORIZON_REDIS_CONNECTION", "default")
	t.Setenv("GOHORIZON_TELEMETRY_BATCH", "1")
	t.Setenv("GOHORIZON_E2E_QUEUE", testQueue)

	h := &harness{t: t, root: root, fixture: fixture, php: php, dir: t.TempDir()}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	h.redis = goredis.NewClient(&goredis.Options{Addr: testRedisAddr, DB: testRedisDB, DialTimeout: 2 * time.Second})
	if err := h.redis.Ping(ctx).Err(); err != nil {
		t.Skipf("acceptance tests need Redis at %s: %v", testRedisAddr, err)
	}

	h.cleanRedis()
	t.Cleanup(func() {
		h.killWorkers()
		h.cleanRedis()
		if err := h.redis.Close(); err != nil {
			t.Logf("close redis: %v", err)
		}
	})

	return h
}

// writeConfig writes a gohorizon.yaml for the fixture and returns it with its path.
func (h *harness) writeConfig(listen string) (*config.Config, string) {
	h.t.Helper()

	src := fmt.Sprintf(`version: 1

server:
  listen: %q

storage:
  driver: sqlite
  path: %q

redis:
  address: %q
  password_env: GOHORIZON_REDIS_PASSWORD
  db: %d

applications:
  - id: %s
    name: GoHorizon acceptance fixture
    path: %q
    php: %q
    artisan: artisan
    environment: %s
    redis_connection: redis
    supervisors:
      - id: e2e
        queues:
          - %s
        balance:
          strategy: fixed
        workers:
          min: 1
          max: 1
        worker:
          sleep: 1
          tries: 1
          timeout: 10
          memory: 128
        shutdown_grace: 30
        restart_backoff: 1
`,
		listen,
		filepath.Join(h.dir, "metrics.db"),
		testRedisAddr,
		testRedisDB,
		testApplication,
		h.fixture,
		h.php,
		testEnvironment,
		testQueue,
	)

	path := filepath.Join(h.dir, "gohorizon.yaml")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		h.t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		h.t.Fatalf("load config: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		h.t.Fatalf("validate config: %v", err)
	}
	return cfg, path
}

// startService runs a supervisor in this process behind a real HTTP server, the same
// wiring cmd/gohorizon uses.
func (h *harness) startService(cfg *config.Config) (string, func()) {
	h.t.Helper()

	logs := &lockedBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, cancel := context.WithCancel(context.Background())
	service, err := supervisor.New(ctx, cfg, log)
	if err != nil {
		t := h.t
		t.Fatalf("supervisor.New: %v", err)
	}
	server := httptest.NewServer(api.NewRouter(service, log))
	runErr := make(chan error, 1)
	go func() { runErr <- service.Run(ctx) }()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-runErr:
				if err != nil {
					h.t.Errorf("service.Run: %v", err)
				}
			case <-time.After(90 * time.Second):
				h.t.Error("the service did not stop after its workers were signalled")
			}
			server.Close()
			if err := service.Close(); err != nil {
				h.t.Errorf("service.Close: %v", err)
			}
			if h.t.Failed() {
				h.t.Logf("GoHorizon log:\n%s", logs.String())
			}
		})
	}
	h.t.Cleanup(stop)
	return server.URL, stop
}

// buildBinary compiles the real command, so the restart test can kill a supervisor
// process without killing the test process.
func (h *harness) buildBinary() string {
	h.t.Helper()

	binary := filepath.Join(h.dir, "gohorizon")
	cmd := exec.Command("go", "build", "-o", binary, "./cmd/gohorizon")
	cmd.Dir = h.root
	if out, err := cmd.CombinedOutput(); err != nil {
		h.t.Fatalf("go build ./cmd/gohorizon: %v\n%s", err, out)
	}
	return binary
}

type instance struct {
	cmd     *exec.Cmd
	logPath string
	done    chan struct{}
	waitErr error
}

func (h *harness) startBinary(binary, configPath string) *instance {
	h.t.Helper()

	logPath := filepath.Join(h.dir, fmt.Sprintf("gohorizon-%d.log", time.Now().UnixNano()))
	logFile, err := os.Create(logPath)
	if err != nil {
		h.t.Fatalf("create supervisor log: %v", err)
	}

	cmd := exec.Command(binary, "run", "-config", configPath)
	cmd.Dir = h.root
	cmd.Env = os.Environ()
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		h.t.Fatalf("start gohorizon: %v", err)
	}

	inst := &instance{cmd: cmd, logPath: logPath, done: make(chan struct{})}
	go func() {
		inst.waitErr = cmd.Wait()
		logFile.Close()
		close(inst.done)
	}()

	h.t.Cleanup(func() {
		select {
		case <-inst.done:
		default:
			_ = cmd.Process.Kill()
			<-inst.done
		}
		if h.t.Failed() {
			if data, err := os.ReadFile(logPath); err == nil {
				h.t.Logf("supervisor log %s:\n%s", logPath, tail(data, 4000))
			}
		}
	})
	return inst
}

func (i *instance) wait(timeout time.Duration) error {
	select {
	case <-i.done:
		return i.waitErr
	case <-time.After(timeout):
		return fmt.Errorf("still running after %s", timeout)
	}
}

func (i *instance) kill(t *testing.T) {
	t.Helper()
	if err := i.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("kill supervisor: %v", err)
	}
	if err := i.wait(15 * time.Second); err == nil {
		t.Fatal("the killed supervisor exited successfully; the restart test needs a real kill")
	}
}

func (i *instance) terminate(t *testing.T) {
	t.Helper()
	if err := i.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM supervisor: %v", err)
	}
	if err := i.wait(90 * time.Second); err != nil {
		t.Fatalf("supervisor did not stop gracefully: %v", err)
	}
}

// dispatch enqueues one fixture job through Laravel itself, so the test never writes a
// queue payload by hand.
func (h *harness) dispatch(kind, argument string) error {
	cmd := exec.Command(h.php, "dispatch.php", kind, argument)
	cmd.Dir = h.fixture
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("dispatch %s: %w\n%s", kind, err, out)
	}
	return nil
}

// report is the part of GET /api/v1/health the acceptance tests assert on.
type report struct {
	Status    string `json:"status"`
	Telemetry struct {
		Events  int `json:"events"`
		Invalid int `json:"invalid"`
		Errors  int `json:"errors"`
	} `json:"telemetry"`
	Supervisors []reportSupervisor `json:"supervisors"`
}

type reportSupervisor struct {
	ID      string         `json:"id"`
	Desired int            `json:"desired"`
	Running int            `json:"running"`
	Crashed int            `json:"crashed"`
	Adopted int            `json:"adopted"`
	Workers []reportWorker `json:"workers"`
}

type reportWorker struct {
	UUID     string `json:"uuid"`
	PID      int    `json:"pid"`
	State    string `json:"state"`
	Restarts int    `json:"restarts"`
	Adopted  bool   `json:"adopted"`
}

func (r report) supervisor(t *testing.T) reportSupervisor {
	t.Helper()
	if len(r.Supervisors) != 1 {
		t.Fatalf("supervisors = %d, want the one configured supervisor", len(r.Supervisors))
	}
	return r.Supervisors[0]
}

func (h *harness) fetchReport(base string) (report, error) {
	resp, err := http.Get(base + "/api/v1/health")
	if err != nil {
		return report{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		return report{}, fmt.Errorf("health returned %d", resp.StatusCode)
	}
	var payload report
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return report{}, fmt.Errorf("decode health: %w", err)
	}
	return payload, nil
}

type history struct {
	Buckets []struct {
		Processed int `json:"processed"`
		Released  int `json:"released"`
		Failed    int `json:"failed"`
		TimedOut  int `json:"timed_out"`
	} `json:"buckets"`
}

func (h history) total(processed bool) int {
	sum := 0
	for _, bucket := range h.Buckets {
		if processed {
			sum += bucket.Processed
		} else {
			sum += bucket.Failed
		}
	}
	return sum
}

func (h *harness) fetchHistory(base string) (history, error) {
	resp, err := http.Get(base + "/api/v1/metrics?queue=" + testQueue)
	if err != nil {
		return history{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return history{}, fmt.Errorf("metrics returned %d", resp.StatusCode)
	}
	var payload history
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return history{}, fmt.Errorf("decode metrics: %w", err)
	}
	return payload, nil
}

// eventually polls until check passes. It reports the last failure, so a timeout says
// what was actually wrong instead of only that nothing happened.
func eventually(t *testing.T, timeout time.Duration, what string, check func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if err := check(); err == nil {
			return
		} else {
			last = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s: %v", timeout, what, last)
}

// cleanRedis removes only this test's namespace and queue keys. It never flushes the
// database: the fixture deliberately shares a Redis server with whatever else is
// running on it.
func (h *harness) cleanRedis() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	patterns := []string{
		"gohorizon:" + testApplication + ":" + testEnvironment + ":*",
		"*queues:" + testQueue + "*",
	}
	for _, pattern := range patterns {
		iter := h.redis.Scan(ctx, 0, pattern, 200).Iterator()
		for iter.Next(ctx) {
			if err := h.redis.Del(ctx, iter.Val()).Err(); err != nil {
				h.t.Logf("delete key %q: %v", iter.Val(), err)
			}
		}
		if err := iter.Err(); err != nil {
			h.t.Logf("scan %q: %v", pattern, err)
		}
	}
}

// killWorkers removes any worker a failed test left behind, matching only this
// application's worker names.
func (h *harness) killWorkers() {
	prefix := "gohorizon:" + testApplication + ":" + testEnvironment + ":"
	infos, err := process.Scan()
	if err != nil {
		h.t.Logf("scan processes: %v", err)
		return
	}
	for _, info := range infos {
		if strings.HasPrefix(info.Flag("name"), prefix) {
			if err := syscall.Kill(info.PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				h.t.Logf("kill leftover worker %d: %v", info.PID, err)
			}
		}
	}
}

func testContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release the port: %v", err)
	}
	return port
}

func tail(data []byte, limit int) string {
	if len(data) <= limit {
		return string(data)
	}
	return "…" + string(data[len(data)-limit:])
}

// lockedBuffer keeps slog output safe to read while the service and the HTTP handler
// are still writing to it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
