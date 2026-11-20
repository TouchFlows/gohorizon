// Package api exposes the read-only operational endpoints needed in Milestone 1.
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/gohorizon/gohorizon/internal/health"
)

type StatusProvider interface {
	Report() health.Report
}

func NewRouter(provider StatusProvider, log *slog.Logger) http.Handler {
	r := chi.NewRouter()
	r.Use(recoverer(log))

	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
			report := provider.Report()
			w.Header().Set("Content-Type", "application/json")
			if report.Status != "ok" {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
			_ = json.NewEncoder(w).Encode(report)
		})
	})

	return r
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
