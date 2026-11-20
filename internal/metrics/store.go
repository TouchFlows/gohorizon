// Package metrics persists consumed telemetry as durable per-queue aggregates.
//
// History lives in SQLite at storage.path (handoff §7.3/§9, docs/adr/0007): one row per
// queue per minute, plus the event ids already counted so a replayed stream entry can
// never be added twice. Reading it back is what a later API or dashboard will do; the
// dashboard itself is out of scope.
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

const schema = `
CREATE TABLE IF NOT EXISTS recorded_events (
	id TEXT PRIMARY KEY
);

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
// outcome counters, so it is not stored twice.
type Bucket struct {
	Start      time.Time
	Processed  int64
	Released   int64
	Failed     int64
	TimedOut   int64
	DurationMS int64
}

// Store is the SQLite-backed metrics history.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the metrics database and its schema.
func Open(path string) (*Store, error) {
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
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// RecordJob adds one finished job to its queue's minute bucket. It is idempotent by
// event id: the stream is at-least-once, so a replay after a crash must not double
// count. It satisfies the telemetry consumer's sink.
func (s *Store) RecordJob(ctx context.Context, eventID, queue, outcome string, durationMS int64, at time.Time) error {
	if eventID == "" || queue == "" {
		return errors.New("metrics record requires an event id and a queue")
	}
	column, ok := outcomeColumns[outcome]
	if !ok {
		return fmt.Errorf("unknown job outcome %q", outcome)
	}
	if durationMS < 0 || at.IsZero() {
		return fmt.Errorf("invalid job record for event %s", eventID)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin metrics transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx,
		`INSERT INTO recorded_events (id) VALUES (?) ON CONFLICT(id) DO NOTHING`, eventID)
	if err != nil {
		return fmt.Errorf("record event id: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check event id: %w", err)
	}
	if inserted == 0 {
		return tx.Commit() // already counted before a replay
	}

	query := fmt.Sprintf(`INSERT INTO queue_buckets (queue, bucket_start, duration_ms, %[1]s)
VALUES (?, ?, ?, 1)
ON CONFLICT(queue, bucket_start) DO UPDATE SET
	duration_ms = duration_ms + excluded.duration_ms,
	%[1]s = %[1]s + 1`, column)
	bucket := at.UTC().Truncate(bucketSize).Unix()
	if _, err := tx.ExecContext(ctx, query, queue, bucket, durationMS); err != nil {
		return fmt.Errorf("aggregate job metric: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit metrics transaction: %w", err)
	}
	return nil
}

// Buckets returns a queue's history in [from, to), oldest first.
func (s *Store) Buckets(ctx context.Context, queue string, from, to time.Time) ([]Bucket, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT bucket_start, processed, released, failed, timed_out, duration_ms
FROM queue_buckets
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
