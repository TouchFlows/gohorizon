package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
	Available         bool     `json:"available"`
	Schema            int      `json:"schema"`
	Package           string   `json:"package"`
	PackageVersion    string   `json:"package_version"`
	Application       string   `json:"application"`
	Environment       string   `json:"environment"`
	Laravel           string   `json:"laravel"`
	PHP               string   `json:"php"`
	DefaultConnection string   `json:"default_connection"`
	PCNTL             bool     `json:"pcntl"`
	TelemetryStream   string   `json:"telemetry_stream"`
	Events            []string `json:"events"`
	Connections       map[string]struct {
		Driver     string `json:"driver"`
		RetryAfter *int   `json:"retry_after"`
	} `json:"connections"`
	Operations       map[string]bool   `json:"operations"`
	OperationReasons map[string]string `json:"operation_reasons"`
	// ReadOnlySince is when the deployment went read-only, 0 while it is readable.
	ReadOnlySince int64 `json:"read_only_since"`
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

// TestCapabilitiesPublishesWhenTheDeploymentWentReadOnly is the banner's other half: the error says
// why the controls are missing, and this instant says how long they have been, so the dashboard can
// say "read-only for 12 minutes" from the descriptor alone rather than asking health as well
// (docs/adr/0014).
func TestCapabilitiesPublishesWhenTheDeploymentWentReadOnly(t *testing.T) {
	answer := readCapabilities(t, &stubProvider{
		capsErr:       errors.New("gohorizon/laravel adapter is unavailable: the package is not installed"),
		readOnlySince: 1763600000,
	})

	if answer.Available {
		t.Error("available = true for an adapter that could not answer")
	}
	if answer.ReadOnlySince != 1763600000 {
		t.Errorf("read_only_since = %d, want the instant the service recorded", answer.ReadOnlySince)
	}

	// A readable deployment has not been read-only at all, so the field stays out of the document
	// instead of publishing an instant a banner would count from forever.
	readable := readCapabilities(t, &stubProvider{caps: descriptor()})
	if readable.ReadOnlySince != 0 {
		t.Errorf("read_only_since = %d, want 0 for a readable deployment", readable.ReadOnlySince)
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
			name: "inspect",
			payloads: func(c *adapter.Capabilities) {
				c.Payloads.Inspect = adapter.InspectionSchema + 1
			},
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

// TestCapabilitiesRefreshNeedsAnAdministratorAndTheToken gates the one route that starts PHP on
// demand. A viewer or an operator can read the descriptor, but asking the deployment to account
// for itself again is an administrator's call, and the token is the same double-submit check
// every mutation carries — so a request that would run a command needs both (docs/adr/0011,
// docs/adr/0014).
func TestCapabilitiesRefreshNeedsAnAdministratorAndTheToken(t *testing.T) {
	cases := []struct {
		name string
		role string
		// token picks the header: "cookie" echoes the issued token, "wrong" sends another,
		// "" sends none.
		token string
		want  int
	}{
		{name: "a viewer with the token", role: auth.RoleViewer, token: "cookie", want: http.StatusForbidden},
		{name: "an operator with the token", role: auth.RoleOperator, token: "cookie", want: http.StatusForbidden},
		{name: "an administrator with no token", role: auth.RoleAdministrator, token: "", want: http.StatusForbidden},
		{name: "an administrator with the wrong token", role: auth.RoleAdministrator, token: "wrong", want: http.StatusForbidden},
		{name: "an administrator with the token", role: auth.RoleAdministrator, token: "cookie", want: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := &stubProvider{caps: descriptor(), reprobed: descriptor()}
			handler, _, _ := adminRouterFor(t, tc.role, provider)
			session, csrf := signInForAdmin(t, handler)

			token := ""
			switch tc.token {
			case "cookie":
				token = csrf.Value
			case "wrong":
				token = "not-the-issued-token"
			}
			recorder := administrativeCall(handler, http.MethodPost, "/api/v1/capabilities/refresh", token, session, csrf)

			if recorder.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, tc.want, recorder.Body)
			}
			// Nothing starts a command unless the request passed both checks, so a refusal is
			// proved by the provider never being asked to re-read.
			wantReads := 0
			if tc.want == http.StatusOK {
				wantReads = 1
			}
			if provider.reprobeReads != wantReads {
				t.Errorf("re-reads = %d, want %d", provider.reprobeReads, wantReads)
			}
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
		})
	}
}

