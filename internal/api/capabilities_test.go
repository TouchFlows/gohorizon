package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gohorizon/gohorizon/internal/adapter"
	"github.com/gohorizon/gohorizon/internal/audit"
	"github.com/gohorizon/gohorizon/internal/auth"
	"github.com/gohorizon/gohorizon/internal/health"
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
	Operations       map[string]bool   `json:"operations"`
	OperationReasons map[string]string `json:"operation_reasons"`
	// ApplicationPayloadSchemas is what the application says its own commands emit.
	ApplicationPayloadSchemas *struct {
		Inspect    int `json:"inspect"`
		FailedJobs int `json:"failed_jobs"`
	} `json:"application_payload_schemas"`
	PayloadSchemas struct {
		Inspect    int `json:"inspect"`
		FailedJobs int `json:"failed_jobs"`
	} `json:"payload_schemas"`
	Error string `json:"error"`
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
		// And every one of them carries the reason, so a client that only reads the per-op
		// map is told as much as one that reads `error`.
		if answer.OperationReasons[operation] != answer.Error {
			t.Errorf("operation_reasons[%q] = %q, want the descriptor's reason %q",
				operation, answer.OperationReasons[operation], answer.Error)
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

// administrativeCall presents the cookies a signed-in browser holds and, when token is not
// empty, the header it echoes: everything the administrative routes ask for before the
// descriptor is consulted.
func administrativeCall(handler http.Handler, method, target, token string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, nil)
	request.RemoteAddr = "192.0.2.10:5555"
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	if token != "" {
		request.Header.Set(auth.CSRFHeader, token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// TestUnreadableCapabilitiesRefuseTheActionsTheDashboardDrops covers the server-side half of the
// gate: a control the dashboard stops drawing is also refused when a caller keeps it. The
// descriptor is the only thing that can refuse here — the same role, token and report answer
// 202/200 when it is readable (admin_test.go) — so this is not a role or session answer in
// disguise, and nothing is attempted on the pool or the application first.
func TestUnreadableCapabilitiesRefuseTheActionsTheDashboardDrops(t *testing.T) {
	caps := descriptor()
	caps.Schema = adapter.SchemaVersion + 1
	provider := &stubProvider{
		caps: caps,
		report: health.Report{
			Status: "ok",
			Supervisors: []health.Supervisor{{
				ID:      "default",
				Running: 1,
				Workers: []health.Worker{{UUID: testWorkerUUID, Queue: "default", State: "running"}},
			}},
		},
	}
	handler, provider, records := adminRouterFor(t, auth.RoleOperator, provider)
	session, csrf := signInForAdmin(t, handler)

	// The refusal is the descriptor's own reason, so a caller learns which side to upgrade
	// rather than only that the action did not happen.
	reason := caps.Reason(nil)
	for _, want := range []string{"schema 2", "schema 1"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("reason = %q, want it to name %q", reason, want)
		}
	}

	cases := []struct {
		action string
		method string
		target string
	}{
		{audit.ActionWorkerRestart, http.MethodPost, "/api/v1/workers/" + testWorkerUUID + "/restart"},
		{audit.ActionFailedJobRetry, http.MethodPost, "/api/v1/failed/" + testJobUUID + "/retry"},
		{audit.ActionFailedJobDelete, http.MethodDelete, "/api/v1/failed/" + testJobUUID},
	}
	for _, tc := range cases {
		recorder := administrativeCall(handler, tc.method, tc.target, csrf.Value, session, csrf)
		if recorder.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503 (body %s)", tc.action, recorder.Code, recorder.Body)
			continue
		}
		if cache := recorder.Header().Get("Cache-Control"); cache != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", tc.action, cache)
		}
		var payload struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Errorf("%s: decode body: %v (%s)", tc.action, err, recorder.Body)
			continue
		}
		if payload.Error != reason {
			t.Errorf("%s: error = %q, want the descriptor's reason %q", tc.action, payload.Error, reason)
		}
	}

	// A refusal is not an action that happened and was then explained: neither the pool nor
	// the application was asked to do anything.
	if len(provider.restarted) != 0 || len(provider.retried) != 0 || len(provider.forgotten) != 0 {
		t.Errorf("refused actions reached the provider: restarted=%v retried=%v forgotten=%v",
			provider.restarted, provider.retried, provider.forgotten)
	}
	if provider.jobID != "" {
		t.Errorf("history was consulted for %q, want the refusal to come before the action", provider.jobID)
	}

	// Each refusal is recorded under the action the caller tried, so the trail says which
	// control a deployment presented as read-only was asked for anyway.
	for _, tc := range cases {
		if !strings.Contains(records.String(), `"action":"`+tc.action+`"`) {
			t.Errorf("no audit record for %s in %s", tc.action, records)
		}
	}
	if got := strings.Count(records.String(), `"outcome":"refused"`); got != len(cases) {
		t.Errorf("refused audit records = %d, want %d", got, len(cases))
	}
}

