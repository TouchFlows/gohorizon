// Package metrics persists consumed telemetry as durable per-queue aggregates.
//
// History lives in SQLite at storage.path (handoff §7.3/§9, docs/adr/0007): one row per
// queue per minute, rolled up into hours and days, plus the event ids already counted so
// a replayed stream entry can never be added twice. `GET /api/v1/metrics` serves the
// minute level back; the dashboard itself is out of scope.
package metrics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// bucketSize is the aggregation window: one row per queue per minute of traffic.
const bucketSize = time.Minute

// Default retention follows handoff §9: raw telemetry lives 24–72 hours (the upper
// bound, so a consumer that was down still recognises replays), minute aggregates 7
// days, hourly aggregates 30 days and daily aggregates 12 months. The service passes
// configured windows in; these are the fallbacks for an omitted storage.retention.
const (
	rawRetention    = 72 * time.Hour
	minuteRetention = 7 * 24 * time.Hour
	hourlyRetention = 30 * 24 * time.Hour
	dailyRetention  = 365 * 24 * time.Hour
)

const schema = `
CREATE TABLE IF NOT EXISTS recorded_events (
	id TEXT PRIMARY KEY,
	recorded_at INTEGER NOT NULL
);

-- One row per finished job attempt: what an operator reads as job history. It is a raw
-- observation, pruned on the same window as the event ids (docs/adr/0007).
CREATE TABLE IF NOT EXISTS jobs (
	id TEXT PRIMARY KEY,
	job_id TEXT NOT NULL,
	uuid TEXT NOT NULL DEFAULT '',
	class TEXT NOT NULL,
	queue TEXT NOT NULL,
	connection TEXT NOT NULL DEFAULT '',
	outcome TEXT NOT NULL,
	attempts INTEGER NOT NULL,
	duration_ms INTEGER NOT NULL,
	finished_at INTEGER NOT NULL,
	exception_class TEXT NOT NULL DEFAULT '',
	exception_message TEXT NOT NULL DEFAULT ''
);

-- Newest-first by outcome, queue or job: the three questions job history asks.
CREATE INDEX IF NOT EXISTS jobs_finished_at ON jobs (finished_at DESC, id);
CREATE INDEX IF NOT EXISTS jobs_job_id ON jobs (job_id, finished_at DESC);
CREATE INDEX IF NOT EXISTS jobs_queue_finished_at ON jobs (queue, finished_at DESC);
CREATE INDEX IF NOT EXISTS jobs_outcome_finished_at ON jobs (outcome, finished_at DESC);

CREATE TABLE IF NOT EXISTS queue_buckets (
	queue TEXT NOT NULL,
	bucket_start INTEGER NOT NULL,
	processed INTEGER NOT NULL DEFAULT 0,
	released INTEGER NOT NULL DEFAULT 0,
	failed INTEGER NOT NULL DEFAULT 0,
	timed_out INTEGER NOT NULL DEFAULT 0,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (queue, bucket_start)
);

CREATE TABLE IF NOT EXISTS queue_hourly_buckets (
	queue TEXT NOT NULL,
	bucket_start INTEGER NOT NULL,
	processed INTEGER NOT NULL DEFAULT 0,
	released INTEGER NOT NULL DEFAULT 0,
	failed INTEGER NOT NULL DEFAULT 0,
	timed_out INTEGER NOT NULL DEFAULT 0,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (queue, bucket_start)
);

CREATE TABLE IF NOT EXISTS queue_daily_buckets (
	queue TEXT NOT NULL,
	bucket_start INTEGER NOT NULL,
	processed INTEGER NOT NULL DEFAULT 0,
	released INTEGER NOT NULL DEFAULT 0,
	failed INTEGER NOT NULL DEFAULT 0,
	timed_out INTEGER NOT NULL DEFAULT 0,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (queue, bucket_start)
);
`

// outcomeColumns maps an outcome to its counter column. The set is closed, and only
// these fixed names ever reach SQL.
var outcomeColumns = map[string]string{
	"processed": "processed",
	"released":  "released",
	"failed":    "failed",
	"timed_out": "timed_out",
}

