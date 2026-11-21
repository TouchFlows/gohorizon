// Package telemetry consumes the lifecycle events the gohorizon/laravel package
// publishes to the Redis Stream.
//
// This is observation, never an execution ledger (handoff §14.7): the consumer
// validates, deduplicates and acknowledges events, counts them, and hands job metrics
// to a durable sink (docs/adr/0007). The dashboard is a later slice.
package telemetry

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// SchemaVersion is the event schema this consumer understands. An event with any
// other schema is discarded and counted, never guessed (docs/adr/0006).
const SchemaVersion = 1

const (
	TypeJobFinished   = "job.finished"
	TypeWorkerStopped = "worker.stopped"
)

// Outcomes is the closed set of terminal job outcomes, in the order the Laravel
// package documents them. tests/fixtures/telemetry-outcomes.json is the same
// vocabulary; both test suites read it, so an outcome cannot be added on one side
// alone (docs/adr/0006).
var Outcomes = []string{"processed", "released", "failed", "timed_out"}

// Event is a telemetry record. Only the fields the consumer acts on are decoded; the
// rest of the payload is ignored so the schema can grow without breaking ingestion.
type Event struct {
	Schema      int       `json:"schema"`
	ID          string    `json:"id"`
	Type        string    `json:"type"`
	Application string    `json:"application"`
	Environment string    `json:"environment"`
	Worker      string    `json:"worker"`
	At          time.Time `json:"at"`

	Job         *jobPayload    `json:"job,omitempty"`
	WorkerEvent *workerPayload `json:"worker_event,omitempty"`
}

type jobPayload struct {
	UUID       string     `json:"uuid"`
	ID         string     `json:"id"`
	Class      string     `json:"class"`
	Connection string     `json:"connection"`
	Queue      string     `json:"queue"`
	Attempts   int        `json:"attempts"`
	Outcome    string     `json:"outcome"`
	DurationMS int64      `json:"duration_ms"`
	Exception  *Exception `json:"exception"`
}

// Identity is the key that ties attempts of one job together: the uuid Laravel assigned
// when there is one, otherwise "id:" plus the queue job id. That fallback is the
// recorder's own (it keys in-flight attempts the same way), so both halves agree on what
// names a job when no uuid was published.
func (j *jobPayload) Identity() string {
	if uuid := strings.TrimSpace(j.UUID); uuid != "" {
		return uuid
	}
	return "id:" + strings.TrimSpace(j.ID)
}

// Exception is the failure a throwing attempt carries. The publisher sanitizes and
// truncates the message before publishing it (docs/adr/0006); the consumer only
// transports it, and `Message` is the sanitized text to show an operator.
type Exception struct {
	Class   string `json:"class"`
	Message string `json:"message"`
}

type workerPayload struct {
	Reason string `json:"reason"`
}

// Decode parses and validates one stream entry's payload.
func Decode(payload string) (Event, error) {
	var event Event
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return Event{}, fmt.Errorf("decode telemetry event: %w", err)
	}
	if err := event.validate(); err != nil {
		return Event{}, err
	}
	return event, nil
}

func (e Event) validate() error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if e.Schema != SchemaVersion {
		add("schema %d is not supported (expected %d)", e.Schema, SchemaVersion)
	}
	if strings.TrimSpace(e.ID) == "" {
		add("id is required: it is the deduplication key")
	}
	if e.Application == "" || e.Environment == "" {
		add("application and environment are required")
	}
	if e.At.IsZero() {
		add("at is required")
	}

	switch e.Type {
	case TypeJobFinished:
		if e.Job == nil {
			add("%s requires a job object", TypeJobFinished)
		} else if !slices.Contains(Outcomes, e.Job.Outcome) {
			add("job.outcome %q is not one of %s", e.Job.Outcome, strings.Join(Outcomes, ", "))
		} else if strings.TrimSpace(e.Job.Queue) == "" {
			add("%s requires job.queue: metrics are aggregated per queue", TypeJobFinished)
		}
		// A record without an identity cannot be attributed to a job, so it is refused
		// rather than stored as an anonymous row. Laravel assigns a uuid when it pushes
		// a job, but a job that reached the queue some other way has none, so the queue
		// job id is the identity that must be there.
		if e.Job != nil {
			if strings.TrimSpace(e.Job.ID) == "" {
				add("%s requires job.id: it identifies a job Laravel gave no uuid", TypeJobFinished)
			}
			if strings.TrimSpace(e.Job.Class) == "" {
				add("%s requires job.class: it is what an operator reads in history", TypeJobFinished)
			}
			if e.Job.Attempts < 0 {
				add("job.attempts must be zero or positive, got %d", e.Job.Attempts)
			}
			if e.Job.Exception != nil && strings.TrimSpace(e.Job.Exception.Class) == "" {
				add("job.exception requires a class: it is the only part an operator can act on")
			}
		}
	case TypeWorkerStopped:
		if e.WorkerEvent == nil {
			add("%s requires a worker_event object", TypeWorkerStopped)
		} else if strings.TrimSpace(e.WorkerEvent.Reason) == "" {
			add("%s requires worker_event.reason", TypeWorkerStopped)
		}
		if strings.TrimSpace(e.Worker) == "" {
			add("%s requires the worker name", TypeWorkerStopped)
		}
	default:
		add("type %q is not supported", e.Type)
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid telemetry event: %s", strings.Join(problems, "; "))
	}
	return nil
}
