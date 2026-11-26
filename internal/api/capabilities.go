package api

import (
	"fmt"
	"net/http"
	"sort"

	"github.com/gohorizon/gohorizon/internal/adapter"
)

// capabilitiesResponse is GET /api/v1/capabilities: what the application said about itself at
// startup, plus the operations this service can offer given that answer. It is one descriptor
// rather than a health field, so the dashboard's controls have a single place to ask
// (docs/adr/0014).
//
// Available is false when the application could not answer or answered a schema this service
// does not understand; Error then says which. The answer is still a 200: "this deployment
// cannot do that" is what the caller asked, not a failed read, and a 503 would only invite a
// retry of a fact that will not change until the service restarts.
type capabilitiesResponse struct {
	Available         bool                            `json:"available"`
	Schema            int                             `json:"schema,omitempty"`
	Package           string                          `json:"package,omitempty"`
	PackageVersion    string                          `json:"package_version,omitempty"`
	Laravel           string                          `json:"laravel,omitempty"`
	PHP               string                          `json:"php,omitempty"`
	Application       string                          `json:"application,omitempty"`
	Environment       string                          `json:"environment,omitempty"`
	DefaultConnection string                          `json:"default_connection,omitempty"`
	Connections       map[string]capabilityConnection `json:"connections,omitempty"`
	PCNTL             bool                            `json:"pcntl"`
	TelemetryStream   string                          `json:"telemetry_stream,omitempty"`
	Events            []string                        `json:"events,omitempty"`
	Operations        map[string]bool                 `json:"operations"`
	Error             string                          `json:"error,omitempty"`
}

// capabilityConnection is one queue connection as the descriptor reports it. RetryAfter is
// null when the driver has no reservation window, which is not a zero-second window.
type capabilityConnection struct {
	Driver     string `json:"driver,omitempty"`
	RetryAfter *int   `json:"retry_after"`
}

// capabilityOperations names what this service can do against the application. Queue
// inspection, telemetry, the failed store and its two actions all run the application's own
// artisan commands, so they are offered only when the probe answered with a schema this
// service understands. Restarting a worker is the supervisor's own action and does not depend
// on the application at all, so it stays offered while the rest do not (docs/adr/0014).
func capabilityOperations(understood bool) map[string]bool {
	return map[string]bool{
		"inspect_queues":    understood,
		"telemetry":         understood,
		"failed_jobs_list":  understood,
		"failed_job_retry":  understood,
		"failed_job_delete": understood,
		"restart_workers":   true,
	}
}

// capabilities publishes the startup descriptor. A payload whose schema this service does not
// know is reported as unavailable rather than half-read: the fields it thinks it recognises
// need not mean the same thing in another schema.
func capabilities(provider Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caps, err := provider.Capabilities()
		response := capabilitiesResponse{
			Available:  err == nil && caps.Supported(),
			Operations: capabilityOperations(false),
		}
		if err != nil {
			response.Error = err.Error()
		} else if !caps.Supported() {
			response.Error = fmt.Sprintf(
				"the application reports capabilities schema %d; this GoHorizon understands schema %d",
				caps.Schema, adapter.SchemaVersion)
		} else {
			response.Schema = caps.Schema
			response.Package = caps.Package
			response.PackageVersion = caps.PackageVersion
			response.Laravel = caps.Laravel
			response.PHP = caps.PHP
			response.Application = caps.Application
			response.Environment = caps.Environment
			response.DefaultConnection = caps.Queue.DefaultConnection
			response.Connections = capabilityConnections(caps)
			response.PCNTL = caps.Runtime.PCNTL
			response.TelemetryStream = caps.Telemetry.Stream
			response.Events = caps.Events
			response.Operations = capabilityOperations(true)
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, response)
	}
}

// capabilityConnections copies the connections the application defined, in name order so the
// document does not reshuffle between reads.
func capabilityConnections(caps adapter.Capabilities) map[string]capabilityConnection {
	connections := make(map[string]capabilityConnection, len(caps.Queue.Connections))
	names := make([]string, 0, len(caps.Queue.Connections))
	for name := range caps.Queue.Connections {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		conn := caps.Queue.Connections[name]
		out := capabilityConnection{Driver: conn.Driver}
		if conn.RetryAfter > 0 {
			seconds := int(conn.RetryAfter)
			out.RetryAfter = &seconds
		}
		connections[name] = out
	}
	return connections
}
