package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gohorizon/gohorizon/internal/metrics"
)

// jobAt builds one finished attempt at a known time, so a cursor's contents are checkable.
func jobAt(id string, at time.Time) metrics.Job {
	return metrics.Job{
		ID:         id,
		JobID:      "u-" + id,
		UUID:       "u-" + id,
		Class:      `App\Jobs\SendMail`,
		Queue:      "emails",
		Connection: "redis",
		Outcome:    "processed",
		Attempts:   1,
		DurationMS: 120,
		FinishedAt: at,
	}
}

func TestJobsReadsOnePageWithTheFilters(t *testing.T) {
	start := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	provider := &stubProvider{jobs: []metrics.Job{jobAt("e1", start), jobAt("e2", start.Add(time.Minute))}}

	recorder := get(t, provider, "/api/v1/jobs?queue=emails&outcome=failed&limit=5")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if provider.jobQuery.Queue != "emails" || provider.jobQuery.Outcome != "failed" {
		t.Errorf("store query (%+v), want the requested filters", provider.jobQuery)
	}
	// One row more than the page, so the handler knows whether another page exists.
	if provider.jobQuery.Limit != 6 {
		t.Errorf("store limit = %d, want the page size plus the probe row", provider.jobQuery.Limit)
	}

	var payload jobsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(payload.Jobs) != 2 || payload.Jobs[0].ID != "e1" {
		t.Errorf("jobs = %+v, want the stored page", payload.Jobs)
	}
	// The probe row was not there, so this page is the last one.
	if payload.NextBefore != "" {
		t.Errorf("next_before = %q, want none on the last page", payload.NextBefore)
	}
}

