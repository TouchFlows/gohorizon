package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/gohorizon/gohorizon/internal/adapter"
)

// failedJobsResponse is one page of the application's failed store. NextBefore is the
// opaque cursor for the page after this one, empty on the last page — the same shape as job
// history, so a client pages the two the same way (docs/adr/0013).
type failedJobsResponse struct {
	FailedJobs []adapter.FailedJob `json:"failed_jobs"`
	NextBefore string              `json:"next_before"`
}

// failedJobList lists the application's own failed jobs through the adapter. Unlike job
// history this is not an observation GoHorizon keeps: it is the store Laravel retries and
// forgets from, so the screen and the two actions agree on what exists (docs/adr/0012,
// docs/adr/0013). It is a read, so any signed-in role may see it; it is mounted with the
// other reads and not with the administrative routes.
func failedJobList(provider Provider, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()

		limit := adapter.DefaultFailedJobsLimit
		if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > adapter.MaxFailedJobsLimit {
				writeError(w, http.StatusBadRequest, "limit must be between 1 and "+strconv.Itoa(adapter.MaxFailedJobsLimit))
				return
			}
			limit = parsed
		}

		before := strings.TrimSpace(query.Get("before"))
		if before != "" && !cursorShaped(before) {
			writeError(w, http.StatusBadRequest, "before must be the next_before cursor of a previous page")
			return
		}

		page, err := provider.FailedJobs(r.Context(), adapter.FailedJobQuery{Limit: limit, Before: before})
		if err != nil {
			if errors.Is(err, adapter.ErrInvalidFailedJobCursor) {
				// The cursor is one this service never issued: the caller's mistake, and not
				// an application that could not answer.
				writeError(w, http.StatusBadRequest, "before must be the next_before cursor of a previous page")
				return
			}
			// The application is the only place that knows its failed store, so a command
			// that could not run is the service being unavailable, not an empty list.
			log.Error("read failed jobs", "error", err)
			writeError(w, http.StatusServiceUnavailable, "could not read failed jobs")
			return
		}

		if page.FailedJobs == nil {
			page.FailedJobs = []adapter.FailedJob{} // an empty page is [], never null
		}
		writeJSON(w, http.StatusOK, failedJobsResponse{FailedJobs: page.FailedJobs, NextBefore: page.NextBefore})
	}
}

// cursorShaped reports whether value could be one of the command's base64url cursors. Whether
// it decodes is the command's answer; this only keeps whitespace and separators off the
// application's command line.
func cursorShaped(value string) bool {
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
