package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gohorizon/gohorizon/internal/health"
)

type stubProvider struct{ report health.Report }

func (s stubProvider) Report() health.Report { return s.report }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestHealthExposesTelemetryCounters(t *testing.T) {
	provider := stubProvider{report: health.Report{
		Status:  "ok",
		Version: "test",
		Telemetry: health.Telemetry{
			Events:     42,
			Duplicates: 3,
			Invalid:    1,
			Errors:     2,
		},
	}}

	recorder := httptest.NewRecorder()
	NewRouter(provider, discardLogger()).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	var payload struct {
		Telemetry struct {
			Events     int  `json:"events"`
			Duplicates int  `json:"duplicates"`
			Invalid    int  `json:"invalid"`
			Errors     int  `json:"errors"`
			Failing    bool `json:"failing"`
		} `json:"telemetry"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if payload.Telemetry.Events != 42 || payload.Telemetry.Duplicates != 3 ||
		payload.Telemetry.Invalid != 1 || payload.Telemetry.Errors != 2 {
		t.Errorf("telemetry = %+v, want the consumer's counters", payload.Telemetry)
	}
	if payload.Telemetry.Failing {
		t.Error("failing = true for a healthy consumer")
	}
}

func TestHealthReportsFailingIngestion(t *testing.T) {
	provider := stubProvider{report: health.Report{
		Status: "degraded",
		Telemetry: health.Telemetry{
			Errors:    7,
			Failing:   true,
			LastError: "read group \"gohorizon\": redis is down",
		},
	}}

	recorder := httptest.NewRecorder()
	NewRouter(provider, discardLogger()).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for a degraded report", recorder.Code)
	}

	var payload struct {
		Status    string `json:"status"`
		Telemetry struct {
			Failing   bool   `json:"failing"`
			LastError string `json:"last_error"`
		} `json:"telemetry"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !payload.Telemetry.Failing || payload.Telemetry.LastError == "" {
		t.Errorf("telemetry = %+v, want the live failure visible", payload.Telemetry)
	}
}
