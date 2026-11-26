package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/gohorizon/gohorizon/internal/adapter"
)

// descriptor is a capabilities payload as the package documents it, including the facts the
// endpoint publishes but the preflight does not read.
func descriptor() adapter.Capabilities {
	caps := adapter.Capabilities{
		Schema:         adapter.SchemaVersion,
		Package:        "gohorizon/laravel",
		PackageVersion: "0.1.0",
		Laravel:        "13.0.0",
		PHP:            "8.4.28",
		Application:    "production",
		Environment:    "production",
		Events:         []string{"Illuminate\\Queue\\Events\\JobProcessed"},
	}
	caps.Queue.DefaultConnection = "redis"
	caps.Queue.Connections = map[string]adapter.Connection{
		"redis": {Driver: "redis", RetryAfter: 90},
		"sync":  {Driver: "sync"},
	}
	caps.Runtime.PCNTL = true
	caps.Telemetry.Stream = "gohorizon:production:production:events"
	return caps
}

// capabilitiesAnswer is the payload the endpoint published, decoded into the fields a client
// gates its controls on.
type capabilitiesAnswer struct {
	Available       bool     `json:"available"`
	Schema          int      `json:"schema"`
	Package         string   `json:"package"`
	Application     string   `json:"application"`
	Environment     string   `json:"environment"`
	Laravel         string   `json:"laravel"`
	PCNTL           bool     `json:"pcntl"`
	TelemetryStream string   `json:"telemetry_stream"`
	Events          []string `json:"events"`
	Connections     map[string]struct {
		Driver     string `json:"driver"`
		RetryAfter *int   `json:"retry_after"`
	} `json:"connections"`
	Operations map[string]bool `json:"operations"`
	Error      string          `json:"error"`
}

func readCapabilities(t *testing.T, provider Provider) capabilitiesAnswer {
	t.Helper()
	recorder := get(t, provider, "/api/v1/capabilities")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if cache := recorder.Header().Get("Cache-Control"); cache != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cache)
	}
	var answer capabilitiesAnswer
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode body: %v (%s)", err, recorder.Body)
	}
	return answer
}

func TestCapabilitiesPublishesTheApplicationDescriptor(t *testing.T) {
	answer := readCapabilities(t, &stubProvider{caps: descriptor()})

	if !answer.Available {
		t.Fatalf("available = false, want true; error %q", answer.Error)
	}
	if answer.Schema != adapter.SchemaVersion || answer.Package != "gohorizon/laravel" ||
		answer.Application != "production" || answer.Environment != "production" || answer.Laravel != "13.0.0" {
		t.Errorf("descriptor = %+v, want the application's own identifiers", answer)
	}
	if !answer.PCNTL || answer.TelemetryStream == "" || len(answer.Events) != 1 {
		t.Errorf("runtime/telemetry = %+v, want what the application reported", answer)
	}
	if redis := answer.Connections["redis"]; redis.Driver != "redis" || redis.RetryAfter == nil || *redis.RetryAfter != 90 {
		t.Errorf("redis = %+v, want driver redis and retry_after 90", redis)
	}
	// A driver without a reservation window publishes null, not an invented zero.
	if sync := answer.Connections["sync"]; sync.Driver != "sync" || sync.RetryAfter != nil {
		t.Errorf("sync = %+v, want no retry_after", sync)
	}

	// Every operation is offered when the application understands the request.
	for _, operation := range []string{
		"inspect_queues", "telemetry", "failed_jobs_list",
		"failed_job_retry", "failed_job_delete", "restart_workers",
	} {
		if !answer.Operations[operation] {
			t.Errorf("operations[%q] = false, want true", operation)
		}
	}
}

func TestCapabilitiesOffersNothingWhenTheApplicationCannotAnswer(t *testing.T) {
	answer := readCapabilities(t, &stubProvider{capsErr: errors.New("gohorizon/laravel adapter is unavailable: exit status 1: could not open input file: artisan")})

	if answer.Available {
		t.Error("available = true for an adapter that could not answer")
	}
	if answer.Error == "" {
		t.Error("error is empty, so the dashboard cannot say why the controls are missing")
	}
	// A deployment the service cannot read is read-only as a whole, restart included.
	for _, operation := range []string{
		"inspect_queues", "telemetry", "failed_jobs_list",
		"failed_job_retry", "failed_job_delete", "restart_workers",
	} {
		if answer.Operations[operation] {
			t.Errorf("operations[%q] = true although the application never answered", operation)
		}
	}
}

func TestCapabilitiesRefusesAnUnknownSchema(t *testing.T) {
	caps := descriptor()
	caps.Schema = adapter.SchemaVersion + 1
	answer := readCapabilities(t, &stubProvider{caps: caps})

	if answer.Available {
		t.Error("available = true for a schema this service does not understand")
	}
	if answer.Error == "" {
		t.Error("error is empty, so an operator cannot tell a broken package from an unreadable one")
	}
	// The fields it thinks it recognises are not published from another schema.
	if answer.Package != "" || answer.Application != "" || len(answer.Connections) != 0 {
		t.Errorf("descriptor = %+v, want nothing read from an unknown schema", answer)
	}
	if answer.Operations["failed_job_retry"] {
		t.Error("operations[failed_job_retry] = true, want false for an unknown schema")
	}
}

func TestCapabilitiesIsAReadAnySessionMayMake(t *testing.T) {
	handler, _ := authedRouter(t)

	if code := serve(handler, http.MethodGet, "/api/v1/capabilities", "").Code; code != http.StatusUnauthorized {
		t.Fatalf("status without a session = %d, want 401", code)
	}

	signedIn := serve(handler, http.MethodPost, "/api/v1/session", `{"name":"alice","password":"s3cret"}`)
	if signedIn.Code != http.StatusOK {
		t.Fatalf("sign-in: status = %d, body %s", signedIn.Code, signedIn.Body)
	}
	cookie := sessionCookie(t, signedIn)
	// alice is a viewer, and this is a read: gating the controls must not need a role.
	if code := serve(handler, http.MethodGet, "/api/v1/capabilities", "", cookie).Code; code != http.StatusOK {
		t.Fatalf("status for a signed-in viewer = %d, want 200", code)
	}
}

func TestCapabilitiesIsReadOnly(t *testing.T) {
	provider := &stubProvider{caps: descriptor()}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewRouter(provider, log)

	if code := serve(handler, http.MethodPost, "/api/v1/capabilities", "{}").Code; code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", code)
	}
}
