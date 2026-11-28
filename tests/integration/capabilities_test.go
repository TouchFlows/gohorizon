//go:build linux

package integration_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gohorizon/gohorizon/internal/auth"
)

// rawCapabilities mirrors `php artisan gohorizon:capabilities --json` itself, not the
// adapter's view of it: the test compares the endpoint with the PHP payload field by field,
// so a field the Go decoder stops reading cannot quietly become an empty value on both
// sides. RetryAfter is a pointer because null and a number mean different things.
type rawCapabilities struct {
	Schema      int    `json:"schema"`
	Package     string `json:"package"`
	Laravel     string `json:"laravel"`
	PHP         string `json:"php"`
	Application string `json:"application"`
	Environment string `json:"environment"`
	Queue       struct {
		Connections map[string]struct {
			Driver     string `json:"driver"`
			RetryAfter *int   `json:"retry_after"`
		} `json:"connections"`
	} `json:"queue"`
	Runtime struct {
		PCNTL bool `json:"pcntl"`
	} `json:"runtime"`
	Telemetry struct {
		Stream string `json:"stream"`
	} `json:"telemetry"`
	Events []string `json:"events"`
	// Payloads are the versions the application reports for its own commands' payloads.
	Payloads struct {
		Inspect    int `json:"inspect"`
		FailedJobs int `json:"failed_jobs"`
	} `json:"payload_schemas"`
}

// capabilitiesAnswer is the published descriptor as a client reads it.
type capabilitiesAnswer struct {
	Available       bool   `json:"available"`
	Schema          int    `json:"schema"`
	Package         string `json:"package"`
	PackageVersion  string `json:"package_version"`
	Laravel         string `json:"laravel"`
	PHP             string `json:"php"`
	Application     string `json:"application"`
	Environment     string `json:"environment"`
	DefaultConn     string `json:"default_connection"`
	TelemetryStream string `json:"telemetry_stream"`
	PCNTL           bool   `json:"pcntl"`
	Connections     map[string]struct {
		Driver     string `json:"driver"`
		RetryAfter *int   `json:"retry_after"`
	} `json:"connections"`
	Events     []string        `json:"events"`
	Operations map[string]bool `json:"operations"`
	// PayloadSchemas are the versions this service reads of the application's other payloads.
	PayloadSchemas struct {
		Inspect    int `json:"inspect"`
		FailedJobs int `json:"failed_jobs"`
	} `json:"payload_schemas"`
	// OperationReasons says why a withheld operation is withheld.
	OperationReasons map[string]string `json:"operation_reasons"`
	// ReadOnlySince is when the deployment went read-only, 0 while it is readable.
	ReadOnlySince int64 `json:"read_only_since"`
	// ApplicationPayloadSchemas is what the application says its own commands emit.
	ApplicationPayloadSchemas *struct {
		Inspect    int `json:"inspect"`
		FailedJobs int `json:"failed_jobs"`
	} `json:"application_payload_schemas"`
	Error string `json:"error"`
}

// capabilitiesPayload runs the documented command in the fixture, so the comparison is against
// the application's own answer and not against a second Go decode of the same bytes.
func (h *harness) capabilitiesPayload(t *testing.T) rawCapabilities {
	t.Helper()

	cmd := exec.Command(h.php, "artisan", "gohorizon:capabilities", "--json")
	cmd.Dir = h.fixture
	cmd.Env = os.Environ()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("php artisan gohorizon:capabilities --json: %v", err)
	}

	var payload rawCapabilities
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode the application's capabilities: %v\n%s", err, out)
	}
	return payload
}

// payloadSchema runs one of the application's documented commands and returns the schema its
// payload declares, so a version the descriptor publishes can be compared with the one the
// command actually prints rather than with a second Go constant.
func (h *harness) payloadSchema(t *testing.T, command ...string) int {
	t.Helper()

	args := append([]string{"artisan"}, command...)
	args = append(args, "--json")
	cmd := exec.Command(h.php, args...)
	cmd.Dir = h.fixture
	cmd.Env = os.Environ()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: %v", strings.Join(args, " "), err)
	}

	// A command may print a line before its payload, so the payload is the last JSON line.
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var payload struct {
			Schema int `json:"schema"`
		}
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			t.Fatalf("decode %s: %v", strings.Join(args, " "), err)
		}
		if payload.Schema == 0 {
			t.Fatalf("%s printed no schema", strings.Join(args, " "))
		}
		return payload.Schema
	}
	t.Fatalf("%s printed no JSON payload", strings.Join(args, " "))
	return 0
}

// refusingPHP writes a PHP executable that refuses the capabilities command with this reason
// on stderr and passes every other command through to the fixture's own interpreter. It is a
// real deployment whose package is missing rather than a dressed endpoint: the descriptor is a
// startup snapshot, so the refusal has to happen while the binary starts.
func (h *harness) refusingPHP(reason string) string {
	h.t.Helper()

	path := filepath.Join(h.dir, "php-no-capabilities")
	script := fmt.Sprintf(`#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "gohorizon:capabilities" ]; then
    echo '%s' >&2
    exit 1
  fi
done
exec '%s' "$@"
`, reason, h.php)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		h.t.Fatalf("write the refusing php: %v", err)
	}
	return path
}

// answeringPHP writes a PHP executable that answers the capabilities command with this payload
// and passes every other command through to the fixture's own interpreter. The payload is the
// application's answer, not the endpoint's: what this service makes of it is the assertion.
func (h *harness) answeringPHP(payload string) string {
	h.t.Helper()

	path := filepath.Join(h.dir, "php-other-schema")
	script := fmt.Sprintf(`#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "gohorizon:capabilities" ]; then
    echo '%s'
    exit 0
  fi
done
exec '%s' "$@"
`, payload, h.php)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		h.t.Fatalf("write the answering php: %v", err)
	}
	return path
}

// fileAnsweringPHP writes a PHP executable that answers the capabilities command with the
// contents of answerPath, so a test can change the application's answer while the service runs,
// and passes every other command through to the fixture's own interpreter.
func (h *harness) fileAnsweringPHP(answerPath string) string {
	h.t.Helper()

	path := filepath.Join(h.dir, "php-file-answer")
	script := fmt.Sprintf(`#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "gohorizon:capabilities" ]; then
    cat '%s'
    exit 0
  fi
done
exec '%s' "$@"
`, answerPath, h.php)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		h.t.Fatalf("write the file-answering php: %v", err)
	}
	return path
}

