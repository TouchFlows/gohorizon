package api

import (
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gohorizon/gohorizon/internal/metrics"
	"github.com/gohorizon/gohorizon/internal/telemetry"
)

// The job history page bounds: a size a dashboard can render without paging, and a ceiling
// that keeps one request from reading a whole retention window.
const (
	defaultJobLimit = 50
	maxJobLimit     = 200
)

// jobsResponse is one page of job history. NextBefore is the opaque cursor for the page after
// this one, empty on the last page: it is the final row's (finished_at, id), so the next
// request continues exactly where this one stopped.
type jobsResponse struct {
	Jobs       []metrics.Job `json:"jobs"`
	NextBefore string        `json:"next_before"`
}

// jobHistory lists finished attempts newest first, filtered by queue and outcome and paged by
// keyset. It reads history the consumer stored (docs/adr/0007); nothing here writes.
func jobHistory(provider Provider, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		jobQuery := metrics.JobQuery{Queue: strings.TrimSpace(query.Get("queue"))}

		limit := defaultJobLimit
		if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > maxJobLimit {
				writeError(w, http.StatusBadRequest, "limit must be between 1 and 200")
				return
			}
			limit = parsed
		}

		if raw := strings.TrimSpace(query.Get("outcome")); raw != "" {
			if !slices.Contains(telemetry.Outcomes, raw) {
				writeError(w, http.StatusBadRequest, "outcome must be one of "+strings.Join(telemetry.Outcomes, ", "))
				return
			}
			jobQuery.Outcome = raw
		}

		if raw := query.Get("before"); raw != "" {
			cursor, err := parseJobCursor(raw)
			if err != nil {
				writeError(w, http.StatusBadRequest, "before must be the next_before cursor of a previous page")
				return
			}
			jobQuery.Before = &cursor
		}

		// One row more than the page says whether another page exists, so a full last page
		// offers no cursor instead of one that returns nothing.
		jobQuery.Limit = limit + 1
		jobs, err := provider.Jobs(r.Context(), jobQuery)
		if err != nil {
			writeHistoryError(w, log, err, "read job history", "could not read job history")
			return
		}

		response := jobsResponse{Jobs: []metrics.Job{}} // an empty page is [], never null
		if len(jobs) > limit {
			jobs = jobs[:limit]
			response.NextBefore = formatJobCursor(jobs[len(jobs)-1])
		}
		if len(jobs) > 0 {
			response.Jobs = jobs
		}
		writeJSON(w, http.StatusOK, response)
	}
}

// jobDetail returns one job's newest attempt by identity: the uuid Laravel published, or the
// `id:` form for a job that reached the queue without one.
func jobDetail(provider Provider, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity := strings.TrimSpace(chi.URLParam(r, "id"))
		if identity == "" {
			writeError(w, http.StatusBadRequest, "job id is required")
			return
		}

		job, err := provider.Job(r.Context(), identity)
		if err != nil {
			if errors.Is(err, metrics.ErrJobNotFound) {
				writeError(w, http.StatusNotFound, "job not found")
				return
			}
			writeHistoryError(w, log, err, "read job history", "could not read job history")
			return
		}
		writeJSON(w, http.StatusOK, job)
	}
}

// writeHistoryError maps a history read failure onto its status, keeping the store's error —
// which can name the database path — out of the response. Unavailable history is a 503, as on
// the metrics endpoint, so a disabled store never looks like "no jobs ran".
func writeHistoryError(w http.ResponseWriter, log *slog.Logger, err error, what, message string) {
	if errors.Is(err, metrics.ErrDisabled) {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	log.Error(what, "error", err)
	writeError(w, http.StatusInternalServerError, message)
}

// formatJobCursor renders the (finished_at, id) keyset as "<unix seconds>:<event id>". Event
// ids are uuids, so the first colon separates the halves and a client cannot invent a
// position the store would misread.
func formatJobCursor(job metrics.Job) string {
	return strconv.FormatInt(job.FinishedAt.UTC().Unix(), 10) + ":" + job.ID
}

func parseJobCursor(raw string) (metrics.JobCursor, error) {
	seconds, id, found := strings.Cut(strings.TrimSpace(raw), ":")
	if !found || id == "" {
		return metrics.JobCursor{}, errors.New("cursor is not finished_at:id")
	}
	parsed, err := strconv.ParseInt(seconds, 10, 64)
	if err != nil {
		return metrics.JobCursor{}, errors.New("cursor timestamp is not a unix second")
	}
	return metrics.JobCursor{FinishedAt: time.Unix(parsed, 0).UTC(), ID: id}, nil
}
