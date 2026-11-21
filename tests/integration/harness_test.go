//go:build linux

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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

	"github.com/gohorizon/gohorizon/internal/adapter"
	"github.com/gohorizon/gohorizon/internal/metrics"
	"github.com/gohorizon/gohorizon/internal/process"
)

const (
	testApplication = "gohorizon-e2e"
	testEnvironment = "e2e"
	testQueue       = "gohorizon-e2e"
	testConnection  = "redis"
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

	// A job whose worker dies is only recoverable through its reservation window: too
	// long and the retry test waits a minute and a half, too short and it must stay
	// above worker.timeout, which the preflight enforces.
	t.Setenv("REDIS_QUEUE_RETRY_AFTER", "10")

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

// writeConfig writes a gohorizon.yaml for the fixture and returns its path with the
// loopback URL the command will serve.
func (h *harness) writeConfig() (string, string) {
	h.t.Helper()
	return h.writeConfigAt(testRedisAddr)
}

// writeConfigAt is writeConfig with the control plane's Redis address overridden, so a
// test can route GoHorizon through a proxy. Only GoHorizon's own client moves: the
// fixture's Laravel connection stays on testRedisAddr through its environment, which is
// what makes an outage here an outage of the control plane alone.
func (h *harness) writeConfigAt(address string) (string, string) {
	h.t.Helper()

	listen := fmt.Sprintf("127.0.0.1:%d", freePort(h.t))
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
          timeout: 6
          memory: 128
        shutdown_grace: 30
        restart_backoff: 1
`,
		listen,
		filepath.Join(h.dir, "metrics.db"),
		address,
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
	return path, "http://" + listen
}

// buildBinary compiles the real command, so every acceptance test runs the same wiring
// the operator does, and a test can kill a supervisor without killing itself.
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

// inspectQueues asks the application itself how much work its queue holds, through the
// same documented command the service samples depth with. Nothing here reads a Laravel
// queue key, so the assertions hold whatever layout the framework uses.
func (h *harness) inspectQueues(ctx context.Context) (adapter.Inspection, error) {
	probe := adapter.Probe{PHP: h.php, Artisan: "artisan", Dir: h.fixture}
	return probe.Queues(ctx, testConnection, []string{testQueue})
}

// awaitQueueDepth waits until Laravel reports exactly these counts for the test queue.
// It is how a test proves a job is still reserved (a killed worker did not lose it) or
// gone (a graceful stop did not leave it behind).
func awaitQueueDepth(t *testing.T, h *harness, pending, reserved int, what string) {
	t.Helper()

	eventually(t, 20*time.Second, what, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		inspection, err := h.inspectQueues(ctx)
		if err != nil {
			return err
		}
		row := inspection.Queues[0]
		if row.Pending == nil || row.Reserved == nil {
			return fmt.Errorf("inspection did not report pending and reserved: %+v", row)
		}
		if *row.Pending != pending || *row.Reserved != reserved {
			return fmt.Errorf("pending=%d reserved=%d, want pending=%d reserved=%d",
				*row.Pending, *row.Reserved, pending, reserved)
		}
		return nil
	})
}

// persistedHistory reads the metrics the service wrote to disk. A stopped service has no
// API to ask, and its history is the record of what it accounted for, so reopening the
// database is how a test proves an event was handled before the process exited.
func (h *harness) persistedHistory(queue string) (processed, failed int, err error) {
	store, err := metrics.Open(filepath.Join(h.dir, "metrics.db"), metrics.DefaultRetention())
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = store.Close() }()

	now := time.Now()
	buckets, err := store.Buckets(context.Background(), metrics.ResolutionMinute, queue,
		now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		return 0, 0, err
	}
	for _, bucket := range buckets {
		processed += int(bucket.Processed)
		failed += int(bucket.Failed)
	}
	return processed, failed, nil
}

// awaitStarted waits until the slow fixture job wrote its start line, so a test only
// signals a worker it can prove is in the middle of a job.
func awaitStarted(t *testing.T, marker string) {
	t.Helper()

	eventually(t, 30*time.Second, "the slow job to start inside the worker", func() error {
		lines, err := markerLines(marker)
		if err != nil {
			return err
		}
		if lines["started"] == 0 {
			return fmt.Errorf("no start line in %s yet", marker)
		}
		return nil
	})
}

// markerLines counts the lines one of the fixture's marker files holds, so a test can
// assert a job ran once instead of at least once.
func markerLines(path string) (map[string]int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int)
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			counts[line]++
		}
	}
	return counts, nil
}

// requireRuns asserts how many times the slow job started and finished. The counts are
// exact on purpose: "at least once" would pass while a job ran twice.
func requireRuns(t *testing.T, marker string, started, ran int) {
	t.Helper()

	lines, err := markerLines(marker)
	if err != nil {
		t.Fatalf("read %s: %v", marker, err)
	}
	if lines["started"] != started || lines["ran"] != ran {
		t.Fatalf("%s: started=%d ran=%d, want started=%d ran=%d",
			marker, lines["started"], lines["ran"], started, ran)
	}
}

// report is the part of GET /api/v1/health the acceptance tests assert on.
type report struct {
	Status string `json:"status"`
	Lock   struct {
		Held      bool   `json:"held"`
		Degraded  bool   `json:"degraded"`
		LastError string `json:"last_error"`
	} `json:"lock"`
	Telemetry struct {
		Events    int    `json:"events"`
		Invalid   int    `json:"invalid"`
		Errors    int    `json:"errors"`
		Failing   bool   `json:"failing"`
		LastError string `json:"last_error"`
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
	_, payload, err := h.fetchReportCode(base)
	return payload, err
}

// fetchReportCode is fetchReport plus the HTTP status, so a test can prove the 503 that
// ADR 0004 ties to lock loss instead of inferring it from the body.
func (h *harness) fetchReportCode(base string) (int, report, error) {
	resp, err := http.Get(base + "/api/v1/health")
	if err != nil {
		return 0, report{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		return resp.StatusCode, report{}, fmt.Errorf("health returned %d", resp.StatusCode)
	}

	var payload report
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return resp.StatusCode, report{}, fmt.Errorf("decode health: %w", err)
	}
	return resp.StatusCode, payload, nil
}

type history struct {
	Buckets []struct {
		Processed int `json:"processed"`
		Released  int `json:"released"`
		Failed    int `json:"failed"`
		TimedOut  int `json:"timed_out"`
	} `json:"buckets"`
}

// outcomeTotals is every outcome the history holds, summed over the buckets.
type outcomeTotals struct {
	processed int
	released  int
	failed    int
	timedOut  int
}

func (h history) outcomes() outcomeTotals {
	var totals outcomeTotals
	for _, bucket := range h.Buckets {
		totals.processed += bucket.Processed
		totals.released += bucket.Released
		totals.failed += bucket.Failed
		totals.timedOut += bucket.TimedOut
	}
	return totals
}

func (h history) totals() (processed, failed int) {
	totals := h.outcomes()
	return totals.processed, totals.failed
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

// workerPIDs lists the live workers this application's supervisors own. The worker name
// prefix is GoHorizon's own namespace, so nothing else can match it.
func (h *harness) workerPIDs() ([]int, error) {
	prefix := "gohorizon:" + testApplication + ":" + testEnvironment + ":"
	infos, err := process.Scan()
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, info := range infos {
		if strings.HasPrefix(info.Flag("name"), prefix) {
			pids = append(pids, info.PID)
		}
	}
	return pids, nil
}

// killWorkers removes any worker a failed test left behind, matching only this
// application's worker names.
func (h *harness) killWorkers() {
	pids, err := h.workerPIDs()
	if err != nil {
		h.t.Logf("scan processes: %v", err)
		return
	}
	for _, pid := range pids {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			h.t.Logf("kill leftover worker %d: %v", pid, err)
		}
	}
}

// supervisorLockKey is the ownership key the service takes at startup. A lock-loss test
// writes it as the instance that takes over would, through the same raw client the
// other Redis assertions use.
func (h *harness) supervisorLockKey() string {
	return "gohorizon:" + testApplication + ":" + testEnvironment + ":coord:lock:supervisor"
}

// redisProxy is a loopback TCP proxy in front of the fixture's Redis. Pausing it closes the
// listener and every live connection, which is what an unreachable Redis looks like to
// GoHorizon's client, without touching the Redis the Laravel workers use.
//
// A proxy rather than stopping Redis on purpose: the suite shares the developer's Redis with
// whatever else runs on it, and pausing a proxy can only affect this test.
type redisProxy struct {
	t      *testing.T
	target string
	addr   string

	mu       sync.Mutex
	listener net.Listener
	conns    map[net.Conn]struct{}
	up       bool
}

// newRedisProxy listens on a loopback port and forwards to target until the test ends.
func newRedisProxy(t *testing.T, target string) *redisProxy {
	t.Helper()

	p := &redisProxy{t: t, target: target, conns: make(map[net.Conn]struct{})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the redis proxy: %v", err)
	}
	p.addr = listener.Addr().String()
	p.listener = listener
	p.up = true
	go p.serve(listener)

	t.Cleanup(func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.up = false
		p.closeLocked()
	})
	return p
}

// Addr is the address GoHorizon's redis.address must point at.
func (p *redisProxy) Addr() string { return p.addr }

// Pause makes the proxy unreachable: new dials are refused and open connections are
// dropped, the way a Redis that stopped accepting clients looks.
func (p *redisProxy) Pause() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.up {
		return
	}
	p.up = false
	p.closeLocked()
}

// Resume accepts clients again on the same address, so a client that retries reconnects
// without the test having to restart anything.
func (p *redisProxy) Resume() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.up {
		return
	}
	// Connections closed at the pause can leave the port in TIME_WAIT; a listener still
	// rebinds normally, but retry briefly instead of flaking the test.
	var err error
	for attempt := 0; attempt < 100; attempt++ {
		var listener net.Listener
		if listener, err = net.Listen("tcp", p.addr); err == nil {
			p.listener = listener
			p.up = true
			go p.serve(listener)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	p.t.Fatalf("resume the redis proxy on %s: %v", p.addr, err)
}

func (p *redisProxy) closeLocked() {
	if p.listener != nil {
		_ = p.listener.Close()
		p.listener = nil
	}
	for conn := range p.conns {
		_ = conn.Close()
	}
	clear(p.conns)
}

func (p *redisProxy) serve(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		p.track(conn)
		go p.pipe(conn)
	}
}

func (p *redisProxy) pipe(client net.Conn) {
	upstream, err := net.Dial("tcp", p.target)
	if err != nil {
		p.drop(client)
		return
	}
	p.track(upstream)

	go func() {
		_, _ = io.Copy(upstream, client)
		p.drop(client)
		p.drop(upstream)
	}()
	_, _ = io.Copy(client, upstream)
	p.drop(client)
	p.drop(upstream)
}

func (p *redisProxy) track(conn net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// A connection accepted just before a pause belongs to the paused proxy: closing it now
	// keeps Pause's promise that nothing stays connected.
	if !p.up {
		_ = conn.Close()
		return
	}
	p.conns[conn] = struct{}{}
}

func (p *redisProxy) drop(conn net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.conns, conn)
	_ = conn.Close()
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
