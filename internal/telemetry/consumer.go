package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gohorizon/gohorizon/internal/redis"
)

const (
	// DefaultGroup and DefaultConsumer are fixed on purpose: the supervisor lock
	// guarantees one GoHorizon per application, and reusing the consumer name means a
	// restart replays its own unacknowledged entries instead of orphaning them.
	DefaultGroup    = "gohorizon"
	DefaultConsumer = "gohorizon"

	DefaultBatch      int64 = 100
	DefaultBlock            = 2 * time.Second
	DefaultSeen             = 10000
	DefaultMinBackoff       = time.Second
	DefaultMaxBackoff       = 30 * time.Second
)

// Stream is the Redis surface the consumer needs. *redis.Client implements it.
type Stream interface {
	EnsureEventsGroup(ctx context.Context, group string) error
	ReadEventsGroup(ctx context.Context, group, consumer, id string, count int64, block time.Duration) ([]redis.StreamMessage, error)
	AckEvents(ctx context.Context, group string, ids ...string) error
}

// Spec configures a Consumer. Zero values take the defaults above.
type Spec struct {
	Application string
	Environment string
	Block       time.Duration
	MinBackoff  time.Duration
	MaxBackoff  time.Duration
	Logger      *slog.Logger

	// OnWorkerStopped receives the reason from a worker.stopped event. It runs on the
	// consumer goroutine while the event is acknowledged, so it must not block or
	// panic: a slow hook delays ingestion, never a job.
	OnWorkerStopped func(worker, reason string)

	// Sink persists job metrics. It must be idempotent by event id: a failed batch is
	// retried, and a crash can replay an entry that was already handled. A sink error
	// leaves the batch unacknowledged and is counted, so history loss is visible and
	// retried instead of silent.
	Sink JobSink
}

// JobSink receives one finished job. *metrics.Store implements it.
type JobSink interface {
	RecordJob(ctx context.Context, eventID, queue, outcome string, durationMS int64, at time.Time) error
}

// Stats is what the consumer has observed. Failing describes right now, not history.
type Stats struct {
	Events     int
	Duplicates int
	Invalid    int
	Errors     int
	Failing    bool
	LastError  string
}

// Consumer reads the events stream in a consumer group, deduplicates by event id and
// acknowledges what it has handled.
type Consumer struct {
	streams Stream
	spec    Spec
	log     *slog.Logger
	seen    *seenSet

	mu    sync.Mutex
	stats Stats
}

func New(streams Stream, spec Spec) (*Consumer, error) {
	if streams == nil {
		return nil, errors.New("telemetry consumer requires a stream")
	}
	if spec.Application == "" || spec.Environment == "" {
		return nil, errors.New("telemetry consumer requires the application and environment it consumes for")
	}
	if spec.Block == 0 {
		spec.Block = DefaultBlock
	}
	if spec.MinBackoff <= 0 {
		spec.MinBackoff = DefaultMinBackoff
	}
	if spec.MaxBackoff < spec.MinBackoff {
		spec.MaxBackoff = DefaultMaxBackoff
	}
	if spec.Logger == nil {
		spec.Logger = slog.Default()
	}

	return &Consumer{
		streams: streams,
		spec:    spec,
		log:     spec.Logger.With("component", "telemetry"),
		seen:    newSeenSet(DefaultSeen),
	}, nil
}

// Run consumes until the context is cancelled. Redis failures are logged and retried
// with backoff; they never reach the workers and never end supervision (handoff
// §2.11, docs/adr/0006).
func (c *Consumer) Run(ctx context.Context) {
	c.log.Info("telemetry consumer started", "group", DefaultGroup, "consumer", DefaultConsumer)

	backoff := c.spec.MinBackoff
	for ctx.Err() == nil {
		if err := c.streams.EnsureEventsGroup(ctx, DefaultGroup); err != nil {
			c.failed(err)
			if !wait(ctx, backoff) {
				return
			}
			backoff = grow(backoff, c.spec.MaxBackoff)
			continue
		}

		c.drain(ctx)
		if !c.consume(ctx, &backoff) {
			return
		}
	}
}

// consume reads new entries until the context ends. It returns false when Run should
// stop and true when the group should be re-established (Redis may have been flushed).
func (c *Consumer) consume(ctx context.Context, backoff *time.Duration) bool {
	for ctx.Err() == nil {
		messages, err := c.streams.ReadEventsGroup(ctx, DefaultGroup, DefaultConsumer, ">", DefaultBatch, c.spec.Block)
		if err != nil {
			c.failed(err)
			if !wait(ctx, *backoff) {
				return false
			}
			*backoff = grow(*backoff, c.spec.MaxBackoff)
			return true
		}

		c.healthy()
		*backoff = c.spec.MinBackoff
		if err := c.handle(ctx, messages); err != nil {
			// Unacknowledged entries are replayed by drain on the next pass.
			if !wait(ctx, c.spec.MinBackoff) {
				return false
			}
			return true
		}
	}
	return false
}