func TestJobsDefaultsToFiftyAndNoFilters(t *testing.T) {
	provider := &stubProvider{}

	recorder := get(t, provider, "/api/v1/jobs")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if provider.jobQuery.Queue != "" || provider.jobQuery.Outcome != "" || provider.jobQuery.Before != nil {
		t.Errorf("store query = %+v, want no filters", provider.jobQuery)
	}
	if provider.jobQuery.Limit != defaultJobLimit+1 {
		t.Errorf("store limit = %d, want the default page plus the probe row", provider.jobQuery.Limit)
	}

	// An empty page is an empty array, never null.
	var payload struct {
		Jobs []metrics.Job `json:"jobs"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if payload.Jobs == nil || len(payload.Jobs) != 0 {
		t.Errorf("jobs = %#v, want an empty array", payload.Jobs)
	}
}

func TestJobsOffersACursorOnlyWhenMoreRowsRemain(t *testing.T) {
	start := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	provider := &stubProvider{jobs: []metrics.Job{
		jobAt("e3", start.Add(2*time.Minute)),
		jobAt("e2", start.Add(time.Minute)),
		jobAt("e1", start), // the probe row: a third page exists but is not part of this one
	}}

	first := get(t, provider, "/api/v1/jobs?limit=2")
	if first.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", first.Code)
	}

	var page jobsResponse
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(page.Jobs) != 2 || page.Jobs[1].ID != "e2" {
		t.Fatalf("jobs = %+v, want the first two rows", page.Jobs)
	}
	want := fmt.Sprintf("%d:e2", start.Add(time.Minute).Unix())
	if page.NextBefore != want {
		t.Errorf("next_before = %q, want the last row's keyset %q", page.NextBefore, want)
	}

	// The cursor round-trips: the next request continues after exactly that row.
	second := get(t, provider, "/api/v1/jobs?limit=2&before="+page.NextBefore)
	if second.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", second.Code)
	}
	cursor := provider.jobQuery.Before
	if cursor == nil {
		t.Fatal("the cursor was not passed to the store")
	}
	if !cursor.FinishedAt.Equal(start.Add(time.Minute)) || cursor.ID != "e2" {
		t.Errorf("cursor = %s/%s, want the row the page ended on", cursor.FinishedAt, cursor.ID)
	}
}

func TestJobsRejectsMalformedQueries(t *testing.T) {
	targets := []string{
		"/api/v1/jobs?limit=0",
		"/api/v1/jobs?limit=201",
		"/api/v1/jobs?limit=two",
		"/api/v1/jobs?outcome=exploded",
		"/api/v1/jobs?before=nonsense",
		"/api/v1/jobs?before=1763734129:",
		"/api/v1/jobs?before=yesterday:e1",
	}
	for _, target := range targets {
		if code := get(t, &stubProvider{}, target).Code; code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", target, code)
		}
	}
}

func TestJobDetailReturnsTheAttempt(t *testing.T) {
	start := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	detail := jobAt("e2", start)
	detail.Outcome = "failed"
	detail.Attempts = 3
	detail.Exception = &metrics.JobException{Class: "RuntimeException", Message: "the fixture always fails"}
	provider := &stubProvider{job: detail}

	recorder := get(t, provider, "/api/v1/jobs/u-e2")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if provider.jobID != "u-e2" {
		t.Errorf("store lookup %q, want the identity from the path", provider.jobID)
	}

	var payload metrics.Job
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if payload.JobID != "u-e2" || payload.Class != `App\Jobs\SendMail` || payload.Attempts != 3 {
		t.Errorf("job = %+v, want the stored attempt", payload)
	}
	if payload.Exception == nil || payload.Exception.Class != "RuntimeException" ||
		payload.Exception.Message != "the fixture always fails" {
		t.Errorf("exception = %+v, want it on the wire", payload.Exception)
	}
}

func TestJobDetailReportsAnUnknownJob(t *testing.T) {
	recorder := get(t, &stubProvider{jobErr: metrics.ErrJobNotFound}, "/api/v1/jobs/nobody")

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "job not found") {
		t.Errorf("body = %s, want the not-found reason", recorder.Body.String())
	}
}

func TestJobHistoryReportsDisabledStorage(t *testing.T) {
	targets := []string{"/api/v1/jobs", "/api/v1/jobs/u-1"}
	for _, target := range targets {
		recorder := get(t, &stubProvider{jobsErr: metrics.ErrDisabled, jobErr: metrics.ErrDisabled}, target)
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503 when history was never opened", target, recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), "metrics history is disabled") {
			t.Errorf("%s: body = %s, want the disabled-history reason", target, recorder.Body.String())
		}
	}
}

func TestJobHistoryHidesStoreErrors(t *testing.T) {
	err := errors.New("read job history: unable to open /srv/history.db")
	for _, target := range []string{"/api/v1/jobs", "/api/v1/jobs/u-1"} {
		recorder := get(t, &stubProvider{jobsErr: err, jobErr: err}, target)
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("%s: status = %d, want 500", target, recorder.Code)
		}
		if body := recorder.Body.String(); !strings.Contains(body, "could not read job history") ||
			strings.Contains(body, "/srv/history.db") {
			t.Errorf("%s: body = %s, want a generic error without the storage path", target, body)
		}
	}
}

// The cursor is the row's (finished_at, id), and its unix seconds are what the store keys on.
func TestJobCursorRoundTripsTheKeyset(t *testing.T) {
	job := jobAt("e1", time.Date(2026, 11, 21, 12, 0, 0, 500_000_000, time.UTC))

	cursor, err := parseJobCursor(formatJobCursor(job))
	if err != nil {
		t.Fatalf("parseJobCursor: %v", err)
	}
	if !cursor.FinishedAt.Equal(job.FinishedAt.Truncate(time.Second)) || cursor.ID != "e1" {
		t.Errorf("cursor = %s/%s, want the job's second and id", cursor.FinishedAt, cursor.ID)
	}

	// The id is the rest of the string, so a cursor built by hand cannot lose a suffix.
	cursor, err = parseJobCursor(strconv.FormatInt(job.FinishedAt.Unix(), 10) + ":a:b")
	if err != nil || cursor.ID != "a:b" {
		t.Errorf("cursor = %+v/%v, want the whole id after the first colon", cursor, err)
	}
}