// TestCapabilitiesWithholdWhatThePayloadCannotSupport is the derivation behind the operations
// map: a readable descriptor is not a promise that every screen has something to show, so an
// application with no telemetry stream and a connection that runs jobs inline withholds exactly
// those two and says why. Everything else stays offered, because withholding one screen must
// not present a readable deployment as read-only (docs/adr/0014).
func TestCapabilitiesWithholdWhatThePayloadCannotSupport(t *testing.T) {
	caps := descriptor()
	caps.Telemetry.Stream = ""
	caps.Queue.Connections = map[string]adapter.Connection{"sync": {Driver: "sync"}}

	answer := readCapabilities(t, &stubProvider{caps: caps})

	if !answer.Available {
		t.Fatalf("available = false (%q), want the descriptor read", answer.Error)
	}
	for _, operation := range []string{"telemetry", "inspect_queues"} {
		if answer.Operations[operation] {
			t.Errorf("operations[%q] = true although the payload cannot support it", operation)
		}
		if answer.OperationReasons[operation] == "" {
			t.Errorf("operation_reasons[%q] is empty, so the control would vanish without saying why", operation)
		}
	}
	if reason := answer.OperationReasons["inspect_queues"]; !strings.Contains(reason, "sync") {
		t.Errorf("operation_reasons[inspect_queues] = %q, want the connection's driver named", reason)
	}
	for _, operation := range []string{"failed_jobs_list", "failed_job_retry", "failed_job_delete", "restart_workers"} {
		if !answer.Operations[operation] {
			t.Errorf("operations[%q] = false, want what a readable descriptor still offers", operation)
		}
	}

	// Every offered operation has no reason and every withheld one has a reason: a client can
	// gate on the map alone and never draw a control it cannot explain.
	for operation, offered := range answer.Operations {
		reason := answer.OperationReasons[operation]
		if offered && reason != "" {
			t.Errorf("operation_reasons[%q] = %q for an offered operation", operation, reason)
		}
		if !offered && reason == "" {
			t.Errorf("operation_reasons[%q] is empty for a withheld operation", operation)
		}
	}
}

