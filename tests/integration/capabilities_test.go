//go:build linux

package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
}

// capabilitiesAnswer is the published descriptor as a client reads it.
type capabilitiesAnswer struct {
	Available       bool   `json:"available"`
	Schema          int    `json:"schema"`
	Package         string `json:"package"`
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

	// The other payloads this service reads are versioned too, and the descriptor publishes the
	// versions it accepts: compare them with what the application's own commands print, so a
	// package that bumps either payload cannot do it without this test naming the mismatch. The
	// decoders refuse the payload at read time with those same two numbers.
	if got, want := answer.PayloadSchemas.Inspect, h.payloadSchema(t, "gohorizon:inspect", "--connection="+testConnection, "--queue="+testQueue); got != want {
		t.Errorf("payload_schemas.inspect = %d, want the application's %d", got, want)
	}
	if got, want := answer.PayloadSchemas.FailedJobs, h.payloadSchema(t, "gohorizon:failed:list", "--limit=1"); got != want {
		t.Errorf("payload_schemas.failed_jobs = %d, want the application's %d", got, want)
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
	h.startBinary(h.buildBinary(), configPath)
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
