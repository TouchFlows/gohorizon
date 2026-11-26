package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gohorizon/gohorizon/internal/adapter"
	"github.com/gohorizon/gohorizon/internal/auth"
	"github.com/gohorizon/gohorizon/internal/health"
	"github.com/gohorizon/gohorizon/internal/metrics"
)

const testWorkerUUID = "7c9e6679-7425-40de-944b-e07fc1f90ae7"

// testJobUUID is the identity a failed attempt is served under: the uuid Laravel published.
const testJobUUID = "e0a1c2d3-4b5f-4a6b-8c7d-9e0f1a2b3c4d"

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

// retryJob asks for one failed job's retry, with the cookies a browser holds and, when token is
// not empty, the header it echoes.
func retryJob(handler http.Handler, id string, token string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/failed/"+id+"/retry", nil)
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

// failedJob is the history answer a retry is decided on: the newest attempt, failed, with the
// uuid the application stored it under.
func failedJob() metrics.Job {
	return metrics.Job{JobID: testJobUUID, UUID: testJobUUID, Queue: "default", Outcome: "failed"}
}

func TestRetryNeedsTheTokenAndAnOperatorRole(t *testing.T) {
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
			provider.job = failedJob()
			session, csrf := signInForAdmin(t, handler)

			token := ""
			switch tc.token {
			case "cookie":
				token = csrf.Value
			case "wrong":
				token = "not-the-issued-token"
			}
			recorder := retryJob(handler, testJobUUID, token, session, csrf)

			if recorder.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, tc.want, recorder.Body)
			}
			// Nothing is asked of the application unless both checks passed, so a refused call
			// is proved by no retry ever reaching the adapter.
			wantRetries := 0
			if tc.want == http.StatusAccepted {
				wantRetries = 1
			}
			if len(provider.retried) != wantRetries {
				t.Errorf("retried = %v, want %d retry(s)", provider.retried, wantRetries)
			}
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
		})
	}
}

func TestRetryWithoutASessionIsUnauthorized(t *testing.T) {
	handler, provider, _ := adminRouter(t, auth.RoleAdministrator)
	provider.job = failedJob()

	recorder := retryJob(handler, testJobUUID, "anything")
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
	if len(provider.retried) != 0 {
		t.Fatalf("a session-less call retried %v", provider.retried)
	}
}

func TestRetryRefusesAnIdThatCouldBeAnOption(t *testing.T) {
	handler, provider, _ := adminRouter(t, auth.RoleOperator)
	session, csrf := signInForAdmin(t, handler)

	// The id becomes one argument of the application's command, so a value artisan would read
	// as an option never reaches it (docs/adr/0012).
	recorder := retryJob(handler, "-h", csrf.Value, session, csrf)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body)
	}
	if len(provider.retried) != 0 {
		t.Fatalf("an option-like id reached the application: %v", provider.retried)
	}
}

func TestRetryIsAuditedWithTheQueueItWasPushedTo(t *testing.T) {
	handler, provider, records := adminRouter(t, auth.RoleOperator)
	provider.job = failedJob()
	session, csrf := signInForAdmin(t, handler)

	recorder := retryJob(handler, testJobUUID, csrf.Value, session, csrf)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", recorder.Code, recorder.Body)
	}
	// The answer names the identity the application was asked about and the queue it was
	// pushed back to, which is what the operator would look at next.
	var answer map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	if answer["job"] != testJobUUID || answer["queue"] != "default" {
		t.Errorf("answer = %v, want the uuid and its queue", answer)
	}
	if len(provider.retried) != 1 || provider.retried[0] != testJobUUID {
		t.Fatalf("retried = %v, want [%s]", provider.retried, testJobUUID)
	}

	events := auditRecords(t, records)
	allowed := events[len(events)-1]
	if allowed["action"] != "failed-job-retry" || allowed["outcome"] != "allowed" {
		t.Fatalf("record = %v, want an allowed failed-job-retry", allowed)
	}
	if allowed["user"] != "alice" || allowed["role"] != auth.RoleOperator {
		t.Errorf("record = %v, want alice as operator", allowed)
	}
	if allowed["detail"] != "queue default" {
		t.Errorf("detail = %v, want the queue the retry was pushed to", allowed["detail"])
	}
	if allowed["address"] != "192.0.2.10" {
		t.Errorf("address = %v, want the client address", allowed["address"])
	}
}