// TestCapabilitiesWithholdOnlyThePayloadItCannotRead is the version check the application's own
// report makes possible: a package that bumped one payload withholds that screen, and nothing
// else, so a new inspect payload does not take the failed store away with it.
func TestCapabilitiesWithholdOnlyThePayloadItCannotRead(t *testing.T) {
	cases := []struct {
		name     string
		payloads func(*adapter.Capabilities)
		withheld string
		offered  string
	}{
		{
			name:     "inspect",
			payloads: func(c *adapter.Capabilities) { c.Payloads.Inspect = adapter.InspectionSchema + 1 },
			withheld: "inspect_queues",
			offered:  "failed_jobs_list",
		},
		{
			name:     "failed list",
			payloads: func(c *adapter.Capabilities) { c.Payloads.FailedJobs = adapter.FailedJobsSchema + 1 },
			withheld: "failed_jobs_list",
			offered:  "inspect_queues",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caps := descriptor()
			tc.payloads(&caps)

			answer := readCapabilities(t, &stubProvider{caps: caps})

			if !answer.Available {
				t.Fatalf("available = false (%q), want the descriptor read", answer.Error)
			}
			if answer.Operations[tc.withheld] {
				t.Errorf("operations[%q] = true although the application emits a payload this service cannot read", tc.withheld)
			}
			if !answer.Operations[tc.offered] {
				t.Errorf("operations[%q] = false, want only the affected operation withheld", tc.offered)
			}

			reason := answer.OperationReasons[tc.withheld]
			for _, want := range []string{"schema 2", "schema 1"} {
				if !strings.Contains(reason, want) {
					t.Errorf("operation_reasons[%q] = %q, want it to name %q", tc.withheld, reason, want)
				}
			}

			// The two numbers are published separately too, so a client does not have to parse
			// the sentence to see which side is ahead.
			if answer.ApplicationPayloadSchemas == nil {
				t.Fatal("application_payload_schemas is missing, want what the application reported")
			}
			if answer.ApplicationPayloadSchemas.Inspect != caps.Payloads.Inspect ||
				answer.ApplicationPayloadSchemas.FailedJobs != caps.Payloads.FailedJobs {
				t.Errorf("application_payload_schemas = %+v, want %+v", answer.ApplicationPayloadSchemas, caps.Payloads)
			}
		})
	}
}

// TestCapabilitiesWithholdDepthWhenNoConnectionHasAQueue covers the other half of the rule: a
// payload that names no connections, or only ones that run jobs inline, has no depth to count,
// and the reason says which connections it saw rather than repeating the rule.
func TestCapabilitiesWithholdDepthWhenNoConnectionHasAQueue(t *testing.T) {
	cases := []struct {
		name        string
		connections map[string]adapter.Connection
		want        string
	}{
		{"none", nil, "defines no queue connections"},
		{"sync and null", map[string]adapter.Connection{
			"sync": {Driver: "sync"},
			"null": {Driver: "null"},
		}, "null, sync"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caps := descriptor()
			caps.Queue.Connections = tc.connections

			answer := readCapabilities(t, &stubProvider{caps: caps})

			if answer.Operations["inspect_queues"] {
				t.Error("operations[inspect_queues] = true although no connection has a queue")
			}
			if reason := answer.OperationReasons["inspect_queues"]; !strings.Contains(reason, tc.want) {
				t.Errorf("operation_reasons[inspect_queues] = %q, want it to contain %q", reason, tc.want)
			}
		})
	}
}

// TestCapabilitiesPublishesThePayloadSchemas pins the versions the descriptor advertises for the
// application's other payloads: they are the ones the decoders enforce, published whether or not
// the descriptor itself is usable, so a package upgrade of either payload is visible before it
// is read (docs/adr/0014).
func TestCapabilitiesPublishesThePayloadSchemas(t *testing.T) {
	answer := readCapabilities(t, &stubProvider{caps: descriptor()})

	if answer.PayloadSchemas.Inspect != adapter.InspectionSchema {
		t.Errorf("payload_schemas.inspect = %d, want the version this service reads (%d)",
			answer.PayloadSchemas.Inspect, adapter.InspectionSchema)
	}
	if answer.PayloadSchemas.FailedJobs != adapter.FailedJobsSchema {
		t.Errorf("payload_schemas.failed_jobs = %d, want the version this service reads (%d)",
			answer.PayloadSchemas.FailedJobs, adapter.FailedJobsSchema)
	}

	// An unusable descriptor still says which payloads this service reads: the client comparing
	// them with its application is not helped by learning it only when everything works.
	unavailable := readCapabilities(t, &stubProvider{capsErr: errors.New("the package is not installed")})
	if unavailable.PayloadSchemas.Inspect == 0 || unavailable.PayloadSchemas.FailedJobs == 0 {
		t.Errorf("payload_schemas = %+v, want the versions published with an unavailable descriptor",
			unavailable.PayloadSchemas)
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
