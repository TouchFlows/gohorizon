package health

// Report is the /api/v1/health payload and the single source of truth for whether
// GoHorizon is doing its job.
type Report struct {
	Status        string       `json:"status"`
	Version       string       `json:"version"`
	Application   string       `json:"application,omitempty"`
	Environment   string       `json:"environment,omitempty"`
	UptimeSeconds int64        `json:"uptime_seconds"`
	Lock          Lock         `json:"lock"`
	Telemetry     Telemetry    `json:"telemetry"`
	Queues        Queues       `json:"queues"`
	Capabilities  Capabilities `json:"capabilities"`
	Supervisors   []Supervisor `json:"supervisors"`
}

// Capabilities is what the application said about itself and whether this service can act on
// it, which is the state the dashboard's controls and the administrative routes follow
// (docs/adr/0014). It is body-only: a deployment presented as read-only still supervises its
// pool, so it never degrades the report status — an alert reads this field or the gauge, never
// the health check an orchestrator would restart on.
type Capabilities struct {
	Available bool `json:"available"`
	// Schema is the version the application reported, 0 when it could not answer at all.
	Schema int `json:"schema,omitempty"`
	// FailedReads counts the consecutive re-reads that did not answer since the last one that
	// did, so a run of them is visible before the deployment is presented as read-only.
	FailedReads int    `json:"failed_reads"`
	Error       string `json:"error,omitempty"`
	// Contradiction is what the application's current answer contradicts in the running
	// configuration — a retry_after below a worker's timeout, a --timeout without pcntl — in the
	// words the launch preflight refuses with. The pool is already running, so this is published
	// and never enforced (docs/adr/0014).
	Contradiction string `json:"contradiction,omitempty"`
	// LastReadAt is when the re-read loop last got an answer, in unix seconds, 0 when it never
	// has. Every other field here describes the last answer, so a loop that stopped looks like a
	// healthy one; this is the field an alert on staleness reads (docs/adr/0014).
	LastReadAt int64 `json:"last_read_at,omitempty"`
}

type Lock struct {
	Held      bool   `json:"held"`
	Degraded  bool   `json:"degraded"`
	LastError string `json:"last_error,omitempty"`
}

// Telemetry is the ingestion path: counters since start plus whether it is failing
// right now. It is body-only for monitoring: a failing ingestion path neither
// degrades the report status nor affects the workers.
type Telemetry struct {
	Events     int    `json:"events"`
	Duplicates int    `json:"duplicates"`
	Invalid    int    `json:"invalid"`
	Errors     int    `json:"errors"`
	Failing    bool   `json:"failing"`
	LastError  string `json:"last_error,omitempty"`
}

// Queues is the last queue-depth sample taken through the package's inspection
// command. It is body-only: a missing or failing package leaves the report status
// alone, because a failed health check must not restart a supervisor whose workers are
// healthy (docs/adr/0001, docs/adr/0004 item 7).
//
// A count the driver cannot report is null rather than zero.
type Queues struct {
	Connection string       `json:"connection,omitempty"`
	Error      string       `json:"error,omitempty"`
	Depths     []QueueDepth `json:"depths"`
}

type QueueDepth struct {
	Queue    string `json:"queue"`
	Pending  *int   `json:"pending"`
	Reserved *int   `json:"reserved"`
	Delayed  *int   `json:"delayed"`
	// OldestPendingAt is the unix time the oldest waiting job was enqueued, so a stalled
	// queue is visible even when its depth looks small. nil means the driver could not
	// report it, which is not the same as zero.
	OldestPendingAt    *int64 `json:"oldest_pending_at"`
	OldestPendingError string `json:"oldest_pending_error,omitempty"`
}

type Supervisor struct {
	ID          string   `json:"id"`
	Desired     int      `json:"desired"`
	Running     int      `json:"running"`
	Stopping    int      `json:"stopping"`
	Crashed     int      `json:"crashed"`
	Adopted     int      `json:"adopted"`
	Escalations int      `json:"escalations"`
	Workers     []Worker `json:"workers"`
}

type Worker struct {
	UUID           string `json:"uuid"`
	Queue          string `json:"queue"`
	PID            int    `json:"pid"`
	State          string `json:"state"`
	Restarts       int    `json:"restarts"`
	LastExitCode   int    `json:"last_exit_code"`
	LastExitReason string `json:"last_exit_reason,omitempty"`
	Adopted        bool   `json:"adopted"`
	// CrashLooping marks a worker that keeps dying before it can do any work. The pool
	// keeps replacing it, so this is body-only and leaves the report status alone.
	CrashLooping bool `json:"crash_looping"`
}
