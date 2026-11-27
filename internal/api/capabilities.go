package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/gohorizon/gohorizon/internal/adapter"
	"github.com/gohorizon/gohorizon/internal/audit"
	"github.com/gohorizon/gohorizon/internal/auth"
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
	// PayloadSchemas are the versions of the application's other documented payloads this
	// service reads. They describe this service, not the application, so they are published
	// even when the descriptor is unavailable: a client comparing them with what the
	// application's commands print can tell which side to upgrade.
	PayloadSchemas payloadSchemas `json:"payload_schemas"`
	// ApplicationPayloadSchemas is what the application says its own commands emit, when it
	// says anything. Published beside the versions this service reads so a mismatch is visible
	// as two numbers rather than only as the reason an operation was withheld.
	ApplicationPayloadSchemas *payloadSchemas `json:"application_payload_schemas,omitempty"`
	// ReadOnlySince is when the deployment was first presented as read-only, in unix seconds, 0
	// while it is readable. Error says why the controls are missing; this says how long they have
	// been, which is what a banner has to show to be useful, and it is the same instant health
	// publishes (docs/adr/0014).
	ReadOnlySince int64           `json:"read_only_since,omitempty"`
	Operations    map[string]bool `json:"operations"`
	// OperationReasons says why each withheld operation is withheld, in the words a refused
	// action returns. A false entry without one would be a control that disappears with no
	// explanation, which is the failure the descriptor exists to avoid (docs/adr/0014).
	OperationReasons map[string]string `json:"operation_reasons,omitempty"`
	Error            string            `json:"error,omitempty"`
}

// payloadSchemas names the versions of the queue-inspection and failed-list payloads this
// service accepts. Each read refuses a payload that declares another version with both numbers
// in the error, which is what keeps a package upgrade from being read as if nothing had changed
// (docs/adr/0014).
type payloadSchemas struct {
	Inspect    int `json:"inspect"`
	FailedJobs int `json:"failed_jobs"`
}

// payloadSchemaVersions is the one place those versions are read from, so the descriptor cannot
// publish a number the decoders do not enforce.
func payloadSchemaVersions() payloadSchemas {
	return payloadSchemas{Inspect: adapter.InspectionSchema, FailedJobs: adapter.FailedJobsSchema}
}

// capabilityConnection is one queue connection as the descriptor reports it. RetryAfter is
// null when the driver has no reservation window, which is not a zero-second window.
type capabilityConnection struct {
	Driver     string `json:"driver,omitempty"`
	RetryAfter *int   `json:"retry_after"`
}

// capabilityOperationNames is every operation the descriptor publishes, so the set a client
// sees does not depend on which of them happen to be offered.
var capabilityOperationNames = []string{
	"inspect_queues",
	"telemetry",
	"failed_jobs_list",
	"failed_job_retry",
	"failed_job_delete",
	"restart_workers",
}

// capabilityOperations is what this service offers against the application, derived from what
// the descriptor says rather than only from whether it could be read, together with the reason
// for each operation it withholds.
//
// A descriptor that cannot be read is read-only as a whole, restart included: the dashboard
// must never offer an action beside an unexplained failure (docs/adr/0014). A descriptor that
// can be read is still not a promise that everything works — an application with no telemetry
// stream has no events to show, and a connection whose driver runs jobs inline or drops them
// has no queue to count — so those two follow the facts the payload carries. The rest follow
// the descriptor being readable, because the payload does not describe anything narrower.
func capabilityOperations(caps adapter.Capabilities, available bool, unavailable string) (map[string]bool, map[string]string) {
	operations := make(map[string]bool, len(capabilityOperationNames))
	reasons := make(map[string]string, len(capabilityOperationNames))

	withhold := func(operation, reason string) {
		operations[operation] = false
		reasons[operation] = reason
	}

	if !available {
		for _, operation := range capabilityOperationNames {
			withhold(operation, unavailable)
		}
		return operations, reasons
	}

	// Restarting is the supervisor's own action, so the payload cannot withhold it.
	operations["restart_workers"] = true

	if caps.Telemetry.Stream == "" {
		withhold("telemetry", "the application reports no telemetry stream, so there are no events to show")
	} else {
		operations["telemetry"] = true
	}

	switch {
	case caps.InspectPayloadRefusal() != "":
		// The application's own command prints a payload this service cannot read, so the
		// depth it would count is not one to publish. A driver without a queue is the other
		// reason the same screen cannot be filled.
		withhold("inspect_queues", caps.InspectPayloadRefusal())
	case inspectableDrivers(caps) == nil:
		withhold("inspect_queues", noInspectableQueue(caps))
	default:
		operations["inspect_queues"] = true
	}

	// The failed store is the application's own failer, and its list payload is versioned like
	// the inspect one. A package that changed that payload withholds the list — and only the
	// list, because retry and delete run Laravel's own commands rather than that payload.
	if reason := caps.FailedJobsPayloadRefusal(); reason != "" {
		withhold("failed_jobs_list", reason)
	} else {
		operations["failed_jobs_list"] = true
	}
	operations["failed_job_retry"] = true
	operations["failed_job_delete"] = true

	return operations, reasons
}

