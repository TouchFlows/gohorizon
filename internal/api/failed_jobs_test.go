package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/gohorizon/gohorizon/internal/adapter"
	"github.com/gohorizon/gohorizon/internal/auth"
)

func failedStoreJob(id string) adapter.FailedJob {
	return adapter.FailedJob{
		ID:         id,
		UUID:       testJobUUID,
		Connection: "redis",
		Queue:      "default",
		Job:        `App\Jobs\SendInvoice`,
		FailedAt:   "2026-11-26 09:30:00",
		Exception:  "RuntimeException: could not reach the mail server",
	}
}

func TestFailedJobsReadsTheApplicationsOwnStore(t *testing.T) {
	provider := &stubProvider{
		failedJobs: []adapter.FailedJob{failedStoreJob("17"), failedStoreJob("16")},
		failedNext: "16",
	}

	recorder := get(t, provider, "/api/v1/failed")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body)
	}
	// The default page matches job history, so a client that sends nothing pages the same way.
	if provider.failedQuery != (adapter.FailedJobQuery{Limit: adapter.DefaultFailedJobsLimit}) {
		t.Errorf("query = %+v, want the default page", provider.failedQuery)
	}

	var payload struct {
		FailedJobs []adapter.FailedJob `json:"failed_jobs"`
		NextBefore string              `json:"next_before"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(payload.FailedJobs) != 2 {
		t.Fatalf("failed_jobs = %+v, want both records", payload.FailedJobs)
	}
	first := payload.FailedJobs[0]
	if first.ID != "17" || first.UUID != testJobUUID || first.Queue != "default" ||
		first.Job != `App\Jobs\SendInvoice` || first.Exception == "" {
		t.Errorf("first = %+v, want the record the application reported", first)
	}
	if payload.NextBefore != "16" {
		t.Errorf("next_before = %q, want the cursor", payload.NextBefore)
	}
}

func TestFailedJobsPassesThePageToTheAdapter(t *testing.T) {
	provider := &stubProvider{}

	recorder := get(t, provider, "/api/v1/failed?limit=2&before=9")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body)
	}
	if provider.failedQuery != (adapter.FailedJobQuery{Limit: 2, Before: "9"}) {
		t.Errorf("query = %+v, want limit 2 before 9", provider.failedQuery)
	}
}

func TestFailedJobsAnswersAnEmptyPageAsAnEmptyArray(t *testing.T) {
	recorder := get(t, &stubProvider{}, "/api/v1/failed")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	// An application with no failed jobs is not an unavailable application.
	if !strings.Contains(recorder.Body.String(), `"failed_jobs":[]`) {
		t.Errorf("body = %s, want an empty array", recorder.Body.String())
	}
}

func TestFailedJobsRejectsAPageItCannotAskFor(t *testing.T) {
	targets := []string{
		"/api/v1/failed?limit=0",
		"/api/v1/failed?limit=201",
		"/api/v1/failed?limit=abc",
		// A cursor is opaque, so only its shape is rejected here; whether it decodes is the
		// application's answer, read by the case below.
		"/api/v1/failed?before=1.5",
		"/api/v1/failed?before=a%20b",
	}
	for _, target := range targets {
		provider := &stubProvider{}
		if code := get(t, provider, target).Code; code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", target, code)
		}
		if provider.failedQuery.Limit != 0 {
			t.Errorf("%s reached the adapter with %+v", target, provider.failedQuery)
		}
	}
}

func TestFailedJobsAnswersAnUnreadableCursorWithABadRequest(t *testing.T) {
	provider := &stubProvider{failedErr: adapter.ErrInvalidFailedJobCursor}

	// A cursor this service never issued is the caller's mistake, not an application that
	// could not answer.
	recorder := get(t, provider, "/api/v1/failed?before=not-a-cursor")

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body)
	}
	if !strings.Contains(recorder.Body.String(), "next_before") {
		t.Errorf("body = %s, want the cursor field named", recorder.Body.String())
	}
}

// TestFailedJobsRefusesAPayloadItCannotRead is the endpoint half of withholding the operation: a
// failed-list schema this service does not read is refused before the command runs, with the
// reason the descriptor publishes, instead of starting PHP to fail decoding on every request
// (docs/adr/0014).
func TestFailedJobsRefusesAPayloadItCannotRead(t *testing.T) {
	caps := descriptor()
	caps.Payloads.FailedJobs = adapter.FailedJobsSchema + 1
	provider := &stubProvider{caps: caps}

	recorder := get(t, provider, "/api/v1/failed")

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", recorder.Code, recorder.Body)
	}
	for _, want := range []string{"schema 2", "schema 1"} {
		if !strings.Contains(recorder.Body.String(), want) {
			t.Errorf("body = %s, want it to name %q", recorder.Body.String(), want)
		}
	}
	// Nothing was started: the command would print a payload this service cannot read.
	if provider.failedQuery != (adapter.FailedJobQuery{}) {
		t.Errorf("the adapter was asked with %+v, want no read", provider.failedQuery)
	}
}

func TestFailedJobsReportsAnApplicationThatCannotAnswer(t *testing.T) {
	provider := &stubProvider{failedErr: errors.New("gohorizon:failed:list failed in /srv/application")}

	recorder := get(t, provider, "/api/v1/failed")

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "could not read failed jobs") || strings.Contains(body, "/srv/application") {
		t.Errorf("body = %s, want the generic reason without the application path", body)
	}
}

func TestFailedJobsNeedsASessionButNotAnOperator(t *testing.T) {
	handler, _, _ := adminRouter(t, auth.RoleViewer)

	if recorder := serve(handler, http.MethodGet, "/api/v1/failed", ""); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("without a session: status = %d, want 401", recorder.Code)
	}

	session, _ := signInForAdmin(t, handler)
	// Reading is what the least role may do: the administrative group is not involved.
	if recorder := serve(handler, http.MethodGet, "/api/v1/failed", "", session); recorder.Code != http.StatusOK {
		t.Fatalf("as a viewer: status = %d, want 200 (body %s)", recorder.Code, recorder.Body)
	}
}
