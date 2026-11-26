package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// failedListTimeout bounds one listing. The command only reads the application's failed
// store through its failer, so a slow answer means the application is unhealthy.
const failedListTimeout = 15 * time.Second

// failedJobsSchema is the payload version `gohorizon:failed:list --json` prints. It is
// separate from the capabilities schema because the two payloads change independently.
const failedJobsSchema = 1

// The listing page bounds: a size the dashboard can render without paging, and a ceiling
// that keeps one request from asking the application for a whole failed store.
const (
	DefaultFailedJobsLimit = 50
	MaxFailedJobsLimit     = 200
)

// FailedJob is one record of the application's failed store as the command reports it. The
// payload behind it never leaves the application: Job is the class name Laravel recorded,
// and Exception is the sanitised first line of the stored trace. A record without an
// exception carries an empty string rather than a made-up message.
type FailedJob struct {
	ID         string `json:"id"`
	UUID       string `json:"uuid"`
	Connection string `json:"connection"`
	Queue      string `json:"queue"`
	Job        string `json:"job"`
	FailedAt   string `json:"failed_at"`
	Exception  string `json:"exception"`
}

// FailedJobsPage is one page of the application's failed store. NextBefore is the cursor for
// the page after this one, empty on the last page: it is the id of the final record, so the
// next request continues before exactly that id (docs/adr/0013).
type FailedJobsPage struct {
	FailedJobs []FailedJob `json:"failed_jobs"`
	NextBefore string      `json:"next_before"`
}

// FailedJobQuery selects one page. Before is the NextBefore of a previous page and is empty
// for the first page. The adapter only passes digits on, so a cursor can never become an
// artisan option.
type FailedJobQuery struct {
	Limit  int
	Before string
}

// FailedJobs runs `php artisan gohorizon:failed:list --json` and decodes the page. The
// command reads the application's own failer, which is the store retry and delete act on
// (docs/adr/0001, docs/adr/0013).
func (p Probe) FailedJobs(ctx context.Context, query FailedJobQuery) (FailedJobsPage, error) {
	if query.Limit < 1 || query.Limit > MaxFailedJobsLimit {
		return FailedJobsPage{}, fmt.Errorf("failed job limit must be between 1 and %d", MaxFailedJobsLimit)
	}
	if query.Before != "" && !digits(query.Before) {
		return FailedJobsPage{}, fmt.Errorf("failed job cursor must be a record id, got %q", query.Before)
	}

	ctx, cancel := context.WithTimeout(ctx, failedListTimeout)
	defer cancel()

	args := []string{p.Artisan, "gohorizon:failed:list", "--json", "--limit=" + strconv.Itoa(query.Limit)}
	if query.Before != "" {
		args = append(args, "--before="+query.Before)
	}

	cmd := exec.CommandContext(ctx, p.PHP, args...)
	cmd.Dir = p.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		detail := summarise(stderr.String())
		if detail == "" {
			detail = summarise(stdout.String())
		}
		return FailedJobsPage{}, fmt.Errorf("%w: %v: %s", errUnavailable, err, detail)
	}

	page, err := decodeFailedJobs(stdout.Bytes())
	if err != nil {
		return FailedJobsPage{}, fmt.Errorf("%w: %v", errUnavailable, err)
	}
	return page, nil
}

// decodeFailedJobs reads the documented payload, including the case where the application
// printed something before its JSON line.
func decodeFailedJobs(out []byte) (FailedJobsPage, error) {
	text := strings.TrimSpace(string(out))
	if text == "" {
		return FailedJobsPage{}, errors.New("gohorizon:failed:list produced no output")
	}

	var payload struct {
		Schema     int         `json:"schema"`
		FailedJobs []FailedJob `json:"failed_jobs"`
		NextBefore string      `json:"next_before"`
	}
	if err := json.Unmarshal([]byte(payloadLine(text)), &payload); err != nil {
		return FailedJobsPage{}, fmt.Errorf("decode gohorizon:failed:list output: %w", err)
	}
	if payload.Schema != failedJobsSchema {
		return FailedJobsPage{}, fmt.Errorf("gohorizon:failed:list reports schema %d; this GoHorizon understands schema %d",
			payload.Schema, failedJobsSchema)
	}

	return FailedJobsPage{FailedJobs: payload.FailedJobs, NextBefore: payload.NextBefore}, nil
}

// digits reports whether value is one or more decimal digits. It is the only shape a cursor
// may take, because it becomes one argument of the application's command.
func digits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
