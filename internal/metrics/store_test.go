package metrics

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openStore(t *testing.T, path string) *Store {
	t.Helper()
	store, err := Open(path, DefaultRetention())
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

	emails, err := store.Buckets(ctx, ResolutionMinute, "emails", at(0), at(3))
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

	reports, err := store.Buckets(ctx, ResolutionMinute, "reports", at(0), at(3))
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

	buckets, err := store.Buckets(ctx, ResolutionMinute, "emails", at(0), at(3))
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

	first, err := Open(path, DefaultRetention())
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
	buckets, err := second.Buckets(ctx, ResolutionMinute, "emails", at(0), at(3))
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
	buckets, err = second.Buckets(ctx, ResolutionMinute, "emails", at(0), at(3))
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
		id string
		at time.Time
	}{
		{"fresh", now.Add(-time.Hour)},
		{"raw-expired", now.Add(-rawRetention - time.Hour)},
		{"minute-expired", now.Add(-minuteRetention - time.Hour)},
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

	buckets, err := store.Buckets(ctx, ResolutionMinute, "emails", now.Add(-minuteRetention), now)
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
	buckets, err := store.Buckets(ctx, ResolutionMinute, "emails", now.Add(-minuteRetention), now)
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

func TestRollupFoldsClosedMinutesIntoHoursAndDays(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "metrics.db"))
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 12, 30, 0, 0, time.UTC)

	records := []struct {
		id, queue, outcome string
		duration           int64
		at                 time.Time
	}{
		{"a", "emails", "processed", 100, time.Date(2026, 11, 20, 10, 5, 0, 0, time.UTC)},
		{"b", "emails", "failed", 200, time.Date(2026, 11, 20, 10, 35, 0, 0, time.UTC)},
		{"c", "emails", "processed", 300, time.Date(2026, 11, 20, 11, 20, 0, 0, time.UTC)},
		{"d", "reports", "released", 50, time.Date(2026, 11, 20, 10, 10, 0, 0, time.UTC)},
		// The hour in progress is not folded yet.
		{"e", "emails", "processed", 400, time.Date(2026, 11, 20, 12, 10, 0, 0, time.UTC)},
	}
	for _, record := range records {
		if err := store.RecordJob(ctx, record.id, record.queue, record.outcome, record.duration, record.at); err != nil {
			t.Fatalf("RecordJob(%s): %v", record.id, err)
		}
	}
	if err := store.Rollup(ctx, now); err != nil {
		t.Fatalf("Rollup: %v", err)
	}

	hours, err := store.Buckets(ctx, ResolutionHourly, "emails", now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("hourly Buckets: %v", err)
	}
	if len(hours) != 2 {
		t.Fatalf("hours = %+v, want one for 10:00 and one for 11:00", hours)
	}
	ten := hours[0]
	if !ten.Start.Equal(time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)) ||
		ten.Processed != 1 || ten.Failed != 1 || ten.DurationMS != 300 {
		t.Errorf("10:00 hour = %+v, want both 10:xx minutes summed", ten)
	}
	if eleven := hours[1]; eleven.Start.Hour() != 11 || eleven.Processed != 1 || eleven.DurationMS != 300 {
		t.Errorf("11:00 hour = %+v, want the single 11:20 job", eleven)
	}

	days, err := store.Buckets(ctx, ResolutionDaily, "emails", now.Add(-7*24*time.Hour), now)
	if err != nil {
		t.Fatalf("daily Buckets: %v", err)
	}
	if len(days) != 1 {
		t.Fatalf("days = %+v, want one", days)
	}
	day := days[0]
	if !day.Start.Equal(time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)) ||
		totalJobs(day) != 3 || day.DurationMS != 600 {
		t.Errorf("day = %+v, want only the three closed hours", day)
	}

	// Queues roll up independently.
	reports, err := store.Buckets(ctx, ResolutionHourly, "reports", now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("hourly Buckets(reports): %v", err)
	}
	if len(reports) != 1 || reports[0].Released != 1 {
		t.Errorf("reports hours = %+v, want the released job", reports)
	}
}

