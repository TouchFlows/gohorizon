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

// FailedJobsSchema is the payload version `gohorizon:failed:list --json` prints and the only one
// this service reads. It is separate from the capabilities schema because the two payloads
// change independently, and it is published in the capabilities descriptor so a client can see
// which failed-list payload this deployment accepts (docs/adr/0014).
const FailedJobsSchema = 1

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

// ErrInvalidFailedJobCursor means the cursor the caller sent is not one the command issued.
// It is the command's own invalid-argument status, kept distinct so the endpoint can answer
// 400 instead of blaming the application.
var ErrInvalidFailedJobCursor = errors.New("the failed job cursor is not one this service issued")

// FailedJobQuery selects one page. Before is the NextBefore of a previous page and is empty
// for the first page. It is an opaque value the command decodes; the adapter only checks its
// shape, because the meaning of a cursor belongs to the application side that issued it.
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
	if query.Before != "" && !cursorValue(query.Before) {
		return FailedJobsPage{}, fmt.Errorf("failed job cursor is not a page cursor: %q", query.Before)
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
		// The command answers an unreadable cursor with its invalid-argument status. That is
		// the caller's mistake, not the application's, so it keeps its own identity.
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 2 {
			return FailedJobsPage{}, fmt.Errorf("%w: %s", ErrInvalidFailedJobCursor, summarise(stdout.String()+stderr.String()))
		}
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
	if payload.Schema != FailedJobsSchema {
		return FailedJobsPage{}, fmt.Errorf("gohorizon:failed:list reports schema %d; this GoHorizon understands schema %d",
			payload.Schema, FailedJobsSchema)
	}

	return FailedJobsPage{FailedJobs: payload.FailedJobs, NextBefore: payload.NextBefore}, nil
}

// cursorValue reports whether value has the shape of the command's cursor: unpadded
// base64url. The characters are checked here so a cursor can never carry whitespace or an
// option separator into the application's command line; whether it decodes is the command's
// answer to give.
func cursorValue(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
