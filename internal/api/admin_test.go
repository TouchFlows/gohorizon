package api

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gohorizon/gohorizon/internal/auth"
	"github.com/gohorizon/gohorizon/internal/health"
)

const testWorkerUUID = "7c9e6679-7425-40de-944b-e07fc1f90ae7"

// adminRouter builds a router with one user in role, keeping the service log so the audit
// records can be read, and reporting one running worker a test can ask to restart.
func adminRouter(t *testing.T, role string) (http.Handler, *stubProvider, *bytes.Buffer) {
	t.Helper()
	return adminRouterWith(t, role, health.Report{
		Status: "ok",
		Supervisors: []health.Supervisor{{
			ID:      "default",
			Running: 1,
			Workers: []health.Worker{{UUID: testWorkerUUID, Queue: "default", State: "running"}},
		}},
	})
}

func adminRouterWith(t *testing.T, role string, report health.Report) (http.Handler, *stubProvider, *bytes.Buffer) {
	t.Helper()
	hash, err := auth.HashPassword("s3cret")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	store, err := auth.New(auth.Options{
		Users: map[string]auth.User{"alice": {PasswordHash: hash, Role: role}},
		Key:   []byte("0123456789abcdef0123456789abcdef"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	records := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(records, nil))
	provider := &stubProvider{report: report}
	return NewRouter(provider, log, WithAuth(store)), provider, records
}

// signInForAdmin signs alice in and returns the two cookies that matter to a mutation: the
// session, and the token it has to echo.
func signInForAdmin(t *testing.T, handler http.Handler) (*http.Cookie, *http.Cookie) {
	t.Helper()
	recorder := serve(handler, http.MethodPost, "/api/v1/session", `{"name":"alice","password":"s3cret"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("sign-in: status = %d, body %s", recorder.Code, recorder.Body)
	}

	session := sessionCookie(t, recorder)
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name != auth.CSRFCookie {
			continue
		}
		// The bundle has to read this one, so it cannot be HttpOnly — and it has to be
		// SameSite, so another site cannot send it either.
		if cookie.HttpOnly {
			t.Error("the CSRF cookie is HttpOnly; the dashboard could not echo it")
		}
		if cookie.SameSite != http.SameSiteLaxMode {
			t.Errorf("CSRF cookie SameSite = %v, want Lax", cookie.SameSite)
		}
		return session, cookie
	}
	t.Fatalf("sign-in set no %s cookie", auth.CSRFCookie)
	return nil, nil
}

// restart asks for one worker's restart, with the cookies a browser holds and, when token is
// not empty, the header it echoes.
func restart(handler http.Handler, uuid string, token string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/workers/"+uuid+"/restart", nil)
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

func TestRestartNeedsTheTokenAndAnOperatorRole(t *testing.T) {
	cases := []struct {
		name string
		role string
		// token picks the header: "cookie" echoes the issued token, "wrong" sends another,
		// "" sends none.
		token string
		want  int
	}{
		{name: "a viewer with the token", role: auth.RoleViewer, token: "cookie", want: http.StatusForbidden},
		{name: "an operator with no token", role: auth.RoleOperator, token: "", want: http.StatusForbidden},
		{name: "an operator with the wrong token", role: auth.RoleOperator, token: "wrong", want: http.StatusForbidden},
		{name: "an operator with the token", role: auth.RoleOperator, token: "cookie", want: http.StatusAccepted},
		{name: "an administrator with the token", role: auth.RoleAdministrator, token: "cookie", want: http.StatusAccepted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler, provider, _ := adminRouter(t, tc.role)
			session, csrf := signInForAdmin(t, handler)

			token := ""
			switch tc.token {
			case "cookie":
				token = csrf.Value
			case "wrong":
				token = "not-the-issued-token"
			}
			recorder := restart(handler, testWorkerUUID, token, session, csrf)

			if recorder.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, tc.want, recorder.Body)
			}
			// Nothing reaches the pool unless the request passed both checks, so a refused
			// call is proved by the worker never being asked to stop.
			wantRestarts := 0
			if tc.want == http.StatusAccepted {
				wantRestarts = 1
			}
			if len(provider.restarted) != wantRestarts {
				t.Errorf("restarted = %v, want %d restart(s)", provider.restarted, wantRestarts)
			}
			if wantRestarts == 1 && provider.restarted[0] != testWorkerUUID {
				t.Errorf("restarted %q, want %q", provider.restarted[0], testWorkerUUID)
			}
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
		})
	}
}

func TestRestartWithoutASessionIsUnauthorized(t *testing.T) {
	handler, provider, _ := adminRouter(t, auth.RoleAdministrator)

	recorder := restart(handler, testWorkerUUID, "anything")
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
	if len(provider.restarted) != 0 {
		t.Fatalf("a session-less call restarted %v", provider.restarted)
	}
}

func TestRestartRefusalsAreAuditedWithTheirReason(t *testing.T) {
	handler, _, records := adminRouter(t, auth.RoleViewer)
	session, csrf := signInForAdmin(t, handler)

	if recorder := restart(handler, testWorkerUUID, csrf.Value, session, csrf); recorder.Code != http.StatusForbidden {
		t.Fatalf("a viewer's restart: status = %d, want 403", recorder.Code)
	}

	events := auditRecords(t, records)
	refused := events[len(events)-1]
	if refused["action"] != "worker-restart" || refused["outcome"] != "refused" {
		t.Fatalf("record = %v, want a refused worker-restart", refused)
	}
	// The record names the actor, the role that was too low, and why — which is the answer
	// to "why did nothing happen".
	if refused["user"] != "alice" || refused["role"] != auth.RoleViewer {
		t.Errorf("record = %v, want alice as viewer", refused)
	}
	if refused["detail"] != auth.RoleOperator+" role required" {
		t.Errorf("detail = %v, want the required role", refused["detail"])
	}
	if refused["path"] != "/api/v1/workers/"+testWorkerUUID+"/restart" {
		t.Errorf("path = %v, want the path that was refused", refused["path"])
	}
}

func TestRestartWithoutTheTokenIsAuditedAsSuch(t *testing.T) {
	handler, provider, records := adminRouter(t, auth.RoleOperator)
	session, csrf := signInForAdmin(t, handler)

	if recorder := restart(handler, testWorkerUUID, "", session, csrf); recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", recorder.Code)
	}
	if len(provider.restarted) != 0 {
		t.Fatalf("a token-less call restarted %v", provider.restarted)
	}

	events := auditRecords(t, records)
	refused := events[len(events)-1]
	if refused["action"] != "worker-restart" || refused["outcome"] != "refused" {
		t.Fatalf("record = %v, want a refused worker-restart", refused)
	}
	if refused["detail"] != "CSRF token missing or mismatched" {
		t.Errorf("detail = %v, want the missing token", refused["detail"])
	}
	if refused["role"] != auth.RoleOperator {
		t.Errorf("role = %v, want the operator who tried", refused["role"])
	}
}

func TestRestartIsAuditedWithTheSupervisorItLandedIn(t *testing.T) {
	handler, _, records := adminRouter(t, auth.RoleOperator)
	session, csrf := signInForAdmin(t, handler)

	recorder := restart(handler, testWorkerUUID, csrf.Value, session, csrf)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", recorder.Code, recorder.Body)
	}

	events := auditRecords(t, records)
	allowed := events[len(events)-1]
	if allowed["action"] != "worker-restart" || allowed["outcome"] != "allowed" {
		t.Fatalf("record = %v, want an allowed worker-restart", allowed)
	}
	if allowed["user"] != "alice" || allowed["role"] != auth.RoleOperator {
		t.Errorf("record = %v, want alice as operator", allowed)
	}
	if allowed["detail"] != "supervisor default" {
		t.Errorf("detail = %v, want the supervisor that owns the worker", allowed["detail"])
	}
	if allowed["address"] != "192.0.2.10" {
		t.Errorf("address = %v, want the client address", allowed["address"])
	}
}

func TestRestartOfAWorkerNobodyRunsIsANotFound(t *testing.T) {
	handler, provider, records := adminRouterWith(t, auth.RoleOperator, health.Report{Status: "ok"})
	session, csrf := signInForAdmin(t, handler)

	recorder := restart(handler, testWorkerUUID, csrf.Value, session, csrf)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", recorder.Code, recorder.Body)
	}
	if len(provider.restarted) != 0 {
		t.Fatalf("an unknown worker was restarted: %v", provider.restarted)
	}

	events := auditRecords(t, records)
	refused := events[len(events)-1]
	if refused["outcome"] != "refused" {
		t.Fatalf("record = %v, want a refusal", refused)
	}
}

func TestRestartAnswersConflictWhenTheWorkerExitsFirst(t *testing.T) {
	handler, provider, _ := adminRouter(t, auth.RoleOperator)
	// The snapshot still lists it, so the handler tries — and the pool says it is gone.
	provider.restartErr = &stubRestartError{}
	session, csrf := signInForAdmin(t, handler)

	if recorder := restart(handler, testWorkerUUID, csrf.Value, session, csrf); recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", recorder.Code, recorder.Body)
	}
}

type stubRestartError struct{}

func (*stubRestartError) Error() string { return "worker 7c9e6679 is not running" }

func TestReadingTheSessionIssuesAMissingToken(t *testing.T) {
	handler, _, _ := adminRouter(t, auth.RoleOperator)
	session, _ := signInForAdmin(t, handler)

	// A session whose token was never issued — one started before mutations existed, or one
	// whose cookie the browser dropped — gets a fresh one from the route the bundle reads.
	recorder := serve(handler, http.MethodGet, "/api/v1/session", "", session)
	if recorder.Code != http.StatusOK {
		t.Fatalf("session: status = %d", recorder.Code)
	}
	found := false
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == auth.CSRFCookie && cookie.Value != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reading the session set no %s cookie", auth.CSRFCookie)
	}
}

func TestAnOpenServiceHasNoAdministrativeRoute(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	provider := &stubProvider{report: health.Report{
		Status:      "ok",
		Supervisors: []health.Supervisor{{ID: "default", Workers: []health.Worker{{UUID: testWorkerUUID}}}},
	}}
	recorder := restart(NewRouter(provider, log), testWorkerUUID, "anything")

	// With no users there is nobody to hold a role, so the route is not mounted at all: a
	// restart cannot be requested anonymously, whatever token or header is sent.
	if recorder.Code == http.StatusAccepted {
		t.Fatal("an open service accepted a restart")
	}
	if len(provider.restarted) != 0 {
		t.Fatalf("an open service restarted %v", provider.restarted)
	}
}
