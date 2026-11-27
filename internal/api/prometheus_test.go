package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gohorizon/gohorizon/internal/health"
)

// prometheusReport is a report with one of everything: a held lock, counters, two queues
// (one with a count the driver could not report) and a supervisor whose pool is running
// with one worker stuck in a crash loop.
func prometheusReport() health.Report {
	pending, reserved := 5, 0
	return health.Report{
		Status:       "ok",
		Lock:         health.Lock{Held: true},
		Capabilities: health.Capabilities{Available: true, Schema: 1},
		Telemetry:    health.Telemetry{Events: 42, Duplicates: 3, Invalid: 1, Errors: 2},
		Queues: health.Queues{
			Depths: []health.QueueDepth{
				{Queue: "reports", Pending: &pending}, // only one of its three counts is known
				{Queue: "default", Pending: &pending, Reserved: &reserved},
			},
		},
		Supervisors: []health.Supervisor{{
			ID: "default",
			Workers: []health.Worker{
				{UUID: "a", State: "running"},
				{UUID: "b", State: "crashed", CrashLooping: true},
				{UUID: "c", State: "running"},
			},
		}},
	}
}

func TestMetricsExpositionServesTheOperationalState(t *testing.T) {
	recorder := get(t, &stubProvider{report: prometheusReport()}, "/metrics")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("content type = %q, want the exposition format's own media type", got)
	}

	body := recorder.Body.String()
	for _, want := range []string{
		"# TYPE gohorizon_up gauge\ngohorizon_up 1\n",
		"# TYPE gohorizon_lock_held gauge\ngohorizon_lock_held 1\n",
		"# TYPE gohorizon_capabilities_available gauge\ngohorizon_capabilities_available 1\n",
		"# TYPE gohorizon_capabilities_failed_reads gauge\ngohorizon_capabilities_failed_reads 0\n",
		"# TYPE gohorizon_capabilities_contradiction gauge\ngohorizon_capabilities_contradiction 0\n",
		"# TYPE gohorizon_capabilities_last_success_timestamp_seconds gauge\ngohorizon_capabilities_last_success_timestamp_seconds 0\n",
		"# TYPE gohorizon_telemetry_events_total counter\n",
		"gohorizon_telemetry_events_total{result=\"event\"} 42\n",
		"gohorizon_telemetry_events_total{result=\"duplicate\"} 3\n",
		"gohorizon_telemetry_events_total{result=\"invalid\"} 1\n",
		"gohorizon_telemetry_events_total{result=\"error\"} 2\n",
		"# TYPE gohorizon_queue_jobs gauge\n",
		"gohorizon_queue_jobs{queue=\"default\",state=\"pending\"} 5\n",
		// A known zero is a fact, so a count the driver did report is written.
		"gohorizon_queue_jobs{queue=\"default\",state=\"reserved\"} 0\n",
		"gohorizon_queue_jobs{queue=\"reports\",state=\"pending\"} 5\n",
		"# TYPE gohorizon_workers gauge\n",
		"gohorizon_workers{supervisor=\"default\",state=\"crashed\"} 1\n",
		"gohorizon_workers{supervisor=\"default\",state=\"running\"} 2\n",
		"# TYPE gohorizon_crash_looping_workers gauge\n",
		"gohorizon_crash_looping_workers{supervisor=\"default\"} 1\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body has no %q\n%s", want, body)
		}
	}

	// The driver could not report these, so no sample may claim they are zero.
	if strings.Contains(body, `queue="reports",state="reserved"`) ||
		strings.Contains(body, `queue="reports",state="delayed"`) {
		t.Errorf("body states a count the driver never reported:\n%s", body)
	}

	// Queues are ordered by name, so a scrape of the same state is the same bytes.
	if strings.Index(body, `queue="default"`) > strings.Index(body, `queue="reports"`) {
		t.Errorf("queues are not in name order:\n%s", body)
	}
}

// TestMetricsExpositionReportsWhenTheLoopLastAnswered is the staleness sample: a timestamp rather
// than an age, so an alert is `time() - this > 3 * interval` and a loop that stopped is visible
// while every other capabilities sample still looks healthy (docs/adr/0014).
func TestMetricsExpositionReportsWhenTheLoopLastAnswered(t *testing.T) {
	report := prometheusReport()
	report.Capabilities.LastReadAt = 1763600000

	body := get(t, &stubProvider{report: report}, "/metrics").Body.String()

	if !strings.Contains(body, "\ngohorizon_capabilities_last_success_timestamp_seconds 1763600000\n") {
		t.Errorf("body has no last-success sample:\n%s", body)
	}
}

