package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gohorizon/gohorizon/internal/health"
	"github.com/gohorizon/gohorizon/internal/metrics"
)

type stubProvider struct {
	report  health.Report
	buckets []metrics.Bucket
	err     error

	// The last metrics query, so a test can check what the handler parsed.
	resolution metrics.Resolution
	queue      string
	from       time.Time
	to         time.Time
}

func (s *stubProvider) Report() health.Report { return s.report }

func (s *stubProvider) Buckets(_ context.Context, resolution metrics.Resolution, queue string, from, to time.Time) ([]metrics.Bucket, error) {
	s.resolution, s.queue, s.from, s.to = resolution, queue, from, to
	return s.buckets, s.err
}

func get(t *testing.T, provider Provider, target string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	NewRouter(provider, log).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	return recorder
}

func getHealth(t *testing.T, report health.Report) *httptest.ResponseRecorder {
	t.Helper()
	return get(t, &stubProvider{report: report}, "/api/v1/health")
}

func TestHealthExposesTelemetryCounters(t *testing.T) {
	recorder := getHealth(t, health.Report{
		Status: "ok",
		Telemetry: health.Telemetry{
			Events:     42,
			Duplicates: 3,
			Invalid:    1,
			Errors:     2,
		},
	})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	var payload struct {
		Telemetry struct {
			Events     int  `json:"events"`
			Duplicates int  `json:"duplicates"`
			Invalid    int  `json:"invalid"`
			Errors     int  `json:"errors"`
			Failing    bool `json:"failing"`
		} `json:"telemetry"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if payload.Telemetry.Events != 42 || payload.Telemetry.Duplicates != 3 ||
		payload.Telemetry.Invalid != 1 || payload.Telemetry.Errors != 2 {
		t.Errorf("telemetry = %+v, want the consumer's counters", payload.Telemetry)
	}
	if payload.Telemetry.Failing {
		t.Error("failing = true for a healthy consumer")
	}
}

func TestHealthExposesQueueDepth(t *testing.T) {
	pending, reserved, delayed := 5, 1, 2
	oldest := int64(1763600000)
	recorder := getHealth(t, health.Report{
		Status: "ok",
		Queues: health.Queues{
			Connection: "redis",
			Depths: []health.QueueDepth{
				{Queue: "default", Pending: &pending, Reserved: &reserved, Delayed: &delayed, OldestPendingAt: &oldest},
				{Queue: "sync-only", OldestPendingError: "RedisException"},
			},
		},
	})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	var payload struct {
		Queues struct {
			Connection string `json:"connection"`
			Error      string `json:"error"`
			Depths     []struct {
				Queue              string `json:"queue"`
				Pending            *int   `json:"pending"`
				Reserved           *int   `json:"reserved"`
				Delayed            *int   `json:"delayed"`
				OldestPendingAt    *int64 `json:"oldest_pending_at"`
				OldestPendingError string `json:"oldest_pending_error"`
			} `json:"depths"`
		} `json:"queues"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if payload.Queues.Connection != "redis" || payload.Queues.Error != "" {
		t.Errorf("queues = %+v, want the inspection's connection and no error", payload.Queues)
	}
	if len(payload.Queues.Depths) != 2 {
		t.Fatalf("depths = %+v, want both queues", payload.Queues.Depths)
	}
	depth := payload.Queues.Depths[0]
	if depth.Pending == nil || *depth.Pending != 5 || depth.Reserved == nil || *depth.Reserved != 1 ||
		depth.Delayed == nil || *depth.Delayed != 2 {
		t.Errorf("depth = %+v, want the reported counts", depth)
	}
	if depth.OldestPendingAt == nil || *depth.OldestPendingAt != oldest {
		t.Errorf("oldest_pending_at = %v, want the backlog age on the wire", depth.OldestPendingAt)
	}
	// A driver without counts reports nothing, and the body must say null rather than
	// invent a zero; an age it could not read is reported as an error instead.
	if payload.Queues.Depths[1].Pending != nil {
		t.Errorf("pending = %v, want null for a driver that cannot report it", *payload.Queues.Depths[1].Pending)
	}
	if stalled := payload.Queues.Depths[1]; stalled.OldestPendingAt != nil || stalled.OldestPendingError != "RedisException" {
		t.Errorf("stalled = %+v, want the failed age without a timestamp", stalled)
	}
}

// A worker that keeps crashing is reported in the body, like the other body-only
// observations: the manager is already replacing it, so flapping the status would only
// make a health check restart a supervisor that is doing its job.
func TestHealthShowsACrashLoopWithoutChangingTheStatus(t *testing.T) {
	recorder := getHealth(t, health.Report{
		Status: "ok",
		Supervisors: []health.Supervisor{{
			ID:      "default-supervisor",
			Desired: 1,
			Crashed: 1,
			Workers: []health.Worker{{
				UUID: "abcd1234", Queue: "default", State: "crashed",
				Restarts: 3, CrashLooping: true,
			}},
		}},
	})

	var payload struct {
		Status      string `json:"status"`
		Supervisors []struct {
			Workers []struct {
				CrashLooping bool `json:"crash_looping"`
				Restarts     int  `json:"restarts"`
			} `json:"workers"`
		} `json:"supervisors"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if payload.Status != "ok" {
		t.Errorf("status = %q, want a crash loop to leave the status alone", payload.Status)
	}
	if len(payload.Supervisors) != 1 || len(payload.Supervisors[0].Workers) != 1 {
		t.Fatalf("body = %s, want one supervisor with one worker", recorder.Body)
	}
	if worker := payload.Supervisors[0].Workers[0]; !worker.CrashLooping || worker.Restarts != 3 {
		t.Errorf("worker = %+v, want the crash loop reported", worker)
	}
}

func TestFailingIngestionIsVisibleWithoutFailingTheEndpoint(t *testing.T) {
	recorder := getHealth(t, health.Report{
		Status: "ok",
		Telemetry: health.Telemetry{
			Errors:    7,
			Failing:   true,
			LastError: "read group \"gohorizon\": redis is down",
		},
	})

	// A failed check here would get a supervisor with healthy workers restarted.
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 while only ingestion is failing", recorder.Code)
	}

	var payload struct {
		Telemetry struct {
			Errors    int    `json:"errors"`
			Failing   bool   `json:"failing"`
			LastError string `json:"last_error"`
		} `json:"telemetry"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !payload.Telemetry.Failing || payload.Telemetry.LastError == "" || payload.Telemetry.Errors != 7 {
		t.Errorf("telemetry = %+v, want the live failure visible", payload.Telemetry)
	}
}

func TestMetricsReadsPersistedBuckets(t *testing.T) {
	start := time.Date(2026, 11, 20, 21, 0, 0, 0, time.UTC)
	provider := &stubProvider{buckets: []metrics.Bucket{
		{Start: start, Processed: 12, Released: 1, Failed: 2, TimedOut: 3, DurationMS: 4567},
	}}

	recorder := get(t, provider,
		"/api/v1/metrics?queue=default&from=2026-11-20T21:00:00Z&to=2026-11-20T22:00:00Z")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if provider.queue != "default" || !provider.from.Equal(start) || !provider.to.Equal(start.Add(time.Hour)) {
		t.Errorf("store read (%q, %s, %s), want the parsed range", provider.queue, provider.from, provider.to)
	}

	var payload struct {
		Queue      string             `json:"queue"`
		Resolution metrics.Resolution `json:"resolution"`
		From       time.Time          `json:"from"`
		To         time.Time          `json:"to"`
		Buckets    []metrics.Bucket   `json:"buckets"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if payload.Queue != "default" || payload.Resolution != metrics.ResolutionMinute ||
		!payload.From.Equal(start) || !payload.To.Equal(start.Add(time.Hour)) {
		t.Errorf("payload = %q %q %s..%s, want the queried minute range",
			payload.Queue, payload.Resolution, payload.From, payload.To)
	}
	if len(payload.Buckets) != 1 {
		t.Fatalf("buckets = %+v, want one", payload.Buckets)
	}
	bucket := payload.Buckets[0]
	if !bucket.Start.Equal(start) || bucket.Processed != 12 || bucket.Released != 1 ||
		bucket.Failed != 2 || bucket.TimedOut != 3 || bucket.DurationMS != 4567 {
		t.Errorf("bucket = %+v, want the stored counters", bucket)
	}
}

func TestMetricsDefaultsToTheLastHour(t *testing.T) {
	provider := &stubProvider{}

	recorder := get(t, provider, "/api/v1/metrics?queue=emails")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if provider.queue != "emails" || provider.resolution != metrics.ResolutionMinute {
		t.Errorf("store read (%q, %q), want emails at minute resolution", provider.queue, provider.resolution)
	}
	if window := provider.to.Sub(provider.from); window != time.Hour {
		t.Errorf("window = %s, want an hour", window)
	}

	// No history yet is an empty array, not null.
	var payload struct {
		Buckets []metrics.Bucket `json:"buckets"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if payload.Buckets == nil || len(payload.Buckets) != 0 {
		t.Errorf("buckets = %#v, want an empty array", payload.Buckets)
	}
}

func TestMetricsReadsTheRequestedResolution(t *testing.T) {
	for _, resolution := range []metrics.Resolution{metrics.ResolutionHourly, metrics.ResolutionDaily} {
		t.Run(string(resolution), func(t *testing.T) {
			provider := &stubProvider{}
			recorder := get(t, provider, "/api/v1/metrics?queue=default&resolution="+string(resolution))

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", recorder.Code)
			}
			if provider.resolution != resolution {
				t.Errorf("store read resolution = %q, want %q", provider.resolution, resolution)
			}

			var payload struct {
				Resolution metrics.Resolution `json:"resolution"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if payload.Resolution != resolution {
				t.Errorf("payload resolution = %q, want %q", payload.Resolution, resolution)
			}
		})
	}
}

func TestMetricsDefaultsTheWindowToTheResolution(t *testing.T) {
	// The minute default is covered by TestMetricsDefaultsToTheLastHour, which also
	// proves that omitting the parameter selects it.
	for _, tc := range []struct {
		resolution metrics.Resolution
		window     time.Duration
	}{
		{metrics.ResolutionHourly, 24 * time.Hour},
		{metrics.ResolutionDaily, 30 * 24 * time.Hour},
	} {
		t.Run(string(tc.resolution), func(t *testing.T) {
			provider := &stubProvider{}
			recorder := get(t, provider, "/api/v1/metrics?queue=default&resolution="+string(tc.resolution))

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", recorder.Code)
			}
			if window := provider.to.Sub(provider.from); window != tc.window {
				t.Errorf("window = %s, want %s", window, tc.window)
			}
		})
	}
}

func TestMetricsRejectsMalformedQueries(t *testing.T) {
	targets := []string{
		"/api/v1/metrics",
		"/api/v1/metrics?queue=",
		"/api/v1/metrics?queue=default&resolution=weekly",
		"/api/v1/metrics?queue=default&from=yesterday",
		"/api/v1/metrics?queue=default&to=not-a-time",
		"/api/v1/metrics?queue=default&from=2026-11-20T22:00:00Z&to=2026-11-20T21:00:00Z",
	}
	for _, target := range targets {
		if code := get(t, &stubProvider{}, target).Code; code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", target, code)
		}
	}
}

func TestMetricsReportsDisabledHistory(t *testing.T) {
	recorder := get(t, &stubProvider{err: metrics.ErrDisabled}, "/api/v1/metrics?queue=default")

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when history was never opened", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "metrics history is disabled") {
		t.Errorf("body = %s, want the disabled-history reason", recorder.Body.String())
	}
}

func TestMetricsHidesStoreErrors(t *testing.T) {
	provider := &stubProvider{err: errors.New("read queue \"default\": unable to open /srv/history.db")}
	recorder := get(t, provider, "/api/v1/metrics?queue=default")

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "could not read metrics history") || strings.Contains(body, "/srv/history.db") {
		t.Errorf("body = %s, want a generic error without the storage path", body)
	}
}

func TestHealthStillReportsLockDegradation(t *testing.T) {
	recorder := getHealth(t, health.Report{
		Status: "degraded",
		Lock:   health.Lock{Degraded: true, LastError: "lock renewal failed"},
	})

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for a degraded report", recorder.Code)
	}
}