// Bucket is one queue's aggregate for one minute. The job count is the sum of the
// outcome counters, so it is not stored twice. Its JSON tags are the wire shape of
// `GET /api/v1/metrics`.
type Bucket struct {
	Start      time.Time `json:"start"`
	Processed  int64     `json:"processed"`
	Released   int64     `json:"released"`
	Failed     int64     `json:"failed"`
	TimedOut   int64     `json:"timed_out"`
	DurationMS int64     `json:"duration_ms"`
}

// JobRecord is one finished job as the consumer hands it to the store: the observation an
// operator reads as job history. EventID is the stream entry and deduplicates it, JobID is
// the job's identity (its uuid, or the publisher's `id:` fallback), and a job that ran more
// than once has one row per attempt. The exception fields are empty when none was thrown.
type JobRecord struct {
	EventID          string
	JobID            string
	UUID             string
	Class            string
	Queue            string
	Connection       string
	Outcome          string
	Attempts         int
	DurationMS       int64
	FinishedAt       time.Time
	ExceptionClass   string
	ExceptionMessage string
}

// ErrDisabled reports that history was never opened: storage.driver is not sqlite, or
// the database could not be opened at startup (the service supervises anyway).
var ErrDisabled = errors.New("metrics history is disabled")

// Retention bounds how long each level of history lives. A zero field takes its
// default, so a configuration that omits storage.retention keeps the handoff windows.
type Retention struct {
	Raw    time.Duration
	Minute time.Duration
	Hourly time.Duration
	Daily  time.Duration
}

// DefaultRetention is handoff §9's suggested window per level.
func DefaultRetention() Retention {
	return Retention{Raw: rawRetention, Minute: minuteRetention, Hourly: hourlyRetention, Daily: dailyRetention}
}

func (r Retention) withDefaults() Retention {
	defaults := DefaultRetention()
	if r.Raw <= 0 {
		r.Raw = defaults.Raw
	}
	if r.Minute <= 0 {
		r.Minute = defaults.Minute
	}
	if r.Hourly <= 0 {
		r.Hourly = defaults.Hourly
	}
	if r.Daily <= 0 {
		r.Daily = defaults.Daily
	}
	return r
}

// Resolution selects which aggregate level a read returns.
type Resolution string

const (
	ResolutionMinute Resolution = "minute"
	ResolutionHourly Resolution = "hourly"
	ResolutionDaily  Resolution = "daily"
)

// bucketTables maps a resolution to its table. Only these fixed names ever reach SQL.
var bucketTables = map[Resolution]string{
	ResolutionMinute: "queue_buckets",
	ResolutionHourly: "queue_hourly_buckets",
	ResolutionDaily:  "queue_daily_buckets",
}

// Store is the SQLite-backed metrics history.
type Store struct {
	db        *sql.DB
	retention Retention
}

// Open opens (creating if needed) the metrics database and its schema.
func Open(path string, retention Retention) (*Store, error) {
	if path == "" {
		return nil, errors.New("metrics database path must not be empty")
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create metrics directory %s: %w", dir, err)
		}
	}

	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open metrics database %s: %w", path, err)
	}
	// One connection: SQLite serialises writers anyway, and this keeps the consumer's
	// writes from racing the reads that report history.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate metrics database %s: %w", path, err)
	}
	return &Store{db: db, retention: retention.withDefaults()}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Pruned reports how much history one Prune removed, per level.
type Pruned struct {
	Events  int64
	Jobs    int64
	Minutes int64
	Hourly  int64
	Daily   int64
}