// TestCapabilitiesRefreshPublishesTheAnswerItJustRead is the repair's confirmation: the response
// is the document the re-read produced, not the one the endpoint held before, so an administrator
// who has just installed the package reads the controls back in the same call. The record says
// what the read found and who asked, which is the line an operator looks for afterwards
// (docs/adr/0014).
func TestCapabilitiesRefreshPublishesTheAnswerItJustRead(t *testing.T) {
	readable := descriptor()
	provider := &stubProvider{
		caps:     adapter.Capabilities{Schema: adapter.SchemaVersion + 1},
		capsErr:  errors.New("the package is not installed"),
		reprobed: readable,
	}
	handler, _, records := adminRouterFor(t, auth.RoleAdministrator, provider)
	session, csrf := signInForAdmin(t, handler)

	recorder := administrativeCall(handler, http.MethodPost, "/api/v1/capabilities/refresh", csrf.Value, session, csrf)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body)
	}
	var answer capabilitiesAnswer
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode body: %v (%s)", err, recorder.Body)
	}
	if !answer.Available || answer.Error != "" {
		t.Fatalf("available=%t error=%q, want the answer the re-read produced", answer.Available, answer.Error)
	}
	if answer.Package != readable.Package || !answer.Operations["restart_workers"] {
		t.Errorf("descriptor = %+v, want the readable answer with its controls", answer)
	}
	// A deployment that has just become readable has not been read-only at all, so the instant is
	// out of the document rather than counting from the state it left.
	if answer.ReadOnlySince != 0 {
		t.Errorf("read_only_since = %d, want it cleared by the repair", answer.ReadOnlySince)
	}

	events := auditRecords(t, records)
	allowed := events[len(events)-1]
	if allowed["action"] != audit.ActionCapabilityRefresh || allowed["outcome"] != audit.OutcomeAllowed {
		t.Fatalf("record = %v, want an allowed capability-refresh", allowed)
	}
	if allowed["user"] != "alice" || allowed["role"] != auth.RoleAdministrator {
		t.Errorf("record = %v, want alice as administrator", allowed)
	}
	if detail := fmt.Sprint(allowed["detail"]); !strings.Contains(detail, "readable") || !strings.Contains(detail, "0.1.0") {
		t.Errorf("detail = %q, want what the read found", detail)
	}
}

