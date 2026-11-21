package telemetry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func jobEvent(id, outcome string) string {
	return `{"schema":1,"id":"` + id + `","type":"job.finished",` +
		`"application":"production","environment":"production",` +
		`"worker":"gohorizon:production:production:default-supervisor:1a2b3c4d",` +
		`"at":"2026-11-20T10:11:12.345Z",` +
		`"job":{"uuid":"u-1","id":"1","class":"App\\Jobs\\SendMail","connection":"redis",` +
		`"queue":"emails","attempts":2,"outcome":"` + outcome + `","duration_ms":1841,"exception":null},` +
		`"tags":[]}`
}

func TestDecodeReadsTheDocumentedJobEvent(t *testing.T) {
	event, err := Decode(jobEvent("evt-1", "released"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if event.ID != "evt-1" || event.Type != TypeJobFinished {
		t.Errorf("event = %+v, want the job event", event)
	}
	if event.Application != "production" || event.Environment != "production" {
		t.Errorf("scope = %s/%s, want production/production", event.Application, event.Environment)
	}
	if event.Job == nil || event.Job.Outcome != "released" {
		t.Errorf("job = %+v, want outcome released", event.Job)
	}
	job := event.Job
	if job.UUID != "u-1" || job.Class != `App\Jobs\SendMail` {
		t.Errorf("identity = uuid %q class %q, want both preserved", job.UUID, job.Class)
	}
	if job.Connection != "redis" || job.Attempts != 2 {
		t.Errorf("connection = %q attempts = %d, want redis and 2", job.Connection, job.Attempts)
	}
	if job.Exception != nil {
		t.Errorf("exception = %+v, want nil for an event that carries none", job.Exception)
	}
	if event.At.IsZero() {
		t.Error("at was not parsed")
	}
}

// These two payloads are entries a real fixture worker published, copied verbatim from
// `redis-cli XREVRANGE` on the events stream: the success shape and the failure shape.
// They are the evidence that uuid and class are always present, so requiring them cannot
// reject an event the package actually emits.
func TestDecodeReadsWhatARealWorkerPublished(t *testing.T) {
	const published = `{"schema": 1, "id": "cbf772e7-4246-47bc-8e77-c3d2af92ce9f", "type": "job.finished", "application": "gohorizon-e2e", "environment": "e2e", "worker": "unknown", "at": "2026-11-21T14:08:28.891Z", "job": {"uuid": "5a035154-b74d-4d1c-9e68-8c31f557f9e0", "id": "v1S0DWpiaTchaLeeZMnsy6Rd9NNASu2E", "class": "App\\Jobs\\RecordMarker", "connection": "redis", "queue": "default", "attempts": 1, "outcome": "processed", "duration_ms": 9}, "tags": []}`

	event, err := Decode(published)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	job := event.Job
	if job.UUID != "5a035154-b74d-4d1c-9e68-8c31f557f9e0" || job.Class != `App\Jobs\RecordMarker` {
		t.Errorf("identity = %+v, want the published uuid and class", job)
	}
	if job.ID != "v1S0DWpiaTchaLeeZMnsy6Rd9NNASu2E" || job.Connection != "redis" || job.Attempts != 1 {
		t.Errorf("job = %+v, want the published id, connection and attempts", job)
	}
	if job.Exception != nil {
		t.Errorf("exception = %+v, want none: array_filter omits it on success", job.Exception)
	}
}

func TestDecodeKeepsJobsPublishedWithoutAUUID(t *testing.T) {
	// A job pushed by something other than Laravel's dispatcher has no uuid; the queue
	// job id is then the only identity, and dropping the event would lose its metrics.
	payload := strings.Replace(jobEvent("evt-1", "processed"), `"uuid":"u-1",`, "", 1)

	event, err := Decode(payload)
	if err != nil {
		t.Fatalf("Decode a job without a uuid: %v", err)
	}
	if event.Job.UUID != "" {
		t.Errorf("uuid = %q, want it absent", event.Job.UUID)
	}
	if got := event.Job.Identity(); got != "id:1" {
		t.Errorf("Identity() = %q, want the recorder's id: fallback", got)
	}
}

func TestIdentityPrefersTheUUID(t *testing.T) {
	event, err := Decode(jobEvent("evt-1", "processed"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got := event.Job.Identity(); got != "u-1" {
		t.Errorf("Identity() = %q, want the published uuid", got)
	}
}

func TestDecodeReadsThePublishedFailureWithItsException(t *testing.T) {
	const published = `{"schema": 1, "id": "62586ab0-3d98-45a4-a253-1e7396702497", "type": "job.finished", "application": "gohorizon-e2e", "environment": "e2e", "worker": "unknown", "at": "2026-11-21T14:08:49.458Z", "job": {"uuid": "7b128a5c-3571-4f25-bc26-527920230969", "id": "K3BF2fnLUHK69xBoeMSA4PCi1zfMkYxS", "class": "App\\Jobs\\AlwaysFails", "connection": "redis", "queue": "default", "attempts": 1, "outcome": "failed", "duration_ms": 9, "exception": {"class": "RuntimeException", "message": "the acceptance fixture always fails"}}, "tags": []}`

	event, err := Decode(published)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if event.Job.Exception == nil {
		t.Fatalf("job = %+v, want the exception decoded", event.Job)
	}
	if event.Job.Exception.Class != "RuntimeException" || event.Job.Exception.Message == "" {
		t.Errorf("exception = %+v, want the class and sanitized message", event.Job.Exception)
	}
}

func TestOutcomesMatchTheSharedVocabulary(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "tests", "fixtures", "telemetry-outcomes.json"))
	if err != nil {
		t.Fatalf("read the shared outcome fixture: %v", err)
	}
	var fixture struct {
		Schema      int      `json:"schema"`
		JobOutcomes []string `json:"job_outcomes"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode the shared outcome fixture: %v", err)
	}
	if fixture.Schema != SchemaVersion {
		t.Errorf("fixture schema = %d, want %d", fixture.Schema, SchemaVersion)
	}
	if !slices.Equal(Outcomes, fixture.JobOutcomes) {
		t.Errorf("Outcomes = %v, want the shared fixture %v", Outcomes, fixture.JobOutcomes)
	}
}

func TestDecodeAcceptsEverySharedOutcome(t *testing.T) {
	for _, outcome := range Outcomes {
		t.Run(outcome, func(t *testing.T) {
			if _, err := Decode(jobEvent("evt-"+outcome, outcome)); err != nil {
				t.Fatalf("Decode(%s): %v", outcome, err)
			}
		})
	}
}

func TestDecodeReadsTheDocumentedWorkerEvent(t *testing.T) {
	payload := `{"schema":1,"id":"evt-2","type":"worker.stopped",` +
		`"application":"production","environment":"production",` +
		`"worker":"gohorizon:production:production:default-supervisor:1a2b3c4d",` +
		`"at":"2026-11-20T10:11:12.345Z",` +
		`"worker_event":{"exit_code":12,"reason":"memory","connection":"redis","queue":"emails",` +
		`"jobs_processed":117,"memory_mb":128.5}}`

	event, err := Decode(payload)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if event.WorkerEvent == nil || event.WorkerEvent.Reason != "memory" {
		t.Errorf("worker_event = %+v, want reason memory", event.WorkerEvent)
	}
}

func TestDecodeRejectsEventsTheConsumerCannotTrust(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{"unknown schema", strings.Replace(jobEvent("evt-1", "processed"), `"schema":1`, `"schema":2`, 1), "schema 2"},
		{"missing id", strings.Replace(jobEvent("evt-1", "processed"), `"id":"evt-1"`, `"id":""`, 1), "deduplication key"},
		{"unknown type", strings.Replace(jobEvent("evt-1", "processed"), `"type":"job.finished"`, `"type":"job.started"`, 1), "not supported"},
		{"unknown outcome", jobEvent("evt-1", "ok"), "job.outcome"},
		{"job event without a job", `{"schema":1,"id":"e","type":"job.finished","application":"production","environment":"production","at":"2026-11-20T10:11:12.345Z"}`, "requires a job object"},
		{"worker event without a body", `{"schema":1,"id":"e","type":"worker.stopped","application":"production","environment":"production","at":"2026-11-20T10:11:12.345Z"}`, "requires a worker_event"},
		{"missing timestamp", strings.Replace(jobEvent("evt-1", "processed"), `"at":"2026-11-20T10:11:12.345Z",`, "", 1), "at is required"},
		{"missing job id", strings.Replace(jobEvent("evt-1", "processed"), `"id":"1",`, "", 1), "job.id"},
		{"missing job class", strings.Replace(jobEvent("evt-1", "processed"), `"class":"App\\Jobs\\SendMail",`, "", 1), "job.class"},
		{"negative attempts", strings.Replace(jobEvent("evt-1", "processed"), `"attempts":2`, `"attempts":-1`, 1), "job.attempts"},
		{"exception without a class", strings.Replace(jobEvent("evt-1", "failed"), `"exception":null`, `"exception":{"message":"boom"}`, 1), "job.exception"},
		{"not json", `{`, "decode telemetry event"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode(tc.payload)
			if err == nil {
				t.Fatal("Decode accepted an event it cannot trust")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}