// drain replays the entries this consumer read but never acknowledged. Their ids are
// already in the seen set, so a replay after a failed ack is absorbed instead of
// counted twice.
func (c *Consumer) drain(ctx context.Context) {
	for ctx.Err() == nil {
		messages, err := c.streams.ReadEventsGroup(ctx, DefaultGroup, DefaultConsumer, "0", DefaultBatch, -1)
		if err != nil {
			c.failed(err)
			return
		}
		if err := c.handle(ctx, messages); err != nil {
			return
		}
		if int64(len(messages)) < DefaultBatch {
			return
		}
	}
}

// handle validates, deduplicates and acknowledges one batch. It returns an error only
// when acknowledgement failed.
func (c *Consumer) handle(ctx context.Context, messages []redis.StreamMessage) error {
	acked := make([]string, 0, len(messages))

	for _, message := range messages {
		payload, ok := message.Fields["payload"]
		if !ok {
			c.discard(message.ID, "entry has no payload field")
			acked = append(acked, message.ID)
			continue
		}

		event, err := Decode(payload)
		if err != nil {
			c.discard(message.ID, err.Error())
			acked = append(acked, message.ID)
			continue
		}
		if event.Application != c.spec.Application || event.Environment != c.spec.Environment {
			c.discard(message.ID, fmt.Sprintf("event belongs to %s/%s", event.Application, event.Environment))
			acked = append(acked, message.ID)
			continue
		}

		if c.seen.has(event.ID) {
			c.count(func(n *Stats) { n.Duplicates++ })
			acked = append(acked, message.ID)
			continue
		}

		if event.Type == TypeJobFinished && c.spec.Sink != nil {
			if err := c.spec.Sink.RecordJob(ctx, event.ID, event.Job.Queue, event.Job.Outcome, event.Job.DurationMS, event.At); err != nil {
				c.failed(err)
				// Nothing in this batch is acknowledged: the drain path retries it, and
				// the sink's idempotency keeps the retry from counting twice.
				return err
			}
		}

		c.seen.add(event.ID)
		c.count(func(n *Stats) { n.Events++ })
		if event.Type == TypeWorkerStopped && c.spec.OnWorkerStopped != nil {
			c.spec.OnWorkerStopped(event.Worker, event.WorkerEvent.Reason)
		}
		acked = append(acked, message.ID)
	}

	if len(acked) == 0 {
		return nil
	}
	if err := c.streams.AckEvents(ctx, DefaultGroup, acked...); err != nil {
		c.failed(err)
		return err
	}
	return nil
}

// Stats returns a point-in-time copy of the counters.
func (c *Consumer) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

func (c *Consumer) count(apply func(*Stats)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	apply(&c.stats)
}

// discard counts and logs an event that will not be retried; a malformed entry that
// stays unacknowledged would be redelivered forever.
func (c *Consumer) discard(entryID, reason string) {
	c.count(func(n *Stats) { n.Invalid++ })
	c.log.Warn("discarded telemetry event", "entry", entryID, "reason", reason)
}

// failed marks ingestion as currently failing, which /api/v1/health reports. The
// workers never see it.
func (c *Consumer) failed(err error) {
	c.count(func(n *Stats) {
		n.Errors++
		n.Failing = true
		n.LastError = err.Error()
	})
	c.log.Warn("telemetry consumer degraded; workers are unaffected", "error", err)
}

// healthy clears the live failure once a read succeeds, so a recovered Redis stops
// reporting telemetry as degraded without losing the error count.
func (c *Consumer) healthy() {
	c.count(func(n *Stats) {
		n.Failing = false
		n.LastError = ""
	})
}

func wait(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func grow(current, max time.Duration) time.Duration {
	if next := current * 2; next < max {
		return next
	}
	return max
}

// seenSet remembers event ids in a fixed-size window. It is bounded so a long-running
// stream cannot grow the supervisor's memory; an id that falls out of the window is
// accepted again, which is why ack failures are retried immediately instead of much
// later (docs/adr/0006).
type seenSet struct {
	capacity int
	order    []string
	head     int
	index    map[string]struct{}
}

func newSeenSet(capacity int) *seenSet {
	return &seenSet{capacity: capacity, index: make(map[string]struct{}, capacity)}
}

// has reports whether the id is still in the window.
func (s *seenSet) has(id string) bool {
	_, ok := s.index[id]
	return ok
}

// add records the id and reports whether it was new.
func (s *seenSet) add(id string) bool {
	if _, ok := s.index[id]; ok {
		return false
	}

	if len(s.order)-s.head >= s.capacity {
		delete(s.index, s.order[s.head])
		s.head++
	}
	s.order = append(s.order, id)
	s.index[id] = struct{}{}

	if s.head == s.capacity {
		s.order = append([]string(nil), s.order[s.head:]...)
		s.head = 0
	}
	return true
}
