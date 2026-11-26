//go:build linux

package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"testing"
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
	Error      string          `json:"error"`
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

func (h *harness) fetchCapabilities(t *testing.T, base string) capabilitiesAnswer {
	t.Helper()

	resp, err := http.Get(base + "/api/v1/capabilities")
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
