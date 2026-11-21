package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gohorizon/gohorizon/internal/redis"
)

type readResult struct {
	messages []redis.StreamMessage
	err      error
}

// fakeStream stands in for Redis: reads are served from a queue, pending entries from
// pending, and an empty read waits like a real blocking read would.
type fakeStream struct {
	mu        sync.Mutex
	ensures   int
	ensureErr error
	pending   []redis.StreamMessage
	reads     []readResult
	acked     []string
	ackErr    error
}

func (f *fakeStream) EnsureEventsGroup(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensures++
	return f.ensureErr
}

func (f *fakeStream) ReadEventsGroup(_ context.Context, _, _, id string, _ int64, block time.Duration) ([]redis.StreamMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if id == "0" {
		messages := f.pending
		f.pending = nil
		return messages, nil
	}
	if len(f.reads) == 0 {
		if block > 0 {
			time.Sleep(time.Millisecond)
		}
		return nil, nil
	}
	result := f.reads[0]
	f.reads = f.reads[1:]
	return result.messages, result.err
}

func (f *fakeStream) AckEvents(_ context.Context, _ string, ids ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ackErr != nil {
		return f.ackErr
	}
	f.acked = append(f.acked, ids...)
	return nil
}

func (f *fakeStream) ackedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.acked...)
}

func (f *fakeStream) ensureCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ensures
}