// TestMetricsExpositionReportsALateContradiction is the preflight's second life: a retry_after the
// application lowered under a worker's timeout while the pool runs is a state to alert on, since
// the launch it would have refused already happened (docs/adr/0014).
func TestMetricsExpositionReportsALateContradiction(t *testing.T) {
	report := prometheusReport()
	report.Capabilities.Contradiction = "worker launch contradicts the application's capabilities: retry_after (30s) is below worker.timeout (60s)"

	body := get(t, &stubProvider{report: report}, "/metrics").Body.String()

	if !strings.Contains(body, "\ngohorizon_capabilities_contradiction 1\n") {
		t.Errorf("body has no contradiction sample:\n%s", body)
	}
}

// TestMetricsExpositionReportsAnUnreadableDescriptor is the alertable state: a deployment the
// service presents as read-only is a zero on the gauge with the failed-re-read streak beside it,
// so an alert fires without anyone reading the log (docs/adr/0014).
func TestMetricsExpositionReportsAnUnreadableDescriptor(t *testing.T) {
	provider := &stubProvider{report: health.Report{
		Status: "ok",
		Capabilities: health.Capabilities{
			Available:   false,
			Schema:      2,
			FailedReads: 2,
			Error:       "the application reports capabilities schema 2; this GoHorizon understands schema 1",
		},
	}}

	body := get(t, provider, "/metrics").Body.String()
	for _, want := range []string{
		"gohorizon_capabilities_available 0\n",
		"gohorizon_capabilities_failed_reads 2\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body has no %q\n%s", want, body)
		}
	}
	// The sample itself, not the HELP line that also begins with the metric name.
	if strings.Contains(body, "\ngohorizon_capabilities_available 1\n") {
		t.Errorf("body reports an unreadable descriptor as available:\n%s", body)
	}
}

func TestMetricsExpositionEscapesLabelValues(t *testing.T) {
	pending := 1
	odd := "we\"ird\\queue\nowned"
	provider := &stubProvider{report: health.Report{
		Queues:      health.Queues{Depths: []health.QueueDepth{{Queue: odd, Pending: &pending}}},
		Supervisors: []health.Supervisor{{ID: "sup\"1"}},
	}}

	body := get(t, provider, "/metrics").Body.String()

	// The quote and the backslash are escaped, so the value neither ends early nor is read
	// as an escape that is not there.
	if want := `gohorizon_queue_jobs{queue="we\"ird\\queue\nowned",state="pending"} 1`; !strings.Contains(body, want) {
		t.Errorf("body has no %q\n%s", want, body)
	}
	if want := `gohorizon_crash_looping_workers{supervisor="sup\"1"} 0`; !strings.Contains(body, want) {
		t.Errorf("body has no %q\n%s", want, body)
	}
	// A line feed in a value would split the sample; it is written as its escape instead.
	newline := &stubProvider{report: health.Report{
		Queues: health.Queues{Depths: []health.QueueDepth{{Queue: "two\nlines", Pending: &pending}}},
	}}
	line := get(t, newline, "/metrics").Body.String()
	if !strings.Contains(line, `queue="two\nlines"`) {
		t.Errorf("a line feed in a queue name split the sample:\n%s", line)
	}
}

// A lost lock is a zero on the gauge, not a failing scrape: the endpoint still answers so a
// downstream alert can see why.
func TestMetricsExpositionReportsALostLock(t *testing.T) {
	provider := &stubProvider{report: health.Report{
		Status: "degraded",
		Lock:   health.Lock{Degraded: true, LastError: "lock renewal failed"},
	}}

	recorder := get(t, provider, "/metrics")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 while the lock is only lost", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "gohorizon_lock_held 0\n") {
		t.Errorf("body = %s, want a zero lock gauge", recorder.Body.String())
	}
}

// A service that has sampled nothing yet still exposes every family, with no sample that
// would claim a queue or a supervisor exists.
func TestMetricsExpositionBeforeAnySample(t *testing.T) {
	body := get(t, &stubProvider{}, "/metrics").Body.String()

	for _, want := range []string{
		"# TYPE gohorizon_up gauge\ngohorizon_up 1\n",
		"# TYPE gohorizon_telemetry_events_total counter\ngohorizon_telemetry_events_total{result=\"event\"} 0\n",
		"# TYPE gohorizon_workers gauge\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body has no %q\n%s", want, body)
		}
	}
	for _, unwanted := range []string{"gohorizon_queue_jobs{", "gohorizon_workers{", "gohorizon_crash_looping_workers{"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("body invents %s before anything was sampled:\n%s", unwanted, body)
		}
	}
}
