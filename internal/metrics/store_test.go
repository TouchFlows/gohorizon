package metrics

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openStore(t *testing.T, path string) *Store {
	t.Helper()
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return store
}

func at(minute int) time.Time {
	return time.Date(2026, 11, 20, 10, minute, 30, 0, time.UTC)
}

func totalJobs(bucket Bucket) int64 {
	return bucket.Processed + bucket.Released + bucket.Failed + bucket.TimedOut
}

func TestRecordAggregatesPerQueuePerMinuteAndOutcome(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "metrics.db"))
	ctx := context.Background()

	records := []struct {
		id       string
		queue    string
		outcome  string
		duration int64
		at       time.Time
	}{
		{"e1", "emails", "processed", 1200, at(1)},
		{"e2", "emails", "processed", 800, at(1)},
		{"e3", "emails", "failed", 500, at(1)},
		{"e4", "emails", "timed_out", 60000, at(2)},
		{"e5", "reports", "released", 40, at(1)},
	}
	for _, record := range records {
		if err := store.RecordJob(ctx, record.id, record.queue, record.outcome, record.duration, record.at); err != nil {
			t.Fatalf("RecordJob(%s): %v", record.id, err)
		}
	}

	emails, err := store.Buckets(ctx, "emails", at(0), at(3))
	if err != nil {
		t.Fatalf("Buckets: %v", err)
	}
	if len(emails) != 2 {
		t.Fatalf("emails buckets = %d, want 2 (one per minute)", len(emails))
	}
	first := emails[0]
	if totalJobs(first) != 3 || first.Processed != 2 || first.Failed != 1 || first.Released != 0 {
		t.Errorf("first bucket = %+v, want 3 jobs, 2 processed, 1 failed", first)
	}
	if first.DurationMS != 2500 {
		t.Errorf("first bucket duration = %d, want 2500", first.DurationMS)
	}
	if !first.Start.Equal(at(1).Truncate(time.Minute)) {
		t.Errorf("bucket start = %s, want the minute of 10:01", first.Start)
	}
	if second := emails[1]; second.TimedOut != 1 || totalJobs(second) != 1 {
		t.Errorf("second bucket = %+v, want the timed-out job alone", second)
	}

	reports, err := store.Buckets(ctx, "reports", at(0), at(3))
	if err != nil {
		t.Fatalf("Buckets(reports): %v", err)
	}
	if len(reports) != 1 || reports[0].Released != 1 {
		t.Errorf("reports = %+v, want one released job", reports)
	}
}

func TestRecordIsIdempotentByEventID(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "metrics.db"))
	ctx := context.Background()

	if err := store.RecordJob(ctx, "same", "emails", "processed", 100, at(1)); err != nil {
		t.Fatalf("RecordJob: %v", err)
	}
	// The stream is at-least-once: the same event id can arrive again after a crash.
	if err := store.RecordJob(ctx, "same", "emails", "processed", 100, at(1)); err != nil {
		t.Fatalf("RecordJob replay: %v", err)
	}

	buckets, err := store.Buckets(ctx, "emails", at(0), at(3))
	if err != nil {
		t.Fatalf("Buckets: %v", err)
	}
	if len(buckets) != 1 || totalJobs(buckets[0]) != 1 || buckets[0].DurationMS != 100 {
		t.Fatalf("buckets = %+v, want the event counted once", buckets)
	}
}

func TestHistorySurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.db")
	ctx := context.Background()

	first, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := first.RecordJob(ctx, "e1", "emails", "processed", 750, at(1)); err != nil {
		t.Fatalf("RecordJob: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A restarted supervisor opens the same file; the history must still be there.
	second := openStore(t, path)
	buckets, err := second.Buckets(ctx, "emails", at(0), at(3))
	if err != nil {
		t.Fatalf("Buckets after reopen: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Processed != 1 || buckets[0].DurationMS != 750 {
		t.Fatalf("buckets after reopen = %+v, want the persisted aggregate", buckets)
	}

	// The id survives too, so a replay after the restart is still absorbed.
	if err := second.RecordJob(ctx, "e1", "emails", "processed", 750, at(1)); err != nil {
		t.Fatalf("RecordJob after reopen: %v", err)
	}
	buckets, err = second.Buckets(ctx, "emails", at(0), at(3))
	if err != nil {
		t.Fatalf("Buckets: %v", err)
	}
	if jobs := totalJobs(buckets[0]); jobs != 1 {
		t.Errorf("jobs = %d, want the replayed event absorbed after restart", jobs)
	}
}

func TestPruneDropsHistoryPastTheHandoffWindows(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "metrics.db"))
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	records := []struct {
		id   string
		at   time.Time
		note string
	}{
		{"fresh", now.Add(-time.Hour), "within both windows"},
		{"raw-expired", now.Add(-rawRetention - time.Hour), "id expired, aggregate stays"},
		{"minute-expired", now.Add(-minuteRetention - time.Hour), "both expired"},
	}
	for _, record := range records {
		if err := store.RecordJob(ctx, record.id, "emails", "processed", 10, record.at); err != nil {
			t.Fatalf("RecordJob(%s): %v", record.id, err)
		}
	}

	pruned, err := store.Prune(ctx, now)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	// Two ids expire (72h) but only one bucket (7 days): the 4-day-old aggregate stays.
	if pruned.Events != 2 || pruned.Buckets != 1 {
		t.Errorf("pruned = %+v, want 2 event ids and 1 bucket", pruned)
	}

	buckets, err := store.Buckets(ctx, "emails", now.Add(-minuteRetention), now)
	if err != nil {
		t.Fatalf("Buckets: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("buckets = %d, want the fresh and the 4-day-old minute", len(buckets))
	}
	if totalJobs(buckets[len(buckets)-1]) != 1 || buckets[len(buckets)-1].DurationMS != 10 {
		t.Errorf("newest bucket = %+v, want the fresh job", buckets[len(buckets)-1])
	}

	// Pruning is idempotent: a second pass finds nothing to do.
	again, err := store.Prune(ctx, now)
	if err != nil {
		t.Fatalf("Prune again: %v", err)
	}
	if again.Events != 0 || again.Buckets != 0 {
		t.Errorf("second prune = %+v, want nothing left to remove", again)
	}
}

func TestPruneKeepsRowsJustInsideTheWindow(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "metrics.db"))
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	// One second inside the raw window: its id survives, so a replay is still absorbed.
	rawEdge := now.Add(-rawRetention + time.Second)
	if err := store.RecordJob(ctx, "raw-edge", "emails", "processed", 10, rawEdge); err != nil {
		t.Fatalf("RecordJob: %v", err)
	}
	// One second inside the minute window: the aggregate stays even though its id is
	// past the 72-hour raw window (the documented tradeoff).
	bucketEdge := now.Add(-minuteRetention + time.Second)
	if err := store.RecordJob(ctx, "bucket-edge", "emails", "processed", 10, bucketEdge); err != nil {
		t.Fatalf("RecordJob: %v", err)
	}

	pruned, err := store.Prune(ctx, now)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if pruned.Events != 1 || pruned.Buckets != 0 {
		t.Errorf("pruned = %+v, want only the 7-day-old event id removed", pruned)
	}

	if err := store.RecordJob(ctx, "raw-edge", "emails", "processed", 10, rawEdge); err != nil {
		t.Fatalf("RecordJob replay: %v", err)
	}
	buckets, err := store.Buckets(ctx, "emails", now.Add(-minuteRetention), now)
	if err != nil {
		t.Fatalf("Buckets: %v", err)
	}
	var jobs int64
	for _, bucket := range buckets {
		jobs += totalJobs(bucket)
	}
	if jobs != 2 {
		t.Errorf("jobs = %d over %d buckets, want both aggregates and the replay absorbed", jobs, len(buckets))
	}
}

func TestOpenCreatesTheParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state", "metrics.db")

	store := openStore(t, path)
	if err := store.RecordJob(context.Background(), "e1", "emails", "processed", 1, at(1)); err != nil {
		t.Fatalf("RecordJob: %v", err)
	}
}

func TestRecordRejectsRecordsItCannotAggregate(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "metrics.db"))
	ctx := context.Background()

	tests := []struct {
		name    string
		eventID string
		queue   string
		outcome string
		start   time.Time
	}{
		{"unknown outcome", "e1", "emails", "exploded", at(1)},
		{"missing queue", "e2", "", "processed", at(1)},
		{"missing event id", "", "emails", "processed", at(1)},
		{"missing time", "e3", "emails", "processed", time.Time{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := store.RecordJob(ctx, tc.eventID, tc.queue, tc.outcome, 1, tc.start); err == nil {
				t.Fatal("RecordJob accepted a record it cannot aggregate")
			}
		})
	}
}
