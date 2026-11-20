// Package telemetry consumes the lifecycle events the gohorizon/laravel package
// publishes to the Redis Stream.
//
// This is observation, never an execution ledger (handoff §14.7): the consumer
// validates, deduplicates and acknowledges events, and keeps counters. Persisting
// them (metrics history) and the dashboard are later slices.
package telemetry

import (
	"encoding/json"
	"fmt"
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

// Outcome values the Laravel package derives from the job flags.
var outcomes = map[string]bool{
	"processed": true,
	"released":  true,
	"failed":    true,
	"timed_out": true,
}

// Event is a telemetry record. Only the fields the consumer acts on are decoded; the
// rest of the payload is ignored so the schema can grow without breaking ingestion.
type Event struct {
	Schema      int       `json:"schema"`
	ID          string    `json:"id"`
	Type        string    `json:"type"`
	Application string    `json:"application"`
	Environment string    `json:"environment"`
	At          time.Time `json:"at"`

	Job         *jobPayload    `json:"job,omitempty"`
	WorkerEvent *workerPayload `json:"worker_event,omitempty"`
}

type jobPayload struct {
	Outcome string `json:"outcome"`
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
		} else if !outcomes[e.Job.Outcome] {
			add("job.outcome %q is not one of processed, released, failed, timed_out", e.Job.Outcome)
		}
	case TypeWorkerStopped:
		if e.WorkerEvent == nil {
			add("%s requires a worker_event object", TypeWorkerStopped)
		}
	default:
		add("type %q is not supported", e.Type)
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid telemetry event: %s", strings.Join(problems, "; "))
	}
	return nil
}