// TestCapabilitiesRefreshServesTheSameDocumentAsTheRead is the contract between the two ways of
// asking one deployment what it supports: the forced re-read answers with the document a GET
// serves for the same read, field for field. The repair notice names the package identity, which
// is only readable if everything else travelled with it — the connections and their drivers, the
// telemetry stream, the events, the operations and the reason for each one withheld — so a field
// the refresh path lost fails here instead of arriving as a control with nothing to gate on
// (docs/adr/0014).
func TestCapabilitiesRefreshServesTheSameDocumentAsTheRead(t *testing.T) {
	// A readable descriptor that still withholds one screen: the operations map is derived from
	// the payload, so the contract covers the reasons as well as the booleans.
	withheld := descriptor()
	withheld.Payloads.Inspect = adapter.InspectionSchema + 1

	cases := []struct {
		name          string
		caps          adapter.Capabilities
		capsErr       error
		readOnlySince int64
	}{
		{name: "a readable descriptor", caps: withheld},
		{
			name:          "a deployment that cannot answer",
			capsErr:       errors.New("gohorizon/laravel adapter is unavailable: could not open input file: artisan"),
			readOnlySince: 1763600000,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := &stubProvider{
				caps:          tc.caps,
				capsErr:       tc.capsErr,
				reprobed:      tc.caps,
				reprobeErr:    tc.capsErr,
				readOnlySince: tc.readOnlySince,
			}
			handler, _, _ := adminRouterFor(t, auth.RoleAdministrator, provider)
			session, csrf := signInForAdmin(t, handler)

			read := administrativeCall(handler, http.MethodGet, "/api/v1/capabilities", "", session)
			refreshed := administrativeCall(handler, http.MethodPost, "/api/v1/capabilities/refresh", csrf.Value, session, csrf)
			if refreshed.Code != http.StatusOK {
				t.Fatalf("refresh status = %d, want 200 (body %s)", refreshed.Code, refreshed.Body)
			}

			// One read has one spelling, so the two answers being the same bytes is every field at
			// once: a field the refresh path dropped, defaulted or invented shows up as a
			// difference rather than as a field nobody thought to assert.
			if !bytes.Equal(read.Body.Bytes(), refreshed.Body.Bytes()) {
				t.Fatalf("the re-read published\n  %s\nwant the document the read serves\n  %s",
					refreshed.Body, read.Body)
			}

			// Bytes can only agree on what was written, so the shape is pinned too: every field this
			// document publishes for this read, and nothing else. `pcntl` is the one runtime fact
			// without omitempty: a deployment that could not answer still says whether the extension
			// is there, which is not a field of the descriptor it failed to read.
			var document map[string]json.RawMessage
			if err := json.Unmarshal(refreshed.Body.Bytes(), &document); err != nil {
				t.Fatalf("decode body: %v (%s)", err, refreshed.Body)
			}
			want := []string{"available", "payload_schemas", "operations", "operation_reasons", "pcntl"}
			if tc.capsErr == nil {
				want = append(want,
					"schema", "package", "package_version", "laravel", "php", "application",
					"environment", "default_connection", "connections", "telemetry_stream",
					"events", "application_payload_schemas")
			} else {
				want = append(want, "read_only_since", "error")
			}
			published := make(map[string]bool, len(want))
			for _, field := range want {
				published[field] = true
				if _, ok := document[field]; !ok {
					t.Errorf("the answer the re-read published has no %q", field)
				}
			}
			for field := range document {
				if !published[field] {
					t.Errorf("the answer the re-read published has an unexpected %q", field)
				}
			}

			var answer capabilitiesAnswer
			if err := json.Unmarshal(refreshed.Body.Bytes(), &answer); err != nil {
				t.Fatalf("decode body: %v (%s)", err, refreshed.Body)
			}

			if tc.capsErr != nil {
				if answer.Available {
					t.Error("available = true for a read that failed")
				}
				if answer.Error != answer.OperationReasons["restart_workers"] {
					t.Errorf("error = %q, want the reason each withheld operation carries (%q)",
						answer.Error, answer.OperationReasons["restart_workers"])
				}
				if answer.ReadOnlySince != tc.readOnlySince {
					t.Errorf("read_only_since = %d, want %d", answer.ReadOnlySince, tc.readOnlySince)
				}
				if len(answer.Operations) != len(capabilityOperationNames) {
					t.Errorf("operations = %v, want one entry per operation", answer.Operations)
				}
				return
			}

			if !answer.Available {
				t.Fatalf("available = false (%q), want the descriptor the read served", answer.Error)
			}
			// The identifiers the repair notice names, and the rest of what the application reported.
			if answer.Schema != tc.caps.Schema || answer.Package != tc.caps.Package ||
				answer.PackageVersion != tc.caps.PackageVersion || answer.Laravel != tc.caps.Laravel ||
				answer.PHP != tc.caps.PHP || answer.Application != tc.caps.Application ||
				answer.Environment != tc.caps.Environment {
				t.Errorf("identifiers = %+v, want what the application reported (%+v)", answer, tc.caps)
			}
			if answer.DefaultConnection != tc.caps.Queue.DefaultConnection {
				t.Errorf("default_connection = %q, want %q", answer.DefaultConnection, tc.caps.Queue.DefaultConnection)
			}
			// Connections travel with their drivers and reservation windows: a lost connection is a
			// queue the dashboard cannot name.
			if len(answer.Connections) != len(tc.caps.Queue.Connections) {
				t.Errorf("connections = %+v, want %d of them", answer.Connections, len(tc.caps.Queue.Connections))
			}
			if redis := answer.Connections["redis"]; redis.Driver != "redis" || redis.RetryAfter == nil || *redis.RetryAfter != 90 {
				t.Errorf("connections[redis] = %+v, want driver redis and retry_after 90", redis)
			}
			if sync := answer.Connections["sync"]; sync.Driver != "sync" || sync.RetryAfter != nil {
				t.Errorf("connections[sync] = %+v, want the driver and no reservation window", sync)
			}
			// Telemetry and events decide whether there is a stream to watch at all.
			if !answer.PCNTL || answer.TelemetryStream != tc.caps.Telemetry.Stream || len(answer.Events) != len(tc.caps.Events) {
				t.Errorf("runtime/telemetry = %+v, want what the application reported", answer)
			}
			// The operations, and the reason the withheld one carries.
			if len(answer.Operations) != len(capabilityOperationNames) {
				t.Errorf("operations = %v, want one entry per operation", answer.Operations)
			}
			for _, operation := range capabilityOperationNames {
				if _, ok := answer.Operations[operation]; !ok {
					t.Errorf("operations has no entry for %q", operation)
				}
			}
			if answer.Operations["inspect_queues"] {
				t.Error("operations[inspect_queues] = true although the application emits a payload this service cannot read")
			}
			if reason := answer.OperationReasons["inspect_queues"]; !strings.Contains(reason, "schema 2") || !strings.Contains(reason, "schema 1") {
				t.Errorf("operation_reasons[inspect_queues] = %q, want both schema numbers", reason)
			}
			if !answer.Operations["failed_jobs_list"] || !answer.Operations["restart_workers"] {
				t.Errorf("operations = %v, want only the affected screen withheld", answer.Operations)
			}
			// Both payload versions: the ones this service reads, and the ones the application claims.
			if answer.PayloadSchemas.Inspect != adapter.InspectionSchema || answer.PayloadSchemas.FailedJobs != adapter.FailedJobsSchema {
				t.Errorf("payload_schemas = %+v, want the versions this service reads", answer.PayloadSchemas)
			}
			if answer.ApplicationPayloadSchemas == nil ||
				answer.ApplicationPayloadSchemas.Inspect != tc.caps.Payloads.Inspect {
				t.Errorf("application_payload_schemas = %+v, want what the application reported (%+v)",
					answer.ApplicationPayloadSchemas, tc.caps.Payloads)
			}
		})
	}
}