// Prune drops raw event ids and job rows and each aggregate level older than its configured
// window. It is the only bound on the database's growth, so it runs at startup and hourly,
// after Rollup has folded everything the prune is about to delete.
func (s *Store) Prune(ctx context.Context, now time.Time) (Pruned, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Pruned{}, fmt.Errorf("begin prune transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var pruned Pruned
	result, err := tx.ExecContext(ctx, `DELETE FROM recorded_events WHERE recorded_at < ?`,
		now.Add(-s.retention.Raw).UTC().Unix())
	if err != nil {
		return Pruned{}, fmt.Errorf("prune recorded event ids: %w", err)
	}
	if pruned.Events, err = result.RowsAffected(); err != nil {
		return Pruned{}, fmt.Errorf("count pruned event ids: %w", err)
	}

	// Job rows are raw data too: same window, same pass, no second notion of "raw".
	result, err = tx.ExecContext(ctx, `DELETE FROM jobs WHERE finished_at < ?`,
		now.Add(-s.retention.Raw).UTC().Unix())
	if err != nil {
		return Pruned{}, fmt.Errorf("prune job rows: %w", err)
	}
	if pruned.Jobs, err = result.RowsAffected(); err != nil {
		return Pruned{}, fmt.Errorf("count pruned job rows: %w", err)
	}

	if pruned.Minutes, err = pruneBuckets(ctx, tx, ResolutionMinute, now.Add(-s.retention.Minute)); err != nil {
		return Pruned{}, fmt.Errorf("prune minute buckets: %w", err)
	}
	if pruned.Hourly, err = pruneBuckets(ctx, tx, ResolutionHourly, now.Add(-s.retention.Hourly)); err != nil {
		return Pruned{}, fmt.Errorf("prune hourly buckets: %w", err)
	}
	if pruned.Daily, err = pruneBuckets(ctx, tx, ResolutionDaily, now.Add(-s.retention.Daily)); err != nil {
		return Pruned{}, fmt.Errorf("prune daily buckets: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Pruned{}, fmt.Errorf("commit prune transaction: %w", err)
	}
	return pruned, nil
}

// RecordJob stores one finished job attempt and adds it to its queue's minute bucket. It is
// idempotent by event id: the stream is at-least-once, so a replay after a crash must not
// duplicate the row or double count. It satisfies the telemetry consumer's sink.
func (s *Store) RecordJob(ctx context.Context, job JobRecord) error {
	if job.EventID == "" || job.JobID == "" || job.Class == "" || job.Queue == "" {
		return errors.New("metrics record requires an event id, a job id, a class and a queue")
	}
	column, ok := outcomeColumns[job.Outcome]
	if !ok {
		return fmt.Errorf("unknown job outcome %q", job.Outcome)
	}
	if job.DurationMS < 0 || job.Attempts < 0 || job.FinishedAt.IsZero() {
		return fmt.Errorf("invalid job record for event %s", job.EventID)
	}
	at := job.FinishedAt

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin metrics transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx,
		`INSERT INTO recorded_events (id, recorded_at) VALUES (?, ?) ON CONFLICT(id) DO NOTHING`,
		job.EventID, at.UTC().Unix())
	if err != nil {
		return fmt.Errorf("record event id: %w", err)
	}
	eventInserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check event id: %w", err)
	}

	// The row is keyed by the event, so replaying an entry fills nothing twice. A row that
	// is already there still counts as recorded even if its event id was pruned.
	result, err = tx.ExecContext(ctx, `
INSERT INTO jobs (id, job_id, uuid, class, queue, connection, outcome, attempts,
	duration_ms, finished_at, exception_class, exception_message)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO NOTHING`,
		job.EventID, job.JobID, job.UUID, job.Class, job.Queue, job.Connection, job.Outcome,
		job.Attempts, job.DurationMS, at.UTC().Unix(), job.ExceptionClass, job.ExceptionMessage)
	if err != nil {
		return fmt.Errorf("record job row: %w", err)
	}
	jobInserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check job row: %w", err)
	}
	if eventInserted == 0 || jobInserted == 0 {
		return tx.Commit() // already recorded before a replay
	}

	query := fmt.Sprintf(`INSERT INTO queue_buckets (queue, bucket_start, duration_ms, %[1]s)
VALUES (?, ?, ?, 1)
ON CONFLICT(queue, bucket_start) DO UPDATE SET
	duration_ms = duration_ms + excluded.duration_ms,
	%[1]s = %[1]s + 1`, column)
	bucket := at.UTC().Truncate(bucketSize).Unix()
	if _, err := tx.ExecContext(ctx, query, job.Queue, bucket, job.DurationMS); err != nil {
		return fmt.Errorf("aggregate job metric: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit metrics transaction: %w", err)
	}
	return nil
}