// lastSuccessTimestamp reads the re-read timestamp out of a scrape, failing when the sample is
// missing rather than defaulting it to zero.
func lastSuccessTimestamp(t *testing.T, body []byte) int64 {
	t.Helper()

	const family = "gohorizon_capabilities_last_success_timestamp_seconds "
	for _, line := range strings.Split(string(body), "\n") {
		value, ok := strings.CutPrefix(line, family)
		if !ok {
			continue
		}
		parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			t.Fatalf("parse the last-success sample %q: %v", line, err)
		}
		return parsed
	}
	t.Fatalf("the scrape has no %s sample", family)
	return 0
}

// capabilitiesAnswerFor is a schema-1 descriptor whose queue connection has this retry_after, with
// pcntl present so the retry_after is the only thing the preflight can object to, and this
// package version so a test can make an upgrade visible.
func capabilitiesAnswerFor(retryAfter int, packageVersion string) string {
	return fmt.Sprintf(`{"schema":1,"package":"gohorizon/laravel","package_version":"%s","laravel":"13.0.0","php":"8.4.0",`+
		`"queue":{"connections":{"redis":{"driver":"redis","retry_after":%d}}},`+
		`"runtime":{"pcntl":true},"telemetry":{"stream":"gohorizon:events"},"events":[]}`, packageVersion, retryAfter)
}

// auditRecord returns the first audit line for action whose detail contains fragment, once it is
// written: the line an operator selects on after the fact.
func (i *instance) auditRecord(t *testing.T, action, fragment string) map[string]any {
	t.Helper()

	var record map[string]any
	eventually(t, 10*time.Second, "the audit line for "+action, func() error {
		data, err := os.ReadFile(i.logPath)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(data), "\n") {
			var candidate map[string]any
			if err := json.Unmarshal([]byte(line), &candidate); err != nil {
				continue
			}
			if candidate["msg"] != "audit" || candidate["action"] != action {
				continue
			}
			detail, _ := candidate["detail"].(string)
			if strings.Contains(detail, fragment) {
				record = candidate
				return nil
			}
		}
		return fmt.Errorf("no audit line for %s containing %q yet", action, fragment)
	})
	return record
}

// TestContradictionAppearsWhenTheApplicationLowersRetryAfter is the preflight's second life: it
// ran at launch, and the pool it approved keeps running. An application that lowers retry_after
// under the worker's timeout while the pool runs is the same contradiction found late, so it is
// published in health and the scrape instead of waiting for the next restart (docs/adr/0014).
func TestContradictionAppearsWhenTheApplicationLowersRetryAfter(t *testing.T) {
	h := newHarness(t)
	answer := filepath.Join(h.dir, "capabilities.json")
	if err := os.WriteFile(answer, []byte(capabilitiesAnswerFor(10, "0.1.0")), 0o644); err != nil {
		t.Fatalf("write the application's answer: %v", err)
	}

	configPath, base := h.writeConfigWithPHP(h.fileAnsweringPHP(answer))
	inst := h.startBinary(h.buildBinary(), configPath)
	awaitRunning(h, base)

	// The launch the preflight approved: a window above the worker's timeout, nothing to report.
	report, err := h.fetchReport(base)
	if err != nil {
		t.Fatalf("read health: %v", err)
	}
	if report.Capabilities.Contradiction != "" {
		t.Fatalf("contradiction = %q, want none while retry_after is above the timeout",
			report.Capabilities.Contradiction)
	}

	// The same re-read carries a package upgrade, so one transition is recorded with both facts.
	if err := os.WriteFile(answer, []byte(capabilitiesAnswerFor(2, "0.2.0")), 0o644); err != nil {
		t.Fatalf("rewrite the application's answer: %v", err)
	}

	eventually(t, 60*time.Second, "the contradiction in health", func() error {
		payload, err := h.fetchReport(base)
		if err != nil {
			return err
		}
		if payload.Status != "ok" {
			return fmt.Errorf("status = %q, want ok: the pool is still supervised", payload.Status)
		}
		if !payload.Capabilities.Available {
			return fmt.Errorf("available = false, want the readable answer with a contradiction beside it")
		}
		for _, want := range []string{"retry_after", "worker.timeout"} {
			if !strings.Contains(payload.Capabilities.Contradiction, want) {
				return fmt.Errorf("contradiction = %q, want it to name %q", payload.Capabilities.Contradiction, want)
			}
		}
		return nil
	})

	// The scrape carries the same state, so an alert needs no log and no JSON field.
	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the scrape: %v", err)
	}
	if !strings.Contains(string(body), "\ngohorizon_capabilities_contradiction 1\n") {
		t.Errorf("scrape has no contradiction sample:\n%s", body)
	}

	// And the transition is in the audit log, so "what changed, and when" is answerable without
	// reading the warnings above it.
	record := inst.auditRecord(t, "capability-change", "0.1.0 -> gohorizon/laravel 0.2.0")
	if record["outcome"] != "changed" {
		t.Errorf("audit outcome = %v, want changed", record["outcome"])
	}
	if _, ok := record["user"]; ok {
		t.Errorf("audit record = %v, want no actor for a change the service noticed", record)
	}
}

// startupWarning returns the WARN line whose message contains fragment and its structured
// fields, once the service has written it. A degraded start is announced in the log and
// nowhere else, so that line is the whole signal an operator has: the test reads it the way
// an operator would, and asserts the fields instead of the sentence they are glued into.
func (i *instance) startupWarning(t *testing.T, fragment string) (map[string]any, error) {
	t.Helper()

	var record map[string]any
	eventually(t, 10*time.Second, "the startup warning in the supervisor log", func() error {
		data, err := os.ReadFile(i.logPath)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(data), "\n") {
			var candidate map[string]any
			if err := json.Unmarshal([]byte(line), &candidate); err != nil {
				continue
			}
			msg, _ := candidate["msg"].(string)
			if candidate["level"] == "WARN" && strings.Contains(msg, fragment) {
				record = candidate
				break
			}
		}
		if record == nil {
			return fmt.Errorf("no WARN line containing %q yet", fragment)
		}
		return nil
	})
	return record, nil
}

