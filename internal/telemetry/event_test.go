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
	if event.At.IsZero() {
		t.Error("at was not parsed")
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