// Rollup folds closed minute buckets into hourly buckets and closed hourly buckets into
// daily ones. Each pass recomputes the affected periods from their source with absolute
// values, so running it twice cannot double count and a late event inside a closed hour
// is picked up by the next pass. Only the hour in progress is left out: the day in
// progress collects its completed hours and is recomputed by later passes.
func (s *Store) Rollup(ctx context.Context, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin rollup transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	hourStart := now.UTC().Truncate(time.Hour).Unix()
	if _, err := tx.ExecContext(ctx, hourlyRollup, hourStart); err != nil {
		return fmt.Errorf("roll up minute buckets into hours: %w", err)
	}
	if _, err := tx.ExecContext(ctx, dailyRollup, hourStart); err != nil {
		return fmt.Errorf("roll up hourly buckets into days: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit rollup: %w", err)
	}
	return nil
}

// The modulo constants are time.Hour and 24 hours in seconds. The WHERE clause also keeps
// SQLite's upsert parser from reading ON as a join constraint.
const hourlyRollup = `
INSERT INTO queue_hourly_buckets (queue, bucket_start, processed, released, failed, timed_out, duration_ms)
SELECT queue, bucket_start - (bucket_start % 3600),
	SUM(processed), SUM(released), SUM(failed), SUM(timed_out), SUM(duration_ms)
FROM queue_buckets
WHERE bucket_start < ?
GROUP BY queue, bucket_start - (bucket_start % 3600)
ON CONFLICT(queue, bucket_start) DO UPDATE SET
	processed = excluded.processed,
	released = excluded.released,
	failed = excluded.failed,
	timed_out = excluded.timed_out,
	duration_ms = excluded.duration_ms`

const dailyRollup = `
INSERT INTO queue_daily_buckets (queue, bucket_start, processed, released, failed, timed_out, duration_ms)
SELECT queue, bucket_start - (bucket_start % 86400),
	SUM(processed), SUM(released), SUM(failed), SUM(timed_out), SUM(duration_ms)
FROM queue_hourly_buckets
WHERE bucket_start < ?
GROUP BY queue, bucket_start - (bucket_start % 86400)
ON CONFLICT(queue, bucket_start) DO UPDATE SET
	processed = excluded.processed,
	released = excluded.released,
	failed = excluded.failed,
	timed_out = excluded.timed_out,
	duration_ms = excluded.duration_ms`

// bucketTable maps a read or prune target to its table, rejecting anything that is not
// one of the fixed resolutions before a name reaches SQL.
func bucketTable(resolution Resolution) (string, error) {
	table, ok := bucketTables[resolution]
	if !ok {
		return "", fmt.Errorf("unknown metrics resolution %q", resolution)
	}
	return table, nil
}

func pruneBuckets(ctx context.Context, tx *sql.Tx, resolution Resolution, cutoff time.Time) (int64, error) {
	table, err := bucketTable(resolution)
	if err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE bucket_start < ?", cutoff.UTC().Unix())
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// Buckets returns a queue's history at one resolution in [from, to), oldest first.
func (s *Store) Buckets(ctx context.Context, resolution Resolution, queue string, from, to time.Time) ([]Bucket, error) {
	table, err := bucketTable(resolution)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT bucket_start, processed, released, failed, timed_out, duration_ms
FROM `+table+`
WHERE queue = ? AND bucket_start >= ? AND bucket_start < ?
ORDER BY bucket_start`,
		queue, from.UTC().Unix(), to.UTC().Unix())
	if err != nil {
		return nil, fmt.Errorf("read queue %q history: %w", queue, err)
	}
	defer rows.Close()

	var buckets []Bucket
	for rows.Next() {
		var bucket Bucket
		var start int64
		if err := rows.Scan(&start, &bucket.Processed, &bucket.Released,
			&bucket.Failed, &bucket.TimedOut, &bucket.DurationMS); err != nil {
			return nil, fmt.Errorf("scan queue %q history: %w", queue, err)
		}
		bucket.Start = time.Unix(start, 0).UTC()
		buckets = append(buckets, bucket)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read queue %q history: %w", queue, err)
	}
	return buckets, nil
}
