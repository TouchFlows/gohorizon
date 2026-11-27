// Package adapter talks to the gohorizon/laravel package through its documented
// artisan commands (docs/adr/0001-inspection-via-adapter.md). Nothing here reads a
// Laravel queue key, the application's database or any other framework internal.
package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gohorizon/gohorizon/internal/config"
)

const (
	// SchemaVersion is the capabilities schema this GoHorizon understands; a payload that
	// names another is refused rather than read with fields that may not mean the same thing.
	SchemaVersion = 1
	// probeTimeout bounds the capabilities call. The command only reads
	// configuration, so a slow answer means something is wrong with the application.
	probeTimeout = 20 * time.Second
)

// errUnavailable means the application could not answer: the package is not
// installed, artisan failed, or the output was not the documented JSON. The caller
// degrades the preflight instead of failing supervision (docs/adr/0001).
var errUnavailable = errors.New("gohorizon/laravel adapter is unavailable")

// retryAfterValue is a connection's retry_after as the application reports it: a
// number, a numeric string, or null (0) for drivers with no reservation window.
type retryAfterValue int

func (v *retryAfterValue) UnmarshalJSON(b []byte) error {
	raw := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if raw == "" || raw == "null" {
		*v = 0
		return nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("expected an integer or null, got %s", strings.TrimSpace(string(b)))
	}
	*v = retryAfterValue(n)
	return nil
}

// Connection is one queue.connections entry: the driver it names, and the reservation
// window the startup preflight compares a worker's timeout against. The rest of the entry
// is ignored.
type Connection struct {
	Driver     string          `json:"driver"`
	RetryAfter retryAfterValue `json:"retry_after"`
}

// Capabilities is `php artisan gohorizon:capabilities --json`: the identifiers the
// application reports about itself, the connections GoHorizon acts on, and the runtime and
// event facts its preflight reads. Unknown fields are ignored, so a package that adds one
// does not break an older service (docs/adr/0014).
type Capabilities struct {
	Schema         int    `json:"schema"`
	Package        string `json:"package"`
	PackageVersion string `json:"package_version"`
	Laravel        string `json:"laravel"`
	PHP            string `json:"php"`
	Application    string `json:"application"`
	Environment    string `json:"environment"`
	Queue          struct {
		DefaultConnection string                `json:"default_connection"`
		Connections       map[string]Connection `json:"connections"`
	} `json:"queue"`
	Runtime struct {
		PCNTL bool `json:"pcntl"`
	} `json:"runtime"`
	Telemetry struct {
		Stream string `json:"stream"`
	} `json:"telemetry"`
	// Events are the Laravel lifecycle events the application's version has, which is how a
	// newer Laravel with a renamed event is visible instead of silently unobserved.
	Events []string `json:"events"`
}

// Supported reports whether this GoHorizon understands the payload at all: a schema it does
// not know is as unusable as one that never arrived, and the caller must not read the
// fields it thinks it recognises as if they were meaningful.
func (c Capabilities) Supported() bool { return c.Schema == SchemaVersion }

// Reason is why this answer cannot be used, in the words the endpoint, the health report and a
// refused action all publish: the probe's own error when it could not answer, and both schema
// numbers when it answered one this service does not understand. It is empty for an answer that
// can be used, and the caller decides whether it can (Supported).
func (c Capabilities) Reason(err error) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("the application reports capabilities schema %d; this GoHorizon understands schema %d",
		c.Schema, SchemaVersion)
}

// Probe executes the capabilities command in the application directory.
type Probe struct {
	PHP     string
	Artisan string
	Dir     string
}

// Capabilities runs `php artisan gohorizon:capabilities --json` and decodes the
// result.
func (p Probe) Capabilities(ctx context.Context) (Capabilities, error) {
	// One-shot command, so the timeout kills it; unlike a worker, whose lifecycle
	// belongs to the supervisor's graceful stop.
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.PHP, p.Artisan, "gohorizon:capabilities", "--json")
	cmd.Dir = p.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		detail := summarise(stderr.String())
		if detail == "" {
			detail = summarise(stdout.String())
		}
		return Capabilities{}, fmt.Errorf("%w: %v: %s", errUnavailable, err, detail)
	}

	caps, err := decodeCapabilities(stdout.Bytes())
	if err != nil {
		return Capabilities{}, fmt.Errorf("%w: %v", errUnavailable, err)
	}
	return caps, nil
}

func decodeCapabilities(out []byte) (Capabilities, error) {
	text := strings.TrimSpace(string(out))
	if text == "" {
		return Capabilities{}, errors.New("gohorizon:capabilities produced no output")
	}
	var caps Capabilities
	if err := json.Unmarshal([]byte(payloadLine(text)), &caps); err != nil {
		return Capabilities{}, fmt.Errorf("decode gohorizon:capabilities output: %w", err)
	}
	if caps.Schema == 0 {
		return Capabilities{}, errors.New("gohorizon:capabilities output has no schema")
	}
	return caps, nil
}

// payloadLine returns the last JSON object line of command output: an application that
// prints something before its payload must not silently disable the caller.
func payloadLine(text string) string {
	if idx := strings.LastIndex(text, "\n{"); idx >= 0 {
		return strings.TrimSpace(text[idx+1:])
	}
	return text
}

func summarise(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// Check compares the capabilities the application reports with the configured worker
// launch and refuses the combinations that quietly corrupt retry semantics:
//
//   - a `--timeout` cannot be enforced without the pcntl extension (Laravel would
//     accept the option and never fire it), and
//   - `--timeout` must stay below the connection's `retry_after`, otherwise the job
//     is reserved again while a worker is still running it (docs/adr/0002).
//
// Every problem is reported at once; the caller starts no worker.
func Check(caps *Capabilities, app *config.Application) error {
	if caps == nil {
		return errors.New("capabilities are unavailable")
	}
	if caps.Schema != SchemaVersion {
		return fmt.Errorf("gohorizon/laravel reports capabilities schema %d; this GoHorizon understands schema %d",
			caps.Schema, SchemaVersion)
	}

	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if hasTimeout(app) && !caps.Runtime.PCNTL {
		add("the pcntl extension is missing from %s, so queue:work would accept worker.timeout and never enforce it; install pcntl", app.PHP)
	}

	conn, ok := caps.Queue.Connections[app.RedisConnection]
	if !ok {
		add("queue connection %q is not defined in the application's queue.connections (%s)",
			app.RedisConnection, connectionNames(caps))
	} else if conn.RetryAfter > 0 {
		// A null retry_after means the driver has no reservation window to stay
		// under; that is not a failure.
		for i := range app.Supervisors {
			sup := &app.Supervisors[i]
			if sup.Worker.Timeout >= int(conn.RetryAfter) {
				add("supervisor %q: worker.timeout (%ds) must be lower than the retry_after (%ds) of queue connection %q, otherwise the job is retried while a worker is still running it; raise retry_after or lower worker.timeout",
					sup.ID, sup.Worker.Timeout, int(conn.RetryAfter), app.RedisConnection)
			}
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("worker launch contradicts the application's capabilities: %s", strings.Join(problems, "; "))
}

func hasTimeout(app *config.Application) bool {
	for i := range app.Supervisors {
		if app.Supervisors[i].Worker.Timeout > 0 {
			return true
		}
	}
	return false
}

func connectionNames(caps *Capabilities) string {
	names := make([]string, 0, len(caps.Queue.Connections))
	for name := range caps.Queue.Connections {
		names = append(names, name)
	}
	if len(names) == 0 {
		return "none"
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
