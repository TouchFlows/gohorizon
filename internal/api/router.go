// Package api exposes the read-only operational endpoints: health, queue and job history,
// and the Prometheus scrape of the same state.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gohorizon/gohorizon/internal/auth"
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

// Option configures the router without changing the shape every existing caller uses.
type Option func(*routerOptions)

type routerOptions struct {
	// auth is nil when the deployment has no users, which leaves the API open as it was
	// before Milestone 3.
	auth *auth.Store
}

// WithAuth requires a signed-in session for every operational endpoint except the
// session routes themselves and the Prometheus scrape.
func WithAuth(store *auth.Store) Option {
	return func(o *routerOptions) { o.auth = store }
}

func NewRouter(provider Provider, log *slog.Logger, opts ...Option) http.Handler {
	var options routerOptions
	for _, opt := range opts {
		opt(&options)
	}

	r := chi.NewRouter()
	r.Use(recoverer(log))

	// The scrape path is the conventional one, outside the versioned API: a Prometheus
	// configuration names it, not a client written against this service.
	r.Get("/metrics", metricsExposition(provider))

	r.Route("/api/v1", func(r chi.Router) {
		if store := options.auth; store != nil {
			// The session routes are the way in, so they answer without one. Everything
			// else that reports operational state sits behind the gate below.
			r.Post("/session", signIn(store, log))
			r.Get("/session", currentSession(store))
			r.Delete("/session", signOut(store))
		} else {
			// The dashboard asks who it is talking to before it draws a page, and an open
			// service has nobody to be. Answering in JSON says that plainly, so the bundle
			// can tell "authentication is off" from "you are not signed in" — the shell
			// would answer a missing route, and a page cannot read that as an answer.
			r.Get("/session", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Cache-Control", "no-store")
				writeError(w, http.StatusNotFound, "authentication is not configured")
			})
		}

		r.Group(func(r chi.Router) {
			if store := options.auth; store != nil {
				r.Use(requireSession(store))
			}
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
	})

	// Everything the versioned API does not claim is the dashboard: a bundle asset, or the
	// shell for a client route (docs/adr/0008).
	r.NotFound(dashboard())

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

// requireSession refuses anything but a valid session. The answer is JSON like the rest
// of the API, so a client can tell an unauthenticated call from an unavailable one.
func requireSession(store *auth.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := store.Current(r); !ok {
				w.Header().Set("Cache-Control", "no-store")
				writeError(w, http.StatusUnauthorized, "authentication required")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// signIn exchanges a name and password for a session cookie. An unknown name and a wrong
// password get the same answer, and a caller that keeps failing waits out the limiter.
func signIn(store *auth.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name     string `json:"name"`
			Password string `json:"password"`
		}
		// A sign-in body is tiny; a larger one is a client mistake, not a credential.
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "name and password are required")
			return
		}

		identity, err := store.Authenticate(r, body.Name, body.Password)
		if err != nil {
			var limited *auth.RateLimitedError
			if errors.As(err, &limited) {
				seconds := int(math.Ceil(limited.RetryAfter.Seconds()))
				w.Header().Set("Retry-After", strconv.Itoa(seconds))
				writeError(w, http.StatusTooManyRequests, "too many sign-in attempts; try again later")
				return
			}
			log.Warn("sign-in refused", "user", body.Name, "address", r.RemoteAddr)
			writeError(w, http.StatusUnauthorized, "invalid credentials")
			return
		}

		// The body and the cookie both name a session, so neither may be cached by a proxy.
		w.Header().Set("Cache-Control", "no-store")
		if err := store.Start(w, r, identity); err != nil {
			// The store no longer answers the request itself, so this is the only status
			// line written: a session the server could not sign is a failed sign-in.
			log.Error("start session", "user", identity.Name, "error", err)
			writeError(w, http.StatusInternalServerError, "could not start a session")
			return
		}
		writeJSON(w, http.StatusOK, identity)
	}
}

// currentSession answers who the caller is, and is how the dashboard decides between the
// sign-in screen and the pages.
func currentSession(store *auth.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		identity, ok := store.Current(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		writeJSON(w, http.StatusOK, identity)
	}
}

func signOut(store *auth.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		store.End(w, r)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
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