func TestRetryOfARecordOlderThanTheHistoryWindowIsStillAllowed(t *testing.T) {
	handler, provider, records := adminRouter(t, auth.RoleOperator)
	// History is pruned to 72 hours, so a failed record older than that is not in it — but it
	// is still in the application's store, which is what decides (docs/adr/0013).
	provider.jobErr = metrics.ErrJobNotFound
	session, csrf := signInForAdmin(t, handler)

	recorder := retryJob(handler, testJobUUID, csrf.Value, session, csrf)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", recorder.Code, recorder.Body)
	}
	if len(provider.retried) != 1 || provider.retried[0] != testJobUUID {
		t.Fatalf("retried = %v, want [%s]", provider.retried, testJobUUID)
	}
	// History only supplied the queue name, so the answer says it does not know it rather
	// than inventing one or refusing the action.
	var answer map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	if answer["job"] != testJobUUID || answer["queue"] != "" {
		t.Errorf("answer = %v, want the uuid and an unnamed queue", answer)
	}

	events := auditRecords(t, records)
	allowed := events[len(events)-1]
	if allowed["action"] != "failed-job-retry" || allowed["outcome"] != "allowed" {
		t.Fatalf("record = %v, want an allowed failed-job-retry", allowed)
	}
	if allowed["detail"] != "queue unknown" {
		t.Errorf("detail = %v, want the queue named as unknown", allowed["detail"])
	}
}

func TestRetryIsDecidedByTheStoreNotByTheLastOutcome(t *testing.T) {
	handler, provider, _ := adminRouter(t, auth.RoleOperator)
	// History's last observation of the identity is not the question: the application still
	// stores the failed record, so the action is allowed to ask it (docs/adr/0012).
	job := failedJob()
	job.Outcome = "processed"
	provider.job = job
	session, csrf := signInForAdmin(t, handler)

	recorder := retryJob(handler, testJobUUID, csrf.Value, session, csrf)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", recorder.Code, recorder.Body)
	}
	if len(provider.retried) != 1 {
		t.Fatalf("retried = %v, want one retry", provider.retried)
	}
}

func TestRetryOfAFailedJobTheApplicationNoLongerStoresIsANotFound(t *testing.T) {
	handler, provider, records := adminRouter(t, auth.RoleOperator)
	provider.job = failedJob()
	provider.retryErr = adapter.ErrFailedJobNotFound
	session, csrf := signInForAdmin(t, handler)

	recorder := retryJob(handler, testJobUUID, csrf.Value, session, csrf)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", recorder.Code, recorder.Body)
	}

	events := auditRecords(t, records)
	refused := events[len(events)-1]
	if refused["outcome"] != "refused" || refused["detail"] != "the application no longer stores "+testJobUUID {
		t.Fatalf("record = %v, want a refusal naming the missing record", refused)
	}
}

func TestRetryWorksWhenJobHistoryIsDisabled(t *testing.T) {
	handler, provider, records := adminRouter(t, auth.RoleOperator)
	provider.jobErr = metrics.ErrDisabled
	session, csrf := signInForAdmin(t, handler)

	// A deployment that keeps no history still has an application with a failed store, so
	// the action runs and only the queue name is missing (docs/adr/0012).
	recorder := retryJob(handler, testJobUUID, csrf.Value, session, csrf)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", recorder.Code, recorder.Body)
	}
	if len(provider.retried) != 1 || provider.retried[0] != testJobUUID {
		t.Fatalf("retried = %v, want [%s]", provider.retried, testJobUUID)
	}

	events := auditRecords(t, records)
	allowed := events[len(events)-1]
	if allowed["outcome"] != "allowed" || allowed["detail"] != "queue unknown" {
		t.Fatalf("record = %v, want an allowed retry with an unnamed queue", allowed)
	}
}