func (h *harness) fetchCapabilities(t *testing.T, base string, cookies ...*http.Cookie) capabilitiesAnswer {
	t.Helper()

	request, err := http.NewRequest(http.MethodGet, base+"/api/v1/capabilities", nil)
	if err != nil {
		t.Fatalf("GET /api/v1/capabilities: %v", err)
	}
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET /api/v1/capabilities: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/v1/capabilities returned %d", resp.StatusCode)
	}

	var answer capabilitiesAnswer
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		t.Fatalf("decode /api/v1/capabilities: %v", err)
	}
	return answer
}

// TestCapabilitiesEndpointMatchesTheApplicationsPayload keeps the PHP payload and the Go
// descriptor from drifting apart. The endpoint is compared with the command's own output, not
// with the adapter's decode of it, so a field the decoder stops understanding shows up as a
// missing value rather than as two equally empty answers.
func TestCapabilitiesEndpointMatchesTheApplicationsPayload(t *testing.T) {
	h := newHarness(t)
	configPath, base := h.writeConfig()
	h.startBinary(h.buildBinary(), configPath)
	awaitRunning(h, base)

	raw := h.capabilitiesPayload(t)
	answer := h.fetchCapabilities(t, base)

	if !answer.Available || answer.Error != "" {
		t.Fatalf("available=%t error=%q, want the application's descriptor", answer.Available, answer.Error)
	}

	// Identifiers the application reports about itself.
	identifiers := []struct {
		field string
		got   string
		want  string
	}{
		{"package", answer.Package, raw.Package},
		{"laravel", answer.Laravel, raw.Laravel},
		{"php", answer.PHP, raw.PHP},
		{"application", answer.Application, raw.Application},
		{"environment", answer.Environment, raw.Environment},
		{"telemetry_stream", answer.TelemetryStream, raw.Telemetry.Stream},
	}
	for _, id := range identifiers {
		if id.want == "" {
			t.Errorf("the application reported no %s; the fixture no longer proves the field", id.field)
			continue
		}
		if id.got != id.want {
			t.Errorf("%s = %q, want the application's %q", id.field, id.got, id.want)
		}
	}
	if answer.Schema != raw.Schema || answer.Schema == 0 {
		t.Errorf("schema = %d, want the application's %d", answer.Schema, raw.Schema)
	}
	if answer.PCNTL != raw.Runtime.PCNTL {
		t.Errorf("pcntl = %t, want the application's %t", answer.PCNTL, raw.Runtime.PCNTL)
	}

	// The other payloads are versioned too, on both sides: the descriptor publishes the versions
	// this service reads, and the application reports the ones its own commands print. Compare
	// both with the commands themselves, so a package that bumps either payload cannot do it
	// without this test naming the mismatch — and the decoders refuse the payload at read time
	// with those same two numbers.
	inspectSchema := h.payloadSchema(t, "gohorizon:inspect", "--connection="+testConnection, "--queue="+testQueue)
	failedSchema := h.payloadSchema(t, "gohorizon:failed:list", "--limit=1")
	if got := answer.PayloadSchemas.Inspect; got != inspectSchema {
		t.Errorf("payload_schemas.inspect = %d, want the application's %d", got, inspectSchema)
	}
	if got := answer.PayloadSchemas.FailedJobs; got != failedSchema {
		t.Errorf("payload_schemas.failed_jobs = %d, want the application's %d", got, failedSchema)
	}
	if answer.ApplicationPayloadSchemas == nil {
		t.Fatal("application_payload_schemas is missing, so the application's report is not published")
	}
	if got := answer.ApplicationPayloadSchemas.Inspect; got != inspectSchema {
		t.Errorf("application_payload_schemas.inspect = %d, want the command's %d", got, inspectSchema)
	}
	if got := answer.ApplicationPayloadSchemas.FailedJobs; got != failedSchema {
		t.Errorf("application_payload_schemas.failed_jobs = %d, want the command's %d", got, failedSchema)
	}
	// A package that reports the versions this service reads withholds nothing.
	for _, operation := range []string{"inspect_queues", "failed_jobs_list"} {
		if !answer.Operations[operation] {
			t.Errorf("operations[%q] = false although the application reports a payload this service reads", operation)
		}
	}

	// The staleness field: the launch read is the first answer, so health already says when the
	// loop last answered, and the scrape carries the same timestamp (docs/adr/0014).
	healthNow, err := h.fetchReport(base)
	if err != nil {
		t.Fatalf("read health: %v", err)
	}
	if age := time.Now().Unix() - healthNow.Capabilities.LastReadAt; age < 0 || age > 120 {
		t.Errorf("last_read_at = %d, want the launch read's time (age %ds)", healthNow.Capabilities.LastReadAt, age)
	}
	scrape, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer scrape.Body.Close()
	body, err := io.ReadAll(scrape.Body)
	if err != nil {
		t.Fatalf("read the scrape: %v", err)
	}
	if got := lastSuccessTimestamp(t, body); got != healthNow.Capabilities.LastReadAt {
		t.Errorf("gohorizon_capabilities_last_success_timestamp_seconds = %d, want health's %d",
			got, healthNow.Capabilities.LastReadAt)
	}

	// Drivers and reservation windows, per connection. A null retry_after is a driver with no
	// window and must stay null instead of becoming a zero-second window.
	if len(answer.Connections) != len(raw.Queue.Connections) {
		t.Fatalf("connections = %d, want the application's %d", len(answer.Connections), len(raw.Queue.Connections))
	}
	var windows, silent int
	for name, want := range raw.Queue.Connections {
		got, ok := answer.Connections[name]
		if !ok {
			t.Errorf("connection %q is missing from the descriptor", name)
			continue
		}
		if got.Driver != want.Driver {
			t.Errorf("connection %q driver = %q, want the application's %q", name, got.Driver, want.Driver)
		}
		switch {
		case want.RetryAfter == nil && got.RetryAfter != nil:
			t.Errorf("connection %q retry_after = %d, want null for a driver with no window", name, *got.RetryAfter)
		case want.RetryAfter != nil && got.RetryAfter == nil:
			t.Errorf("connection %q retry_after = null, want the application's %d", name, *want.RetryAfter)
		case want.RetryAfter != nil && *want.RetryAfter != *got.RetryAfter:
			t.Errorf("connection %q retry_after = %d, want the application's %d", name, *got.RetryAfter, *want.RetryAfter)
		}
		if want.RetryAfter != nil {
			windows++
		} else {
			silent++
		}
	}
	if windows == 0 || silent == 0 {
		t.Errorf("fixture connections report %d windows and %d nulls, want both so the distinction is proven", windows, silent)
	}
	// The fixture pins the window, so a descriptor that stopped reading a number would not
	// pass by agreeing with itself.
	if redis, ok := answer.Connections["redis"]; !ok || redis.RetryAfter == nil || *redis.RetryAfter != 10 {
		t.Errorf("redis = %+v, want the fixture's retry_after 10", redis)
	}

	// The lifecycle events are what the package's Laravel version exposes; the event list is
	// how a renamed event becomes visible instead of silently unobserved.
	if len(raw.Events) == 0 {
		t.Error("the application reported no lifecycle events; the fixture no longer proves the field")
	}
	if !slices.Equal(answer.Events, raw.Events) {
		t.Errorf("events = %v, want the application's %v", answer.Events, raw.Events)
	}

	// The descriptor answered, so every operation that runs the package's commands is offered,
	// and restarting a worker stays the supervisor's own action.
	wantOperations := map[string]bool{
		"inspect_queues":    true,
		"telemetry":         true,
		"failed_jobs_list":  true,
		"failed_job_retry":  true,
		"failed_job_delete": true,
		"restart_workers":   true,
	}
	if fmt.Sprint(answer.Operations) != fmt.Sprint(wantOperations) {
		t.Errorf("operations = %v, want %v", answer.Operations, wantOperations)
	}
}

