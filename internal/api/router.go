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
	"github.com/gohorizon/gohorizon/internal/audit"
	"github.com/gohorizon/gohorizon/internal/auth"
	"github.com/gohorizon/gohorizon/internal/health"
	"github.com/gohorizon/gohorizon/internal/metrics"
)

// Provider is everything the endpoints expose: the current report, the persisted per-queue
// history and job history, and the one action the API takes on the pool.
type Provider interface {
	Report() health.Report
	Buckets(ctx context.Context, resolution metrics.Resolution, queue string, from, to time.Time) ([]metrics.Bucket, error)
	Jobs(ctx context.Context, query metrics.JobQuery) ([]metrics.Job, error)
	Job(ctx context.Context, jobID string) (metrics.Job, error)
	// RestartWorker asks the supervisor that runs uuid to stop the worker gracefully and
	// start its replacement in the same slot (docs/adr/0011).
	RestartWorker(uuid string) error
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

	// One record per session action. It is written through the same logger the service
	// already runs, so it ships and rotates with everything else (docs/adr/0010).
	auditor := audit.New(log)

	// The scrape path is the conventional one, outside the versioned API: a Prometheus
	// configuration names it, not a client written against this service.
	r.Get("/metrics", metricsExposition(provider))

	r.Route("/api/v1", func(r chi.Router) {
		if store := options.auth; store != nil {
			// The session routes are the way in, so they answer without one. Everything
			// else that reports operational state sits behind the gate below.
			r.Post("/session", signIn(store, log, auditor))
			r.Get("/session", currentSession(store))
			r.Delete("/session", signOut(store, auditor))
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

		// Administration is the only part of this API that changes the world, so it carries
		// the controls the reads do not need: a role below the minimum is refused, and the
		// mutation must echo the token from the cookie (docs/adr/0011).
		//
		// It is mounted only when the deployment has users. With nobody to hold a role there
		// is nobody to allow, and inventing an anonymous administrator is exactly what the
		// roles exist to avoid, so an open service simply has no administrative route.
		if store := options.auth; store != nil {
			r.Group(func(r chi.Router) {
				r.Use(
					requireSession(store),
					requireRole(store, auth.RoleOperator, audit.ActionWorkerRestart, auditor),
					requireCSRF(store, audit.ActionWorkerRestart, auditor),
				)
				r.Post("/workers/{worker}/restart", restartWorker(provider, store, log, auditor))
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

// requireRole refuses a session whose role is below minimum. The refusal is recorded, because
// "why could this person not do it" is one of the questions an audit trail exists to answer,
// and a role that silently did nothing is indistinguishable from a bug.
func requireRole(store *auth.Store, minimum, action string, auditor *audit.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			identity, ok := store.Current(r)
			if !ok {
				// The session check ran first; one that ended between the two is refused
				// exactly like one that was never there.
				w.Header().Set("Cache-Control", "no-store")
				writeError(w, http.StatusUnauthorized, "authentication required")
				return
			}
			if auth.Rank(identity.Role) < auth.Rank(minimum) {
				auditor.Record(r, audit.Event{
					Action:  action,
					Outcome: audit.OutcomeRefused,
					User:    identity.Name,
					Role:    identity.Role,
					Address: store.ClientAddress(r),
					Detail:  minimum + " role required",
				})
				w.Header().Set("Cache-Control", "no-store")
				writeError(w, http.StatusForbidden, minimum+" role required")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requireCSRF demands the double-submit token: a header repeating the cookie's value. The
// session cookie is SameSite=Lax, which already keeps another site's form post from carrying
// it; the token covers what that does not — a client replaying a captured request, a browser
// that does not implement the attribute — and it costs no server state. Neither half is
// secret: what it proves is that the request came from a page on this origin, since only that
// page can read the cookie and set the header (docs/adr/0011).
func requireCSRF(store *auth.Store, action string, auditor *audit.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if store.VerifyCSRF(r) {
				next.ServeHTTP(w, r)
				return
			}
			identity, _ := store.Current(r)
			auditor.Record(r, audit.Event{
				Action:  action,
				Outcome: audit.OutcomeRefused,
				User:    identity.Name,
				Role:    identity.Role,
				Address: store.ClientAddress(r),
				Detail:  "CSRF token missing or mismatched",
			})
			w.Header().Set("Cache-Control", "no-store")
			writeError(w, http.StatusForbidden, "CSRF token missing or mismatched")
		})
	}
}

// restartWorker restarts one worker: the supervisor stops it gracefully and starts its
// replacement in the same slot, so the job in flight finishes instead of being killed
// (docs/adr/0002). The id is looked up in the snapshot before anything is done with it, so a
// worker nobody runs is a 404 rather than an entry point into the pool: the API takes an id
// from the report it just served, never a command or a path (handoff §13).
func restartWorker(provider Provider, store *auth.Store, log *slog.Logger, auditor *audit.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uuid := chi.URLParam(r, "worker")
		identity, _ := store.Current(r)
		refuse := func(status int, detail, message string) {
			auditor.Record(r, audit.Event{
				Action:  audit.ActionWorkerRestart,
				Outcome: audit.OutcomeRefused,
				User:    identity.Name,
				Role:    identity.Role,
				Address: store.ClientAddress(r),
				Detail:  detail,
			})
			w.Header().Set("Cache-Control", "no-store")
			writeError(w, status, message)
		}

		supervisorID, found := supervisorOf(provider.Report(), uuid)
		if !found {
			refuse(http.StatusNotFound, "no worker with id "+uuid, "worker not found")
			return
		}
		if err := provider.RestartWorker(uuid); err != nil {
			// The snapshot had it a moment ago, so it exited in between. The request was
			// well formed; there is simply nothing left to restart.
			log.Warn("restart worker", "worker", uuid, "error", err)
			refuse(http.StatusConflict, "no longer running: "+err.Error(), "the worker is no longer running")
			return
		}

		auditor.Record(r, audit.Event{
			Action:  audit.ActionWorkerRestart,
			Outcome: audit.OutcomeAllowed,
			User:    identity.Name,
			Role:    identity.Role,
			Address: store.ClientAddress(r),
			Detail:  "supervisor " + supervisorID,
		})
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusAccepted, map[string]string{"worker": uuid, "supervisor": supervisorID})
	}
}

// supervisorOf names the supervisor running a worker, so the answer and the record can say
// which pool the action landed in.
func supervisorOf(report health.Report, uuid string) (string, bool) {
	for _, supervisor := range report.Supervisors {
		for _, worker := range supervisor.Workers {
			if worker.UUID == uuid {
				return supervisor.ID, true
			}
		}
	}
	return "", false
}

// signIn exchanges a name and password for a session cookie. An unknown name and a wrong
// password get the same answer, and a caller that keeps failing waits out the limiter.
func signIn(store *auth.Store, log *slog.Logger, auditor *audit.Logger) http.HandlerFunc {
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
			address := store.ClientAddress(r)
			var limited *auth.RateLimitedError
			if errors.As(err, &limited) {
				// The limiter answered before the password was checked, so the record says it
				// was held back rather than refused: those are different questions.
				auditor.Record(r, audit.Event{
					Action:  audit.ActionSignIn,
					Outcome: audit.OutcomeLimited,
					User:    body.Name,
					Address: address,
					Detail:  "too many attempts from this address",
				})
				seconds := int(math.Ceil(limited.RetryAfter.Seconds()))
				w.Header().Set("Retry-After", strconv.Itoa(seconds))
				writeError(w, http.StatusTooManyRequests, "too many sign-in attempts; try again later")
				return
			}
			auditor.Record(r, audit.Event{
				Action:  audit.ActionSignIn,
				Outcome: audit.OutcomeRefused,
				User:    body.Name,
				Address: address,
				Detail:  "invalid credentials",
			})
			log.Warn("sign-in refused", "user", body.Name, "address", address)
			writeError(w, http.StatusUnauthorized, "invalid credentials")
			return
		}

		// The body and the cookie both name a session, so neither may be cached by a proxy.
		w.Header().Set("Cache-Control", "no-store")
		// The record is written before the cookie, so a session that exists in a browser
		// always has a line that says it was started, and by whom.
		auditor.Record(r, audit.Event{
			Action:  audit.ActionSignIn,
			Outcome: audit.OutcomeAllowed,
			User:    identity.Name,
			Role:    identity.Role,
			Address: store.ClientAddress(r),
		})
		if err := store.Start(w, r, identity); err != nil {
			// The store no longer answers the request itself, so this is the only status
			// line written: a session the server could not sign is a failed sign-in.
			log.Error("start session", "user", identity.Name, "error", err)
			writeError(w, http.StatusInternalServerError, "could not start a session")
			return
		}
		// The token a mutation has to echo is issued with the session, so a browser never has
		// to ask for it separately before its first administrative call.
		store.IssueCSRF(w, r)
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
		// A session started before mutations existed has no token; reading the session is
		// where the bundle learns who it is, so this is where it picks one up.
		if _, err := r.Cookie(auth.CSRFCookie); err != nil {
			store.IssueCSRF(w, r)
		}
		writeJSON(w, http.StatusOK, identity)
	}
}

func signOut(store *auth.Store, auditor *audit.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Who ended the session is read before it is ended: afterwards there is nothing left
		// to name. A sign-out with no valid session has no actor, and records nothing.
		if identity, ok := store.Current(r); ok {
			auditor.Record(r, audit.Event{
				Action:  audit.ActionSignOut,
				Outcome: audit.OutcomeAllowed,
				User:    identity.Name,
				Role:    identity.Role,
				Address: store.ClientAddress(r),
			})
		}
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