func TestRetryWhenTheApplicationCannotAnswerIsAServiceUnavailable(t *testing.T) {
	handler, provider, _ := adminRouter(t, auth.RoleOperator)
	provider.job = failedJob()
	provider.retryErr = &stubRestartError{}
	session, csrf := signInForAdmin(t, handler)

	// The command failed for a reason that is not "no such failed job": the application is
	// unhealthy, and the endpoint says so instead of claiming a retry happened.
	if recorder := retryJob(handler, testJobUUID, csrf.Value, session, csrf); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", recorder.Code, recorder.Body)
	}
}

func TestAnOpenServiceHasNoFailedJobRetry(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	provider := &stubProvider{report: health.Report{Status: "ok"}, job: failedJob()}
	recorder := retryJob(NewRouter(provider, log), testJobUUID, "anything")

	// With no users the administrative group is not mounted, so there is nobody to allow and
	// nothing to retry, whatever token or header is sent (docs/adr/0011).
	if recorder.Code == http.StatusAccepted {
		t.Fatal("an open service accepted a failed-job retry")
	}
	if len(provider.retried) != 0 {
		t.Fatalf("an open service retried %v", provider.retried)
	}
}

// deleteJob asks for one failed job's deletion, with the cookies a browser holds and, when
// token is not empty, the header it echoes.
func deleteJob(handler http.Handler, id string, token string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodDelete, "/api/v1/failed/"+id, nil)
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

func TestDeleteNeedsTheTokenAndAnOperatorRole(t *testing.T) {
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
		{name: "an operator with the token", role: auth.RoleOperator, token: "cookie", want: http.StatusOK},
		{name: "an administrator with the token", role: auth.RoleAdministrator, token: "cookie", want: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler, provider, _ := adminRouter(t, tc.role)
			provider.job = failedJob()
			session, csrf := signInForAdmin(t, handler)

			token := ""
			switch tc.token {
			case "cookie":
				token = csrf.Value
			case "wrong":
				token = "not-the-issued-token"
			}
			recorder := deleteJob(handler, testJobUUID, token, session, csrf)

			if recorder.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, tc.want, recorder.Body)
			}
			// Nothing is asked of the application unless both checks passed, so a refused call
			// is proved by no deletion ever reaching the adapter.
			wantDeletes := 0
			if tc.want == http.StatusOK {
				wantDeletes = 1
			}
			if len(provider.forgotten) != wantDeletes {
				t.Errorf("forgotten = %v, want %d deletion(s)", provider.forgotten, wantDeletes)
			}
			if wantDeletes == 1 && (len(provider.forgotten) != 1 || provider.forgotten[0] != testJobUUID) {
				t.Errorf("forgot %v, want [%s]", provider.forgotten, testJobUUID)
			}
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
		})
	}
}

func TestDeleteWithoutASessionIsUnauthorized(t *testing.T) {
	handler, provider, _ := adminRouter(t, auth.RoleAdministrator)
	provider.job = failedJob()

	if recorder := deleteJob(handler, testJobUUID, "anything"); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
	if len(provider.forgotten) != 0 {
		t.Fatalf("a session-less call deleted %v", provider.forgotten)
	}
}

func TestDeleteIsAuditedWithTheQueueItWasRemovedFrom(t *testing.T) {
	handler, provider, records := adminRouter(t, auth.RoleOperator)
	provider.job = failedJob()
	session, csrf := signInForAdmin(t, handler)

	recorder := deleteJob(handler, testJobUUID, csrf.Value, session, csrf)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body)
	}
	// The record is gone when the answer is written, which is what separates this from the 202
	// a retry gets; the body still names the identity and its queue for the log a client keeps.
	var answer map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	if answer["job"] != testJobUUID || answer["queue"] != "default" {
		t.Errorf("answer = %v, want the uuid and its queue", answer)
	}
	if len(provider.forgotten) != 1 || provider.forgotten[0] != testJobUUID {
		t.Fatalf("forgotten = %v, want [%s]", provider.forgotten, testJobUUID)
	}

	events := auditRecords(t, records)
	allowed := events[len(events)-1]
	if allowed["action"] != "failed-job-delete" || allowed["outcome"] != "allowed" {
		t.Fatalf("record = %v, want an allowed failed-job-delete", allowed)
	}
	if allowed["user"] != "alice" || allowed["role"] != auth.RoleOperator {
		t.Errorf("record = %v, want alice as operator", allowed)
	}
	if allowed["detail"] != "queue default" {
		t.Errorf("detail = %v, want the queue the job belonged to", allowed["detail"])
	}
	if allowed["address"] != "192.0.2.10" {
		t.Errorf("address = %v, want the client address", allowed["address"])
	}
}

