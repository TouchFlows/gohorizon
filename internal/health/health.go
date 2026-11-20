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
	Supervisors   []Supervisor `json:"supervisors"`
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
}