// TestCapabilitiesReportTheApplicationsRefusal runs the real binary against an application
// that cannot answer the capabilities probe. The deployment must still supervise its workers
// and present itself as read-only: nothing published, no operation offered, and the probe's
// own reason instead of an invented one (docs/adr/0014).
func TestCapabilitiesReportTheApplicationsRefusal(t *testing.T) {
	h := newHarness(t)
	const reason = "the gohorizon/laravel package is not installed in this deployment"

	configPath, base := h.writeConfigWithPHP(h.refusingPHP(reason))
	inst := h.startBinary(h.buildBinary(), configPath)
	// The refused preflight skips the launch checks; supervision itself must still come up,
	// which is what makes the deployment repairable rather than failed.
	awaitRunning(h, base)

	answer := h.fetchCapabilities(t, base)

	if answer.Available {
		t.Error("available = true, want false when the probe was refused")
	}
	if !strings.Contains(answer.Error, reason) {
		t.Errorf("error = %q, want the probe's own reason %q", answer.Error, reason)
	}
	if !strings.Contains(answer.Error, "unavailable") {
		t.Errorf("error = %q, want it to say the adapter is unavailable", answer.Error)
	}

	// Nothing from the refused probe may leak into the descriptor: half-read identifiers read
	// exactly like an answer to a client that only looks at the fields.
	if answer.Schema != 0 || answer.Package != "" || answer.Application != "" ||
		len(answer.Connections) != 0 || len(answer.Events) != 0 {
		t.Errorf("descriptor published a refused answer: schema=%d package=%q application=%q connections=%d events=%d",
			answer.Schema, answer.Package, answer.Application, len(answer.Connections), len(answer.Events))
	}

	// Every operation is named and off, so a key that disappeared cannot pass as a refusal.
	wantOperations := map[string]bool{
		"inspect_queues":    false,
		"telemetry":         false,
		"failed_jobs_list":  false,
		"failed_job_retry":  false,
		"failed_job_delete": false,
		"restart_workers":   false,
	}
	if fmt.Sprint(answer.Operations) != fmt.Sprint(wantOperations) {
		t.Errorf("operations = %v, want %v", answer.Operations, wantOperations)
	}

	// It started read-only, which is a transition in its own right: no re-read will ever
	// compare two answers that differ, so the launch has to leave both the record and the
	// instant health publishes (docs/adr/0014).
	record := inst.auditRecord(t, "capability-change", "started read-only")
	if record["outcome"] != "changed" {
		t.Errorf("outcome = %v, want %q", record["outcome"], "changed")
	}
	if _, ok := record["user"]; ok {
		t.Errorf("record = %v, want no user: the service found the deployment this way", record)
	}
	if detail := fmt.Sprint(record["detail"]); !strings.Contains(detail, reason) {
		t.Errorf("detail = %q, want the refusal's own reason", detail)
	}
	report, err := h.fetchReport(base)
	if err != nil {
		t.Fatalf("read health: %v", err)
	}
	if report.Capabilities.ReadOnlySince == 0 {
		t.Error("read_only_since = 0, want the instant the deployment was found read-only")
	}
	if report.Capabilities.ReadOnlySince > time.Now().Unix() {
		t.Errorf("read_only_since = %d, want no future instant", report.Capabilities.ReadOnlySince)
	}

	// The descriptor publishes the same instant, so the dashboard's banner can say how long the
	// controls have been missing from the payload it already gates them on (docs/adr/0014).
	if answer.ReadOnlySince != report.Capabilities.ReadOnlySince {
		t.Errorf("descriptor read_only_since = %d, want health's %d",
			answer.ReadOnlySince, report.Capabilities.ReadOnlySince)
	}
}

// TestReadOnlySinceIsStampedAtStartupAndClearedByTheRepair is the age end to end: a deployment
// found read-only at launch publishes and records the instant, and a repair while the service
// runs clears both. An operator asking "how long has this been read-only" reads a number
// instead of searching the log for a warning (docs/adr/0014).
func TestReadOnlySinceIsStampedAtStartupAndClearedByTheRepair(t *testing.T) {
	h := newHarness(t)
	answer := filepath.Join(h.dir, "capabilities.json")

	// The command answers something this service cannot read, so the launch probe fails where a
	// missing package does: no usable descriptor.
	if err := os.WriteFile(answer, []byte("not the documented json"), 0o644); err != nil {
		t.Fatalf("write the application's answer: %v", err)
	}

	configPath, base := h.writeConfigWithPHP(h.fileAnsweringPHP(answer))
	inst := h.startBinary(h.buildBinary(), configPath)
	awaitRunning(h, base)

	report, err := h.fetchReport(base)
	if err != nil {
		t.Fatalf("read health: %v", err)
	}
	if report.Capabilities.Available {
		t.Fatal("available = true, want a launch that could not read the descriptor")
	}
	if report.Capabilities.ReadOnlySince == 0 {
		t.Fatal("read_only_since = 0, want the instant the deployment was found read-only")
	}
	if report.Capabilities.ReadOnlySince > time.Now().Unix() {
		t.Errorf("read_only_since = %d, want no future instant", report.Capabilities.ReadOnlySince)
	}
	inst.auditRecord(t, "capability-change", "started read-only")

	// The repair: an answer this service can read, which is all the read-only state needs to end.
	if err := os.WriteFile(answer, []byte(capabilitiesAnswerFor(90, "0.1.0")), 0o644); err != nil {
		t.Fatalf("rewrite the application's answer: %v", err)
	}

	eventually(t, 60*time.Second, "the repaired descriptor in health", func() error {
		payload, err := h.fetchReport(base)
		if err != nil {
			return err
		}
		if !payload.Capabilities.Available {
			return fmt.Errorf("available = false (%q), want the readable answer", payload.Capabilities.Error)
		}
		if payload.Capabilities.ReadOnlySince != 0 {
			return fmt.Errorf("read_only_since = %d, want it cleared by the repair", payload.Capabilities.ReadOnlySince)
		}
		return nil
	})
	inst.auditRecord(t, "capability-change", "readable again")
}

