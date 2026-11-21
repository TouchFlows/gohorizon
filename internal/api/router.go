// Package api exposes the read-only operational endpoints: health, queue and job history,
// and the Prometheus scrape of the same state.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gohorizon/gohorizon/internal/health"
	"github.com/gohorizon/gohorizon/internal/metrics"
)

// Provider is everything the read-only endpoints expose: the current report, the persisted
// per-queue history and job history.
type Provider interface {
	Report() health.Report
	Buckets(ctx context.Context, resolution metrics.Resolution, queue string, from, to time.Time) ([]metrics.Bucket, error)
	Jobs(ctx context.Context, query metrics.JobQuery) ([]metrics.Job, error)
	Job(ctx context.Context, jobID string) (metrics.Job, error)
}

// defaultHistoryWindows is the range the metrics endpoint answers when no `from` is
// given, per resolution: an hour, a day or a month.
var defaultHistoryWindows = map[metrics.Resolution]time.Duration{
	metrics.ResolutionMinute: time.Hour,
	metrics.ResolutionHourly: 24 * time.Hour,
	metrics.ResolutionDaily:  30 * 24 * time.Hour,
}

func NewRouter(provider Provider, log *slog.Logger) http.Handler {
	r := chi.NewRouter()
	r.Use(recoverer(log))

	// The scrape path is the conventional one, outside the versioned API: a Prometheus
	// configuration names it, not a client written against this service.
	r.Get("/metrics", metricsExposition(provider))

	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
			report := provider.Report()
			status := http.StatusOK
			if report.Status != "ok" {
				status = http.StatusServiceUnavailable
			}
			writeJSON(w, status, report)
		})
		r.Get("/metrics", metricsHistory(provider, log))
		r.Get("/jobs", jobHistory(provider, log))
		r.Get("/jobs/{id}", jobDetail(provider, log))
	})

	return r
}

// metricsResponse carries the store's buckets unchanged; their JSON tags are the wire
// shape, so there is no second copy of the same fields to keep in step.
type metricsResponse struct {
	Queue      string             `json:"queue"`
	Resolution metrics.Resolution `json:"resolution"`
	From       time.Time          `json:"from"`
	To         time.Time          `json:"to"`
	Buckets    []metrics.Bucket   `json:"buckets"`
}

// metricsHistory reads one queue's persisted history for [from, to) at the requested
// resolution (minute when omitted) from the existing store. It is read-only; the
// dashboard consumes it later (docs/adr/0007).
func metricsHistory(provider Provider, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()

		queue := strings.TrimSpace(query.Get("queue"))
		if queue == "" {
			writeError(w, http.StatusBadRequest, "queue is required")
			return
		}

		resolution := metrics.ResolutionMinute
		if raw := strings.TrimSpace(query.Get("resolution")); raw != "" {
			switch candidate := metrics.Resolution(raw); candidate {
			case metrics.ResolutionMinute, metrics.ResolutionHourly, metrics.ResolutionDaily:
				resolution = candidate
			default:
				writeError(w, http.StatusBadRequest, "resolution must be minute, hourly or daily")
				return
			}
		}

		to := time.Now().UTC()
		if raw := query.Get("to"); raw != "" {
			parsed, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				writeError(w, http.StatusBadRequest, "to must be an RFC 3339 timestamp")
				return
			}
			to = parsed.UTC()
		}

		from := to.Add(-defaultHistoryWindows[resolution])
		if raw := query.Get("from"); raw != "" {
			parsed, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				writeError(w, http.StatusBadRequest, "from must be an RFC 3339 timestamp")
				return
			}
			from = parsed.UTC()
		}
		if !from.Before(to) {
			writeError(w, http.StatusBadRequest, "from must be before to")
			return
		}

		buckets, err := provider.Buckets(r.Context(), resolution, queue, from, to)
		if err != nil {
			if errors.Is(err, metrics.ErrDisabled) {
				writeError(w, http.StatusServiceUnavailable, err.Error())
				return
			}
			// The store's error may name the database path; keep it out of the response.
			log.Error("read metrics history", "queue", queue, "error", err)
			writeError(w, http.StatusInternalServerError, "could not read metrics history")
			return
		}

		if buckets == nil {
			buckets = []metrics.Bucket{} // an empty range is [], never null
		}
		writeJSON(w, http.StatusOK, metricsResponse{Queue: queue, Resolution: resolution, From: from, To: to, Buckets: buckets})
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("http handler panic", "panic", rec, "path", r.URL.Path)
					http.Error(w, "internal server error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
