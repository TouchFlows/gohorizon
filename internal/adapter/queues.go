package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// inspectTimeout bounds one inspection. The command asks the application's queue
// contract for counts, so a slow answer means the application is unhealthy.
const (
	inspectTimeout = 10 * time.Second
	// inspectionSchema is the payload version `gohorizon:inspect --json` prints. It is its
	// own version, independent of the capabilities and failed-list payloads.
	inspectionSchema = 1
)

// QueueDepth is one queue as `gohorizon:inspect --json` reports it. A driver that cannot
// answer a count omits it, so each count is a pointer: nil is "not reported", which is
// not the same as zero. The backlog's age is reported the same way, with the exception
// class in OldestPendingError when the answer itself failed.
type QueueDepth struct {
	Queue    string `json:"queue"`
	Pending  *int   `json:"pending"`
	Reserved *int   `json:"reserved"`
	Delayed  *int   `json:"delayed"`
	// OldestPendingAt is when the oldest waiting job was enqueued, in unix seconds: the
	// age of the backlog rather than its size.
	OldestPendingAt    *int64  `json:"oldest_pending_at"`
	OldestPendingError *string `json:"oldest_pending_error"`
}

// Inspection is the part of the inspection payload GoHorizon acts on.
type Inspection struct {
	Schema     int          `json:"schema"`
	Connection string       `json:"connection"`
	Queues     []QueueDepth `json:"queues"`
}

// Queues runs `php artisan gohorizon:inspect --json` for the named queues and decodes
// the result.
func (p Probe) Queues(ctx context.Context, connection string, queues []string) (Inspection, error) {
	ctx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()

	args := []string{p.Artisan, "gohorizon:inspect", "--json"}
	if connection != "" {
		args = append(args, "--connection="+connection)
	}
	for _, queue := range queues {
		args = append(args, "--queue="+queue)
	}

	cmd := exec.CommandContext(ctx, p.PHP, args...)
	cmd.Dir = p.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		detail := summarise(stderr.String())
		if detail == "" {
			detail = summarise(stdout.String())
		}
		return Inspection{}, fmt.Errorf("%w: %v: %s", errUnavailable, err, detail)
	}

	inspection, err := decodeInspection(stdout.Bytes())
	if err != nil {
		return Inspection{}, fmt.Errorf("%w: %v", errUnavailable, err)
	}
	return inspection, nil
}

func decodeInspection(out []byte) (Inspection, error) {
	text := strings.TrimSpace(string(out))
	if text == "" {
		return Inspection{}, errors.New("gohorizon:inspect produced no output")
	}

	var inspection Inspection
	if err := json.Unmarshal([]byte(payloadLine(text)), &inspection); err != nil {
		return Inspection{}, fmt.Errorf("decode gohorizon:inspect output: %w", err)
	}
	if inspection.Schema != inspectionSchema {
		return Inspection{}, fmt.Errorf("gohorizon:inspect reports schema %d; this GoHorizon understands schema %d",
			inspection.Schema, inspectionSchema)
	}
	if len(inspection.Queues) == 0 {
		return Inspection{}, errors.New("gohorizon:inspect reported no queues")
	}
	return inspection, nil
}