// TestCapabilitiesWithholdWhatTheDescriptorCannotSupport runs the real binary against an
// application that answers schema 1 but reports no telemetry stream. The descriptor is readable,
// so the deployment is not read-only; only the screen the payload cannot fill is withheld, and
// the reason names the fact that withheld it rather than the rule.
func TestCapabilitiesWithholdWhatTheDescriptorCannotSupport(t *testing.T) {
	h := newHarness(t)
	payload := `{"schema":1,"package":"gohorizon/laravel","laravel":"13.0.0","php":"8.4.0",` +
		`"queue":{"connections":{"redis":{"driver":"redis","retry_after":90}}},` +
		`"runtime":{"pcntl":true},"telemetry":{"stream":""},"events":[]}`

	configPath, base := h.writeConfigWithPHP(h.answeringPHP(payload))
	h.startBinary(h.buildBinary(), configPath)
	awaitRunning(h, base)

	answer := h.fetchCapabilities(t, base)
	if !answer.Available {
		t.Fatalf("available = false (%q), want the descriptor the application answered", answer.Error)
	}

	if answer.Operations["telemetry"] {
		t.Error("operations[telemetry] = true although the application reports no stream")
	}
	if reason := answer.OperationReasons["telemetry"]; !strings.Contains(reason, "no telemetry stream") {
		t.Errorf("operation_reasons[telemetry] = %q, want the missing stream named", reason)
	}

	// Everything the payload does support is still offered: withholding one screen is not the
	// read-only state, and must not be published as one.
	for _, operation := range []string{"inspect_queues", "failed_jobs_list", "failed_job_retry", "failed_job_delete", "restart_workers"} {
		if !answer.Operations[operation] {
			t.Errorf("operations[%q] = false, want what a readable descriptor offers", operation)
		}
		if reason, ok := answer.OperationReasons[operation]; ok {
			t.Errorf("operation_reasons[%q] = %q, want no reason for an offered operation", operation, reason)
		}
	}
}

// TestCapabilitiesWithholdOnlyThePayloadItCannotRead runs the real binary against an application
// that reports an inspect payload one version ahead of this service. The descriptor is readable
// and the failed store is untouched, so only queue depth is withheld, with both numbers in the
// reason a caller reads.
func TestCapabilitiesWithholdOnlyThePayloadItCannotRead(t *testing.T) {
	h := newHarness(t)
	payload := `{"schema":1,"package":"gohorizon/laravel","laravel":"13.0.0","php":"8.4.0",` +
		`"queue":{"connections":{"redis":{"driver":"redis","retry_after":90}}},` +
		`"runtime":{"pcntl":true},"telemetry":{"stream":"gohorizon:events"},"events":[],` +
		`"payload_schemas":{"inspect":2,"failed_jobs":1}}`

	configPath, base := h.writeConfigWithPHP(h.answeringPHP(payload))
	h.startBinary(h.buildBinary(), configPath)
	awaitRunning(h, base)

	answer := h.fetchCapabilities(t, base)
	if !answer.Available {
		t.Fatalf("available = false (%q), want the descriptor the application answered", answer.Error)
	}

	if answer.Operations["inspect_queues"] {
		t.Error("operations[inspect_queues] = true although the application emits a payload this service cannot read")
	}
	reason := answer.OperationReasons["inspect_queues"]
	for _, want := range []string{"schema 2", "schema 1"} {
		if !strings.Contains(reason, want) {
			t.Errorf("operation_reasons[inspect_queues] = %q, want it to name %q", reason, want)
		}
	}

	// Only the affected operation: the failed list is a different payload, and the deployment
	// is not read-only.
	for _, operation := range []string{"failed_jobs_list", "failed_job_retry", "failed_job_delete", "restart_workers"} {
		if !answer.Operations[operation] {
			t.Errorf("operations[%q] = false, want only the affected operation withheld", operation)
		}
	}

	// Both sides of the comparison are published, so a client can see which one is ahead.
	if answer.ApplicationPayloadSchemas == nil || answer.ApplicationPayloadSchemas.Inspect != 2 {
		t.Errorf("application_payload_schemas = %+v, want the application's inspect 2", answer.ApplicationPayloadSchemas)
	}
	if answer.PayloadSchemas.Inspect != 1 {
		t.Errorf("payload_schemas.inspect = %d, want the version this service reads", answer.PayloadSchemas.Inspect)
	}

	// The loop does not start the command to fail decoding on every tick: health carries the
	// descriptor's reason and no depth, and the status stays ok because the pool is supervised.
	eventually(t, 10*time.Second, "the withheld queue depth in health", func() error {
		payload, err := h.fetchReport(base)
		if err != nil {
			return err
		}
		if payload.Status != "ok" {
			return fmt.Errorf("status = %q, want ok", payload.Status)
		}
		for _, want := range []string{"schema 2", "schema 1"} {
			if !strings.Contains(payload.Queues.Error, want) {
				return fmt.Errorf("queues.error = %q, want it to name %q", payload.Queues.Error, want)
			}
		}
		if len(payload.Queues.Depths) != 0 {
			return fmt.Errorf("depths = %+v, want none for a refused read", payload.Queues.Depths)
		}
		return nil
	})
}