// inspectableDrivers lists the drivers of the application's queue connections that have a queue
// to inspect, in name order, or nil when none has. `sync` runs a job inline and `null` drops it,
// so neither has a queue, a backlog or a depth. An unknown driver is assumed inspectable: the
// inspect payload already omits a count its driver cannot answer instead of inventing one.
func inspectableDrivers(caps adapter.Capabilities) []string {
	seen := make(map[string]bool, len(caps.Queue.Connections))
	var drivers []string
	for _, conn := range caps.Queue.Connections {
		switch conn.Driver {
		case "", "sync", "null":
			continue
		}
		if seen[conn.Driver] {
			continue
		}
		seen[conn.Driver] = true
		drivers = append(drivers, conn.Driver)
	}
	sort.Strings(drivers)
	return drivers
}

// noInspectableQueue is why queue depth is withheld, naming the connections the application
// reported so the reason describes this deployment rather than the rule.
func noInspectableQueue(caps adapter.Capabilities) string {
	if len(caps.Queue.Connections) == 0 {
		return "the application defines no queue connections, so there is no queue to inspect"
	}

	seen := make(map[string]bool, len(caps.Queue.Connections))
	var drivers []string
	for _, conn := range caps.Queue.Connections {
		driver := conn.Driver
		if driver == "" {
			driver = "unnamed"
		}
		if seen[driver] {
			continue
		}
		seen[driver] = true
		drivers = append(drivers, driver)
	}
	sort.Strings(drivers)

	return fmt.Sprintf("the application's queue connections (%s) have no queue to inspect, so queue depth cannot be counted",
		strings.Join(drivers, ", "))
}

// capabilities publishes the descriptor. A payload whose schema this service does not
// know is reported as unavailable rather than half-read: the fields it thinks it recognises
// need not mean the same thing in another schema.
func capabilities(provider Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		caps, err := provider.Capabilities()
		writeDescriptor(w, provider, caps, err)
	}
}

// refreshCapabilities re-reads the descriptor before answering, so a repair can be confirmed
// now rather than at the next interval. The answer is the document a GET would return for the
// same read — a client comparing the two is comparing one deployment, not two formats — and a
// read that fails is still a 200 with `available: false` and the reason, because "the
// application cannot answer" is the answer to the question that was asked (docs/adr/0014).
//
// The record says what the read found, not only that it happened: "was the repair confirmed" is
// what an operator asks the log afterwards, and a line that named neither would leave them to
// ask again.
func refreshCapabilities(provider Provider, store *auth.Store, auditor *audit.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caps, err := provider.ReprobeCapabilities(r.Context())
		me, _ := store.Current(r)
		auditor.Record(r, audit.Event{
			Action:  audit.ActionCapabilityRefresh,
			Outcome: audit.OutcomeAllowed,
			User:    me.Name,
			Role:    me.Role,
			Address: store.ClientAddress(r),
			Detail:  capabilityReadResult(caps, err),
		})
		writeDescriptor(w, provider, caps, err)
	}
}

// capabilityReadResult says what one read found, in the words the descriptor publishes: the
// reason it is read-only, or what became readable.
func capabilityReadResult(caps adapter.Capabilities, err error) string {
	if err != nil || !caps.Supported() {
		return "still read-only: " + caps.Reason(err)
	}
	return fmt.Sprintf("readable: schema %d, package %s %s", caps.Schema, caps.Package, caps.PackageVersion)
}

// writeDescriptor answers with the descriptor computed from one read, so the endpoint and the
// forced re-read cannot publish two different documents about the same deployment.
func writeDescriptor(w http.ResponseWriter, provider Provider, caps adapter.Capabilities, err error) {
	available := err == nil && caps.Supported()
	// The same sentence the health report and a refused action publish, so a client cannot
	// read two different stories about one deployment.
	unavailable := ""
	if !available {
		unavailable = caps.Reason(err)
	}
	operations, reasons := capabilityOperations(caps, available, unavailable)
	response := capabilitiesResponse{
		Available:        available,
		ReadOnlySince:    provider.CapabilityReadOnlySince(),
		PayloadSchemas:   payloadSchemaVersions(),
		Operations:       operations,
		OperationReasons: reasons,
		Error:            unavailable,
	}
	if available {
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
		if caps.Payloads.Inspect != 0 || caps.Payloads.FailedJobs != 0 {
			reported := payloadSchemas{Inspect: caps.Payloads.Inspect, FailedJobs: caps.Payloads.FailedJobs}
			response.ApplicationPayloadSchemas = &reported
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
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