func testConsumer(t *testing.T, streams Stream) *Consumer {
	t.Helper()
	consumer, err := New(streams, Spec{
		Application: "production",
		Environment: "production",
		Block:       time.Millisecond,
		MinBackoff:  time.Millisecond,
		MaxBackoff:  5 * time.Millisecond,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return consumer
}

func payloadEntry(entryID, eventID string) redis.StreamMessage {
	return redis.StreamMessage{ID: entryID, Fields: map[string]string{"payload": jobEvent(eventID, "processed")}}
}

func TestHandleAcknowledgesEveryEventItCounts(t *testing.T) {
	stream := &fakeStream{}
	consumer := testConsumer(t, stream)

	err := consumer.handle(context.Background(), []redis.StreamMessage{
		payloadEntry("1-0", "a"),
		payloadEntry("2-0", "b"),
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}

	if got := consumer.Stats().Events; got != 2 {
		t.Errorf("events = %d, want 2", got)
	}
	if got := stream.ackedIDs(); len(got) != 2 {
		t.Errorf("acked = %v, want both entries", got)
	}
}

func TestHandleDeduplicatesByEventID(t *testing.T) {
	stream := &fakeStream{}
	consumer := testConsumer(t, stream)

	err := consumer.handle(context.Background(), []redis.StreamMessage{
		payloadEntry("1-0", "same"),
		payloadEntry("2-0", "same"),
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}

	stats := consumer.Stats()
	if stats.Events != 1 || stats.Duplicates != 1 {
		t.Errorf("stats = %+v, want one event and one duplicate", stats)
	}
	// The duplicate must still be acknowledged, or it would be redelivered forever.
	if got := stream.ackedIDs(); len(got) != 2 {
		t.Errorf("acked = %v, want both entries", got)
	}
}

func TestHandleDiscardsInvalidEntriesButAcknowledgesThem(t *testing.T) {
	stream := &fakeStream{}
	consumer := testConsumer(t, stream)

	err := consumer.handle(context.Background(), []redis.StreamMessage{
		{ID: "1-0", Fields: map[string]string{}},
		{ID: "2-0", Fields: map[string]string{"payload": `{"schema":99}`}},
		{ID: "3-0", Fields: map[string]string{"payload": jobEvent("ok", "processed")}},
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}

	stats := consumer.Stats()
	if stats.Invalid != 2 || stats.Events != 1 {
		t.Errorf("stats = %+v, want two invalid and one event", stats)
	}
	if got := stream.ackedIDs(); len(got) != 3 {
		t.Errorf("acked = %v, want all three entries: poison entries must not loop", got)
	}
}

func TestHandleIgnoresEventsFromAnotherApplication(t *testing.T) {
	stream := &fakeStream{}
	consumer := testConsumer(t, stream)

	payload := strings.Replace(jobEvent("a", "processed"), `"environment":"production"`, `"environment":"staging"`, 1)

	err := consumer.handle(context.Background(), []redis.StreamMessage{
		{ID: "1-0", Fields: map[string]string{"payload": payload}},
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}

	if stats := consumer.Stats(); stats.Invalid != 1 || stats.Events != 0 {
		t.Errorf("stats = %+v, want the foreign event discarded", stats)
	}
	if got := stream.ackedIDs(); len(got) != 1 {
		t.Errorf("acked = %v, want the foreign entry acknowledged", got)
	}
}

func TestDrainReplaysPendingEntriesAndDeduplicatesThem(t *testing.T) {
	stream := &fakeStream{pending: []redis.StreamMessage{
		payloadEntry("5-0", "already"),
		payloadEntry("6-0", "fresh"),
	}}
	consumer := testConsumer(t, stream)

	// The consumer handed "already" before it stopped without acknowledging it.
	if err := consumer.handle(context.Background(), []redis.StreamMessage{payloadEntry("4-0", "already")}); err != nil {
		t.Fatalf("handle: %v", err)
	}

	consumer.drain(context.Background())

	stats := consumer.Stats()
	if stats.Events != 2 || stats.Duplicates != 1 {
		t.Errorf("stats = %+v, want the replayed event deduplicated", stats)
	}
}

func TestDrainIngestsWhatWasPublishedAfterTheLoopStopped(t *testing.T) {
	stream := &fakeStream{reads: []readResult{
		{messages: []redis.StreamMessage{payloadEntry("9-0", "late")}},
	}}
	consumer := testConsumer(t, stream)

	consumer.Drain(context.Background())

	if stats := consumer.Stats(); stats.Events != 1 {
		t.Errorf("events = %d, want the entry published after the loop stopped", stats.Events)
	}
	if got := stream.ackedIDs(); len(got) != 1 || got[0] != "9-0" {
		t.Errorf("acked = %v, want the late entry acknowledged", got)
	}
}

func TestHandleDoesNotAcknowledgeWhenRedisRejectsIt(t *testing.T) {
	stream := &fakeStream{ackErr: errors.New("redis is down")}
	consumer := testConsumer(t, stream)

	if err := consumer.handle(context.Background(), []redis.StreamMessage{payloadEntry("1-0", "a")}); err == nil {
		t.Fatal("handle reported success although the ack failed")
	}
	if stats := consumer.Stats(); stats.Errors != 1 {
		t.Errorf("errors = %d, want 1", stats.Errors)
	}
}

func TestRunKeepsConsumingAfterRedisErrors(t *testing.T) {
	stream := &fakeStream{reads: []readResult{
		{err: errors.New("redis connection lost")},
		{messages: []redis.StreamMessage{payloadEntry("1-0", "a")}},
	}}
	consumer := testConsumer(t, stream)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		consumer.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	waitFor(t, 3*time.Second, func() bool { return consumer.Stats().Events == 1 }, "the event after a read error")
	if stats := consumer.Stats(); stats.Errors == 0 {
		t.Error("the read error was not counted")
	}
	if got := stream.ackedIDs(); len(got) != 1 || got[0] != "1-0" {
		t.Errorf("acked = %v, want the event acknowledged", got)
	}
	if stats := consumer.Stats(); stats.Failing {
		t.Error("ingestion still reports a live failure after a successful read")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop when the context was cancelled")
	}
}

func TestRunRetriesGroupCreationInsteadOfGivingUp(t *testing.T) {
	stream := &fakeStream{ensureErr: errors.New("redis is down")}
	consumer := testConsumer(t, stream)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		consumer.Run(ctx)
	}()

	waitFor(t, 3*time.Second, func() bool { return stream.ensureCalls() >= 2 }, "group creation to be retried")
	select {
	case <-done:
		t.Fatal("Run gave up while Redis was unavailable; workers would lose telemetry silently")
	default:
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop when the context was cancelled")
	}
	if stats := consumer.Stats(); stats.Errors < 2 {
		t.Errorf("errors = %d, want every failed attempt counted", stats.Errors)
	}
}

func TestWorkerStoppedDeliversItsReasonOnce(t *testing.T) {
	var got []string
	consumer, err := New(&fakeStream{}, Spec{
		Application: "production",
		Environment: "production",
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnWorkerStopped: func(worker, reason string) {
			got = append(got, worker+" "+reason)
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	payload := `{"schema":1,"id":"w-1","type":"worker.stopped",` +
		`"application":"production","environment":"production",` +
		`"worker":"gohorizon:production:production:default-supervisor:1a2b3c4d",` +
		`"at":"2026-11-20T10:11:12.345Z",` +
		`"worker_event":{"exit_code":1,"reason":"timed_out"}}`
	entries := []redis.StreamMessage{{ID: "1-0", Fields: map[string]string{"payload": payload}}}

	if err := consumer.handle(context.Background(), entries); err != nil {
		t.Fatalf("handle: %v", err)
	}
	// A duplicate must not move a worker's exit classification twice.
	if err := consumer.handle(context.Background(), entries); err != nil {
		t.Fatalf("handle duplicate: %v", err)
	}

	want := "gohorizon:production:production:default-supervisor:1a2b3c4d timed_out"
	if len(got) != 1 || got[0] != want {
		t.Errorf("hook calls = %v, want exactly [%q]", got, want)
	}
}

type recordingSink struct {
	mu   sync.Mutex
	jobs []string
	err  error
}

func (r *recordingSink) RecordJob(_ context.Context, eventID, queue, outcome string, durationMS int64, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.jobs = append(r.jobs, fmt.Sprintf("%s %s %s %d", eventID, queue, outcome, durationMS))
	return nil
}

func (r *recordingSink) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.jobs...)
}

func (r *recordingSink) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func TestJobEventsReachTheSinkOnce(t *testing.T) {
	sink := &recordingSink{}
	consumer, err := New(&fakeStream{}, Spec{
		Application: "production",
		Environment: "production",
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sink:        sink,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	entries := []redis.StreamMessage{payloadEntry("1-0", "evt-1")}
	if err := consumer.handle(context.Background(), entries); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if err := consumer.handle(context.Background(), entries); err != nil {
		t.Fatalf("handle duplicate: %v", err)
	}

	want := []string{"evt-1 emails processed 1841"}
	if got := sink.recorded(); len(got) != 1 || got[0] != want[0] {
		t.Errorf("sink records = %v, want %v once", got, want)
	}
	if stats := consumer.Stats(); stats.Events != 1 || stats.Duplicates != 1 {
		t.Errorf("stats = %+v, want one event and one duplicate", stats)
	}
}

func TestJobMetricsSurviveASinkFailure(t *testing.T) {
	stream := &fakeStream{pending: []redis.StreamMessage{payloadEntry("1-0", "evt-1")}}
	sink := &recordingSink{err: errors.New("disk is full")}
	consumer, err := New(stream, Spec{
		Application: "production",
		Environment: "production",
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sink:        sink,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := consumer.handle(context.Background(), []redis.StreamMessage{payloadEntry("1-0", "evt-1")}); err == nil {
		t.Fatal("handle reported success although the sink failed")
	}
	if got := stream.ackedIDs(); len(got) != 0 {
		t.Fatalf("acked = %v, want nothing acknowledged while the metric is unsaved", got)
	}
	if stats := consumer.Stats(); stats.Errors == 0 || !stats.Failing {
		t.Errorf("stats = %+v, want the failure counted and visible", stats)
	}

	// The storage comes back: the pending entry is replayed and recorded, because the
	// event was never marked as seen before the write succeeded.
	sink.fail(nil)
	consumer.drain(context.Background())

	if got := sink.recorded(); len(got) != 1 {
		t.Errorf("sink records = %v, want the retried job recorded once", got)
	}
	if got := stream.ackedIDs(); len(got) != 1 || got[0] != "1-0" {
		t.Errorf("acked = %v, want the entry acknowledged after the retry", got)
	}
}

func TestFailureIsLiveAndRecoveryClearsIt(t *testing.T) {
	consumer := testConsumer(t, &fakeStream{})

	consumer.failed(errors.New("redis is down"))

	stats := consumer.Stats()
	if !stats.Failing || stats.LastError != "redis is down" || stats.Errors != 1 {
		t.Fatalf("stats = %+v, want a live failure", stats)
	}

	consumer.healthy()

	stats = consumer.Stats()
	if stats.Failing || stats.LastError != "" {
		t.Errorf("stats = %+v, want the failure cleared after a successful read", stats)
	}
	if stats.Errors != 1 {
		t.Errorf("errors = %d, want the history kept", stats.Errors)
	}
}

func TestSeenSetForgetsIdsOutsideItsWindow(t *testing.T) {
	seen := newSeenSet(2)

	for _, id := range []string{"a", "b", "c"} {
		if !seen.add(id) {
			t.Fatalf("add(%q) reported a duplicate", id)
		}
	}
	if seen.add("c") {
		t.Error("add(c) accepted an id that is still in the window")
	}
	// The window holds b and c; re-admitting a evicts b.
	if !seen.add("a") {
		t.Error("add(a) rejected an id evicted from the window")
	}
	if seen.add("a") {
		t.Error("add(a) accepted an id that is still in the window")
	}
	if !seen.add("b") {
		t.Error("add(b) rejected an id the re-admitted a pushed out")
	}
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, description)
}