// TestFailedListRefusesAPayloadItCannotRead runs the real binary against an application that
// reports a failed-list payload one version ahead: the endpoint answers with that reason instead
// of starting the command, so a package upgrade is a sentence rather than a decode failure per
// request.
func TestFailedListRefusesAPayloadItCannotRead(t *testing.T) {
	h := newHarness(t)
	payload := `{"schema":1,"package":"gohorizon/laravel","laravel":"13.0.0","php":"8.4.0",` +
		`"queue":{"connections":{"redis":{"driver":"redis","retry_after":90}}},` +
		`"runtime":{"pcntl":true},"telemetry":{"stream":"gohorizon:events"},"events":[],` +
		`"payload_schemas":{"inspect":1,"failed_jobs":2}}`

	configPath, base := h.writeConfigWithPHP(h.answeringPHP(payload))
	h.startBinary(h.buildBinary(), configPath)
	awaitRunning(h, base)

	resp, err := http.Get(base + "/api/v1/failed")
	if err != nil {
		t.Fatalf("GET /api/v1/failed: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the failed-list body: %v", err)
	}

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", resp.StatusCode, body)
	}
	for _, want := range []string{"schema 2", "schema 1"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("body = %s, want it to name %q", body, want)
		}
	}
}

// refusedJobID is an identity the application never stored. The refusal an unreadable descriptor
// produces comes before the application is asked, so any well-formed id proves the same thing.
const refusedJobID = "e0a1c2d3-4b5f-4a6b-8c7d-9e0f1a2b3c4d"

// TestCapabilitiesReportASchemaThisServiceDoesNotUnderstand runs the real binary against an
// application that answers the probe with a schema this service cannot read. The payload is
// well-formed and deliberately contradicts the worker launch (no pcntl, a `--timeout`
// configured), so the test also proves the preflight was skipped rather than guessed from: a
// service that read the fields would refuse to start, and one that ignored the schema would
// publish them.
func TestCapabilitiesReportASchemaThisServiceDoesNotUnderstand(t *testing.T) {
	h := newHarness(t)
	payload := `{"schema":2,"package":"gohorizon/laravel","laravel":"13.0.0","php":"8.4.0",` +
		`"queue":{"default_connection":"redis","connections":{"redis":{"driver":"redis",` +
		`"retry_after":10}}},"runtime":{"pcntl":false},"telemetry":{"stream":"gohorizon:events"},` +
		`"events":[]}`

	// The actions the dashboard drops are only refused by the service if the deployment has
	// users at all: without them there are no administrative routes to refuse (docs/adr/0011).
	// So this is a real signed-in deployment, not an open one.
	hash, err := auth.HashPassword(testOperatorPassword)
	if err != nil {
		t.Fatalf("hash the test password: %v", err)
	}
	configPath, base := h.writeConfigWithAuth(h.answeringPHP(payload), hash)
	inst := h.startBinary(h.buildBinary(), configPath)
	cookies := h.signIn(t, base, testOperator, testOperatorPassword)
	// Supervision comes up although the launch checks could not run, which is what keeps the
	// deployment repairable instead of an unexplained failed start.
	running := awaitRunning(h, base, cookies...)

	// The skipped preflight is not in the descriptor alone: an operator reading the log has
	// to see why the launch checks were abandoned, and which side to upgrade. Both schema
	// numbers are the reason, so a warning that names one of them is not enough.
	warning, warningErr := inst.startupWarning(t, "capabilities schema not understood")
	if warningErr != nil {
		t.Fatalf("read the startup warning: %v", warningErr)
	}
	schema, schemaOK := warning["schema"].(float64)
	understood, understoodOK := warning["understood"].(float64)
	if !schemaOK || schema != 2 {
		t.Errorf("warning schema = %v, want the application's 2", warning["schema"])
	}
	if !understoodOK || understood != 1 {
		t.Errorf("warning understood = %v, want this service's 1", warning["understood"])
	}
	if !strings.Contains(fmt.Sprint(warning["msg"]), "preflight skipped") {
		t.Errorf("warning msg = %v, want it to say the preflight was skipped", warning["msg"])
	}

	answer := h.fetchCapabilities(t, base, cookies...)

	if answer.Available {
		t.Error("available = true, want false for a schema this service does not understand")
	}
	// The reason has to name both versions, or an operator cannot tell which side to upgrade.
	if !strings.Contains(answer.Error, "schema 2") || !strings.Contains(answer.Error, "schema 1") {
		t.Errorf("error = %q, want it to name the application's schema 2 and this service's schema 1", answer.Error)
	}

	// Nothing the payload claims may be published: read through, those fields would look like a
	// descriptor this version understands.
	if answer.Schema != 0 || answer.Package != "" || answer.PCNTL ||
		len(answer.Connections) != 0 || len(answer.Events) != 0 {
		t.Errorf("descriptor published an unread schema: schema=%d package=%q pcntl=%t connections=%d events=%d",
			answer.Schema, answer.Package, answer.PCNTL, len(answer.Connections), len(answer.Events))
	}

	wantOperations := map[string]bool{
		"inspect_queues":    false,
		"telemetry":         false,
		"failed_jobs_list":  false,
		"failed_job_retry":  false,
		"failed_job_delete": false,
		"restart_workers":   false,
	}
	if fmt.Sprint(answer.Operations) != fmt.Sprint(wantOperations) {
		t.Errorf("operations = %v, want %v", answer.Operations, wantOperations)
	}

	// The page is not the only thing between an operator and an action this deployment cannot
	// perform: every control it stopped drawing is refused by the service too, in the words the
	// descriptor publishes. A job id the application never stored is enough, because the refusal
	// comes before anything is asked of the application.
	worker := running.supervisor(t).Workers[0]
	refused := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/v1/workers/" + worker.UUID + "/restart"},
		{http.MethodPost, "/api/v1/failed/" + refusedJobID + "/retry"},
		{http.MethodDelete, "/api/v1/failed/" + refusedJobID},
	}
	for _, action := range refused {
		status, body := h.call(t, base, action.method, action.path, cookies)
		if status != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503 (body %s)", action.method, action.path, status, body)
			continue
		}
		for _, want := range []string{"schema 2", "schema 1"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s %s = %s, want the refusal to name %q", action.method, action.path, body, want)
			}
		}
	}

	// A refusal is not an action that happened and was explained: the worker the restart named
	// is still the one running, so the guard answered rather than the supervisor.
	after, err := h.fetchReport(base, cookies...)
	if err != nil {
		t.Fatalf("read the report after the refused actions: %v", err)
	}
	if got := after.supervisor(t).Workers[0].UUID; got != worker.UUID {
		t.Errorf("worker = %q, want %q still running: the refused restart recycled it", got, worker.UUID)
	}

	// The same state on the report, which is what a monitor reads instead of the log.
	if after.Capabilities.Available {
		t.Error("health reports the deployment as available, want the read-only state")
	}
	for _, want := range []string{"schema 2", "schema 1"} {
		if !strings.Contains(after.Capabilities.Error, want) {
			t.Errorf("health capabilities error = %q, want it to name %q", after.Capabilities.Error, want)
		}
	}
}