func TestDeleteOfARecordOlderThanTheHistoryWindowIsStillAllowed(t *testing.T) {
	handler, provider, records := adminRouter(t, auth.RoleOperator)
	provider.jobErr = metrics.ErrJobNotFound
	session, csrf := signInForAdmin(t, handler)

	if recorder := deleteJob(handler, testJobUUID, csrf.Value, session, csrf); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body)
	}
	if len(provider.forgotten) != 1 || provider.forgotten[0] != testJobUUID {
		t.Fatalf("forgotten = %v, want [%s]", provider.forgotten, testJobUUID)
	}

	events := auditRecords(t, records)
	allowed := events[len(events)-1]
	if allowed["action"] != "failed-job-delete" || allowed["outcome"] != "allowed" {
		t.Fatalf("record = %v, want an allowed failed-job-delete", allowed)
	}
	if allowed["detail"] != "queue unknown" {
		t.Errorf("detail = %v, want the queue named as unknown", allowed["detail"])
	}
}

func TestDeleteWorksWhenJobHistoryIsDisabled(t *testing.T) {
	handler, provider, _ := adminRouter(t, auth.RoleOperator)
	provider.jobErr = metrics.ErrDisabled
	session, csrf := signInForAdmin(t, handler)

	// No history is not an unavailable application: the store is the only thing consulted.
	if recorder := deleteJob(handler, testJobUUID, csrf.Value, session, csrf); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body)
	}
	if len(provider.forgotten) != 1 || provider.forgotten[0] != testJobUUID {
		t.Fatalf("forgotten = %v, want [%s]", provider.forgotten, testJobUUID)
	}
}

func TestDeleteOfAFailedJobTheApplicationNoLongerStoresIsANotFound(t *testing.T) {
	handler, provider, records := adminRouter(t, auth.RoleOperator)
	provider.job = failedJob()
	provider.forgetErr = adapter.ErrFailedJobNotFound
	session, csrf := signInForAdmin(t, handler)

	if recorder := deleteJob(handler, testJobUUID, csrf.Value, session, csrf); recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", recorder.Code, recorder.Body)
	}

	events := auditRecords(t, records)
	refused := events[len(events)-1]
	if refused["outcome"] != "refused" || refused["detail"] != "the application no longer stores "+testJobUUID {
		t.Fatalf("record = %v, want a refusal naming the missing record", refused)
	}
}

func TestDeleteWhenTheApplicationCannotAnswerIsAServiceUnavailable(t *testing.T) {
	handler, provider, _ := adminRouter(t, auth.RoleOperator)
	provider.job = failedJob()
	provider.forgetErr = &stubRestartError{}
	session, csrf := signInForAdmin(t, handler)

	// The command failed for a reason that is not "no such failed job", so the endpoint says
	// the application is unhealthy instead of claiming a deletion happened.
	if recorder := deleteJob(handler, testJobUUID, csrf.Value, session, csrf); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", recorder.Code, recorder.Body)
	}
}

func TestAnOpenServiceHasNoFailedJobDelete(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	provider := &stubProvider{report: health.Report{Status: "ok"}, job: failedJob()}
	recorder := deleteJob(NewRouter(provider, log), testJobUUID, "anything")

	if recorder.Code == http.StatusOK {
		t.Fatal("an open service accepted a failed-job delete")
	}
	if len(provider.forgotten) != 0 {
		t.Fatalf("an open service deleted %v", provider.forgotten)
	}
}