func TestRollupIsIdempotentAndAbsorbsLateMinutes(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "metrics.db"))
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 12, 30, 0, 0, time.UTC)

	if err := store.RecordJob(ctx, "a", "emails", "processed", 100, time.Date(2026, 11, 20, 10, 5, 0, 0, time.UTC)); err != nil {
		t.Fatalf("RecordJob: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := store.Rollup(ctx, now); err != nil {
			t.Fatalf("Rollup %d: %v", i, err)
		}
	}

	hours, err := store.Buckets(ctx, ResolutionHourly, "emails", now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("hourly Buckets: %v", err)
	}
	if len(hours) != 1 || hours[0].Processed != 1 || hours[0].DurationMS != 100 {
		t.Fatalf("hours = %+v, want the job counted once after two rollups", hours)
	}

	// A stream entry delivered after its hour was folded is picked up by the next pass.
	if err := store.RecordJob(ctx, "late", "emails", "failed", 50, time.Date(2026, 11, 20, 10, 40, 0, 0, time.UTC)); err != nil {
		t.Fatalf("RecordJob(late): %v", err)
	}
	if err := store.Rollup(ctx, now); err != nil {
		t.Fatalf("Rollup after late minute: %v", err)
	}
	hours, err = store.Buckets(ctx, ResolutionHourly, "emails", now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("hourly Buckets: %v", err)
	}
	if len(hours) != 1 || hours[0].Processed != 1 || hours[0].Failed != 1 || hours[0].DurationMS != 150 {
		t.Errorf("hours = %+v, want the late failure added without recounting the first job", hours)
	}
}

func TestRollupKeepsHistoryPastTheMinuteWindow(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "metrics.db"))
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	// Older than minute retention but inside hourly retention: a restart must roll it up
	// before the prune that would otherwise delete it.
	at := now.Add(-20 * 24 * time.Hour)
	if err := store.RecordJob(ctx, "old", "emails", "processed", 10, at); err != nil {
		t.Fatalf("RecordJob: %v", err)
	}
	if err := store.Rollup(ctx, now); err != nil {
		t.Fatalf("Rollup: %v", err)
	}
	if _, err := store.Prune(ctx, now); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	minutes, err := store.Buckets(ctx, ResolutionMinute, "emails", now.Add(-30*24*time.Hour), now)
	if err != nil {
		t.Fatalf("minute Buckets: %v", err)
	}
	if len(minutes) != 0 {
		t.Errorf("minutes = %+v, want the expired minute bucket gone", minutes)
	}
	hours, err := store.Buckets(ctx, ResolutionHourly, "emails", now.Add(-30*24*time.Hour), now)
	if err != nil {
		t.Fatalf("hourly Buckets: %v", err)
	}
	if len(hours) != 1 || hours[0].Processed != 1 {
		t.Errorf("hours = %+v, want the job preserved in its hour", hours)
	}
}

func TestPruneUsesTheConfiguredRawWindow(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metrics.db"), Retention{Raw: 24 * time.Hour})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	// 30 hours old: past the configured 24-hour raw window, inside the 72-hour default,
	// and inside the default minute window, so only the id may go.
	if err := store.RecordJob(context.Background(), "id", "emails", "processed", 10, now.Add(-30*time.Hour)); err != nil {
		t.Fatalf("RecordJob: %v", err)
	}
	pruned, err := store.Prune(context.Background(), now)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if pruned.Events != 1 || pruned.Buckets != 0 {
		t.Errorf("pruned = %+v, want only the id removed by the 24-hour window", pruned)
	}
}

func TestPruneUsesTheConfiguredRetentionPerLevel(t *testing.T) {
	retention := Retention{
		Raw:    24 * time.Hour,
		Minute: 48 * time.Hour,
		Hourly: 72 * time.Hour,
		Daily:  90 * time.Hour,
	}
	store, err := Open(filepath.Join(t.TempDir(), "metrics.db"), retention)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	// 79 hours old: past every window except the 90-hour daily one (its day starts 84
	// hours before now).
	old := now.Add(-79 * time.Hour)
	if err := store.RecordJob(ctx, "old", "emails", "processed", 10, old); err != nil {
		t.Fatalf("RecordJob: %v", err)
	}
	if err := store.Rollup(ctx, now); err != nil {
		t.Fatalf("Rollup: %v", err)
	}

	pruned, err := store.Prune(ctx, now)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if pruned.Events != 1 || pruned.Buckets != 1 || pruned.Hourly != 1 || pruned.Daily != 0 {
		t.Fatalf("pruned = %+v, want the id, minute and hour gone but the day kept", pruned)
	}

	// The daily aggregate outlives the hourly rows it was built from, then expires on its
	// own window.
	pruned, err = store.Prune(ctx, now.Add(10*time.Hour))
	if err != nil {
		t.Fatalf("Prune again: %v", err)
	}
	if pruned.Daily != 1 {
		t.Errorf("second prune = %+v, want the expired day removed", pruned)
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
