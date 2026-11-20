// Package api exposes the read-only operational endpoints needed in Milestone 1.
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

// Provider is everything the read-only endpoints expose: the current report and the
// persisted per-queue history.
type Provider interface {
	Report() health.Report
	Buckets(ctx context.Context, queue string, from, to time.Time) ([]metrics.Bucket, error)
}

// defaultHistoryWindow is the range the metrics endpoint answers when no `from` is
// given: the last hour of minute buckets.
const defaultHistoryWindow = time.Hour

func NewRouter(provider Provider, log *slog.Logger) http.Handler {
	r := chi.NewRouter()
	r.Use(recoverer(log))

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
	})

	return r
}

type metricsResponse struct {
	Queue   string          `json:"queue"`
	From    time.Time       `json:"from"`
	To      time.Time       `json:"to"`
	Buckets []bucketPayload `json:"buckets"`
}

type bucketPayload struct {
	Start      time.Time `json:"start"`
	Processed  int64     `json:"processed"`
	Released   int64     `json:"released"`
	Failed     int64     `json:"failed"`
	TimedOut   int64     `json:"timed_out"`
	DurationMS int64     `json:"duration_ms"`
}

// metricsHistory reads one queue's persisted minute buckets for [from, to) from the
// existing store. It is read-only; the dashboard consumes it later (docs/adr/0007).
func metricsHistory(provider Provider, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()

		queue := strings.TrimSpace(query.Get("queue"))
		if queue == "" {
			writeError(w, http.StatusBadRequest, "queue is required")
			return
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

		from := to.Add(-defaultHistoryWindow)
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

		buckets, err := provider.Buckets(r.Context(), queue, from, to)
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

		payload := metricsResponse{
			Queue:   queue,
			From:    from,
			To:      to,
			Buckets: make([]bucketPayload, 0, len(buckets)),
		}
		for _, bucket := range buckets {
			payload.Buckets = append(payload.Buckets, bucketPayload{
				Start:      bucket.Start,
				Processed:  bucket.Processed,
				Released:   bucket.Released,
				Failed:     bucket.Failed,
				TimedOut:   bucket.TimedOut,
				DurationMS: bucket.DurationMS,
			})
		}
		writeJSON(w, http.StatusOK, payload)
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