// TestCapabilitiesRefreshReportsAReadThatStillFails is the other answer: the read happened and the
// application still cannot describe itself. That is a 200 with the reason — the caller asked what
// the deployment is now — and the record says the repair is not confirmed yet, so nobody reads a
// refresh that changed nothing as a refresh that worked (docs/adr/0014).
func TestCapabilitiesRefreshReportsAReadThatStillFails(t *testing.T) {
	const reason = "gohorizon/laravel adapter is unavailable: could not open input file: artisan"
	provider := &stubProvider{caps: descriptor(), reprobeErr: errors.New(reason)}
	handler, _, records := adminRouterFor(t, auth.RoleAdministrator, provider)
	session, csrf := signInForAdmin(t, handler)

	recorder := administrativeCall(handler, http.MethodPost, "/api/v1/capabilities/refresh", csrf.Value, session, csrf)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: the read happened, and this is what it found (body %s)", recorder.Code, recorder.Body)
	}
	var answer capabilitiesAnswer
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode body: %v (%s)", err, recorder.Body)
	}
	if answer.Available {
		t.Error("available = true for a re-read that failed")
	}
	if !strings.Contains(answer.Error, "artisan") {
		t.Errorf("error = %q, want the read's own reason", answer.Error)
	}
	if answer.Operations["restart_workers"] {
		t.Error("operations[restart_workers] = true for a deployment that cannot describe itself")
	}

	events := auditRecords(t, records)
	allowed := events[len(events)-1]
	if allowed["action"] != audit.ActionCapabilityRefresh || allowed["outcome"] != audit.OutcomeAllowed {
		t.Fatalf("record = %v, want an allowed capability-refresh: the read was allowed and performed", allowed)
	}
	if detail := fmt.Sprint(allowed["detail"]); !strings.Contains(detail, "still read-only") {
		t.Errorf("detail = %q, want it to say the deployment is still read-only", detail)
	}
}