// TestCapabilitiesRefreshConfirmsARepairWithoutWaitingForTheTick is the operator's shortcut: the
// deployment is repaired while the service runs, and an administrator asks for a re-read instead
// of waiting out the thirty-second interval or restarting the supervisor. The answer is the
// document that read produced, so the repair is confirmed by the call itself, and the record
// names who asked and what the read found (docs/adr/0014).
func TestCapabilitiesRefreshConfirmsARepairWithoutWaitingForTheTick(t *testing.T) {
	h := newHarness(t)
	answerPath := filepath.Join(h.dir, "capabilities.json")

	// The launch found a deployment it cannot read, which is what a missing or broken package
	// looks like: the service supervises the pool and presents itself as read-only.
	if err := os.WriteFile(answerPath, []byte("not the documented json"), 0o644); err != nil {
		t.Fatalf("write the application's answer: %v", err)
	}
	hash, err := auth.HashPassword(testOperatorPassword)
	if err != nil {
		t.Fatalf("hash the test password: %v", err)
	}
	configPath, base := h.writeConfigWithRole(h.fileAnsweringPHP(answerPath), hash, auth.RoleAdministrator)
	inst := h.startBinary(h.buildBinary(), configPath)
	cookies := h.signIn(t, base, testOperator, testOperatorPassword)
	awaitRunning(h, base, cookies...)

	before := h.fetchCapabilities(t, base, cookies...)
	if before.Available {
		t.Fatalf("available = true at launch, want a deployment that could not be read")
	}

	// The repair. Nothing else changes: the same executable now answers a payload this service
	// reads, and only a re-read can notice.
	if err := os.WriteFile(answerPath, []byte(capabilitiesAnswerFor(90, "0.1.0")), 0o644); err != nil {
		t.Fatalf("rewrite the application's answer: %v", err)
	}

	status, body := h.call(t, base, http.MethodPost, "/api/v1/capabilities/refresh", cookies)
	if status != http.StatusOK {
		t.Fatalf("POST /api/v1/capabilities/refresh = %d, want 200 (body %s)", status, body)
	}
	var refreshed capabilitiesAnswer
	if err := json.Unmarshal([]byte(body), &refreshed); err != nil {
		t.Fatalf("decode the refresh answer: %v (%s)", err, body)
	}
	if !refreshed.Available || refreshed.Error != "" {
		t.Fatalf("available=%t error=%q, want the repaired descriptor in the refresh's own answer",
			refreshed.Available, refreshed.Error)
	}
	if refreshed.Package != "gohorizon/laravel" || refreshed.Laravel == "" || refreshed.PHP == "" {
		t.Errorf("descriptor = %+v, want the application's identifiers", refreshed)
	}
	if refreshed.ReadOnlySince != 0 {
		t.Errorf("read_only_since = %d, want it cleared by the repair", refreshed.ReadOnlySince)
	}
	for _, operation := range []string{"inspect_queues", "telemetry", "failed_jobs_list", "failed_job_retry", "failed_job_delete", "restart_workers"} {
		if !refreshed.Operations[operation] {
			t.Errorf("operations[%q] = false, want the controls back with the readable descriptor", operation)
		}
	}

	// The published state moved with it, so a client that reads next sees the same deployment
	// rather than a refresh that answered and changed nothing.
	published := h.fetchCapabilities(t, base, cookies...)
	if !published.Available || published.ReadOnlySince != 0 || published.Package != refreshed.Package {
		t.Errorf("published = %+v, want the refreshed answer", published)
	}
	report, err := h.fetchReport(base, cookies...)
	if err != nil {
		t.Fatalf("read health after the refresh: %v", err)
	}
	if !report.Capabilities.Available || report.Capabilities.ReadOnlySince != 0 {
		t.Errorf("health capabilities = %+v, want readable with no read-only instant", report.Capabilities)
	}

	// And the call is in the log, with its actor and what the read found.
	record := inst.auditRecord(t, "capability-refresh", "readable")
	if record["outcome"] != "allowed" {
		t.Errorf("outcome = %v, want allowed", record["outcome"])
	}
	if record["user"] != testOperator || record["role"] != auth.RoleAdministrator {
		t.Errorf("record = %v, want %s as administrator", record, testOperator)
	}
}

