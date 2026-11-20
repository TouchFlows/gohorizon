package process

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// stateFileVersion describes the on-disk adoption journal. Bumping it invalidates old
// journals instead of guessing at their meaning.
const stateFileVersion = 1

type stateFile struct {
	Version int      `json:"version"`
	Workers []Record `json:"workers"`
}

// WriteStateFile persists the records atomically. An empty path disables persistence
// (used by unit tests that do not exercise adoption).
func WriteStateFile(path string, records []Record) error {
	if path == "" {
		return nil
	}
	payload := struct {
		Version int      `json:"version"`
		Workers []Record `json:"workers"`
	}{Version: stateFileVersion, Workers: records}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create state directory %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".workers-*.json")
	if err != nil {
		return fmt.Errorf("create state file: %w", err)
	}
	defer os.Remove(tmp.Name())

	enc := jsonEncoder(tmp)
	if err := enc.Encode(payload); err != nil {
		tmp.Close()
		return fmt.Errorf("encode state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close state file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}
	return nil
}

// ReadStateFile returns the previous run's records. A missing or unreadable journal is
// not fatal: the supervisor then falls back to adopting by process identity.
func ReadStateFile(path string) []Record {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var payload struct {
		Version int      `json:"version"`
		Workers []Record `json:"workers"`
	}
	if err := jsonUnmarshal(data, &payload); err != nil {
		return nil
	}
	if payload.Version != stateFileVersion {
		return nil
	}
	return payload.Workers
}

// startedAgo is a helper for backoff reset decisions.
func startedAgo(rec Record, now time.Time) time.Duration {
	if rec.StartedAt.IsZero() {
		return 0
	}
	return now.Sub(rec.StartedAt)
}