// descriptorDifferences lists every path at which two decoded JSON documents disagree, so a
// field the re-read loses is named — `descriptor.connections.redis.retry_after: absent, want
// 10` — instead of the whole documents being reported as different.
func descriptorDifferences(path string, want, got any) []string {
	wantMap, wantIsMap := want.(map[string]any)
	gotMap, gotIsMap := got.(map[string]any)
	if wantIsMap || gotIsMap {
		if !wantIsMap || !gotIsMap {
			return []string{fmt.Sprintf("%s: got %v, want %v", path, got, want)}
		}
		keys := make([]string, 0, len(wantMap)+len(gotMap))
		for key := range wantMap {
			keys = append(keys, key)
		}
		for key := range gotMap {
			if _, ok := wantMap[key]; !ok {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)

		var diffs []string
		for _, key := range keys {
			wantValue, inWant := wantMap[key]
			gotValue, inGot := gotMap[key]
			switch {
			case !inWant:
				diffs = append(diffs, fmt.Sprintf("%s.%s: got %v, want the field absent", path, key, gotValue))
			case !inGot:
				diffs = append(diffs, fmt.Sprintf("%s.%s: absent, want %v", path, key, wantValue))
			default:
				diffs = append(diffs, descriptorDifferences(path+"."+key, wantValue, gotValue)...)
			}
		}
		return diffs
	}

	wantList, wantIsList := want.([]any)
	gotList, gotIsList := got.([]any)
	if wantIsList || gotIsList {
		if !wantIsList || !gotIsList {
			return []string{fmt.Sprintf("%s: got %v, want %v", path, got, want)}
		}
		var diffs []string
		if len(wantList) != len(gotList) {
			diffs = append(diffs, fmt.Sprintf("%s: got %d entries, want %d (%v)", path, len(gotList), len(wantList), wantList))
		}
		for i := 0; i < len(wantList) && i < len(gotList); i++ {
			diffs = append(diffs, descriptorDifferences(fmt.Sprintf("%s[%d]", path, i), wantList[i], gotList[i])...)
		}
		return diffs
	}

	if want != got {
		return []string{fmt.Sprintf("%s: got %v, want %v", path, got, want)}
	}
	return nil
}

// TestCapabilitiesRefreshPublishesTheSameDescriptorAsTheLaunchRead is the real-binary half of
// the descriptor contract. The stub test can only prove that both routes serialize one answer,
// because they share writeDescriptor; this runs the command, reads the launch document, asks for
// a forced re-read — which runs the application again and rebuilds the descriptor from what it
// answers now — and requires the published document to be the same one, field by field.
//
// Every field the fixture proves is checked against the application's own payload first, so the
// two documents cannot agree by both having lost a field: the connection map with its drivers
// and reservation windows, the telemetry stream, the event list and the operations are all
// required to be there with the application's values before the documents are compared.
func TestCapabilitiesRefreshPublishesTheSameDescriptorAsTheLaunchRead(t *testing.T) {
	h := newHarness(t)
	hash, err := auth.HashPassword(testOperatorPassword)
	if err != nil {
		t.Fatalf("hash the test password: %v", err)
	}
	configPath, base := h.writeConfigWithRole(h.php, hash, auth.RoleAdministrator)
	h.startBinary(h.buildBinary(), configPath)
	cookies := h.signIn(t, base, testOperator, testOperatorPassword)
	awaitRunning(h, base, cookies...)

	// The launch read's document: what a client saw of this deployment before anybody asked for
	// a re-read.
	status, launchBody := h.call(t, base, http.MethodGet, "/api/v1/capabilities", cookies)
	if status != http.StatusOK {
		t.Fatalf("GET /api/v1/capabilities = %d, want 200 (body %s)", status, launchBody)
	}
	status, refreshBody := h.call(t, base, http.MethodPost, "/api/v1/capabilities/refresh", cookies)
	if status != http.StatusOK {
		t.Fatalf("POST /api/v1/capabilities/refresh = %d, want 200 (body %s)", status, refreshBody)
	}

	// The launch document has to carry the application's answer, not merely be the document the
	// re-read happens to repeat.
	var launch capabilitiesAnswer
	if err := json.Unmarshal([]byte(launchBody), &launch); err != nil {
		t.Fatalf("decode the launch document: %v (%s)", err, launchBody)
	}
	if !launch.Available || launch.Error != "" {
		t.Fatalf("available=%t error=%q, want the application's descriptor", launch.Available, launch.Error)
	}
	raw := h.capabilitiesPayload(t)
	identifiers := []struct {
		field string
		got   any
		want  any
	}{
		{"schema", launch.Schema, raw.Schema},
		{"package", launch.Package, raw.Package},
		{"laravel", launch.Laravel, raw.Laravel},
		{"php", launch.PHP, raw.PHP},
		{"application", launch.Application, raw.Application},
		{"environment", launch.Environment, raw.Environment},
		{"telemetry_stream", launch.TelemetryStream, raw.Telemetry.Stream},
		{"pcntl", launch.PCNTL, raw.Runtime.PCNTL},
	}
	for _, id := range identifiers {
		if fmt.Sprint(id.want) == "" {
			t.Errorf("the application reported no %s; the fixture no longer proves the field", id.field)
			continue
		}
		if fmt.Sprint(id.got) != fmt.Sprint(id.want) {
			t.Errorf("launch %s = %v, want the application's %v", id.field, id.got, id.want)
		}
	}

	if len(launch.Connections) != len(raw.Queue.Connections) || len(launch.Connections) == 0 {
		t.Errorf("launch connections = %d, want the application's %d", len(launch.Connections), len(raw.Queue.Connections))
	}
	var windows, silent int
	for name, want := range raw.Queue.Connections {
		got, ok := launch.Connections[name]
		if !ok {
			t.Errorf("connection %q is missing from the launch document", name)
			continue
		}
		if got.Driver != want.Driver {
			t.Errorf("connection %q driver = %q, want the application's %q", name, got.Driver, want.Driver)
		}
		switch {
		case want.RetryAfter == nil && got.RetryAfter != nil:
			t.Errorf("connection %q retry_after = %d, want null for a driver with no window", name, *got.RetryAfter)
		case want.RetryAfter != nil && got.RetryAfter == nil:
			t.Errorf("connection %q retry_after = null, want the application's %d", name, *want.RetryAfter)
		case want.RetryAfter != nil && *want.RetryAfter != *got.RetryAfter:
			t.Errorf("connection %q retry_after = %d, want the application's %d", name, *got.RetryAfter, *want.RetryAfter)
		}
		if want.RetryAfter != nil {
			windows++
		} else {
			silent++
		}
	}
	if windows == 0 || silent == 0 {
		t.Errorf("the fixture reports %d windows and %d nulls, want both so the distinction is proven", windows, silent)
	}
	if len(raw.Events) == 0 {
		t.Error("the application reported no lifecycle events; the fixture no longer proves the field")
	}
	if !slices.Equal(launch.Events, raw.Events) {
		t.Errorf("launch events = %v, want the application's %v", launch.Events, raw.Events)
	}
	if launch.ApplicationPayloadSchemas == nil {
		t.Error("application_payload_schemas is missing, so the application's report is not published")
	}
	if len(launch.Operations) == 0 {
		t.Error("operations is empty, so the document says nothing about what the deployment can do")
	}
	for operation, offered := range launch.Operations {
		if !offered {
			t.Errorf("operations[%q] = false, want every operation on a readable descriptor (reasons %v)",
				operation, launch.OperationReasons)
		}
	}

	// Field by field over the whole document: identifiers, the connection map with its drivers
	// and windows, the telemetry stream, the event list, the payload versions, the operations and
	// their reasons. Equality is deep, so a field only one document has is a difference too.
	var want, got map[string]any
	if err := json.Unmarshal([]byte(launchBody), &want); err != nil {
		t.Fatalf("decode the launch document as a map: %v", err)
	}
	if err := json.Unmarshal([]byte(refreshBody), &got); err != nil {
		t.Fatalf("decode the re-read document as a map: %v", err)
	}
	if diffs := descriptorDifferences("descriptor", want, got); len(diffs) > 0 {
		t.Errorf("the forced re-read published a document the launch read did not:\n%s", strings.Join(diffs, "\n"))
	}
}
