package adapter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// failedJobTimeout bounds one administrative call. The command only reads the application's
// failed-job record and pushes one stored payload back onto its queue, so a slow answer means
// the application is unhealthy.
const failedJobTimeout = 15 * time.Second

// ErrFailedJobNotFound means the application has no failed job with the id it was asked for:
// it was never stored, or it has already been retried or forgotten. It is a distinct error
// because the endpoint answers it with a status, not with a failure.
var ErrFailedJobNotFound = errors.New("the application has no failed job with that id")

// RetryFailedJob asks the application to push one failed job back onto its queue, through
// `php artisan queue:retry` (docs/adr/0012). GoHorizon never reads or writes a Laravel queue
// key (handoff §13, docs/adr/0001): the application forgets the failed record and re-publishes
// the payload it stored, which is the same thing an operator running the command would get.
//
// The command reports an id it cannot find on its output and still exits successfully, so the
// answer is read as well as the status. Both phrasings Laravel 11–13 use for that are matched;
// anything else it prints is left to the caller's log.
func (p Probe) RetryFailedJob(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("no failed job id given")
	}
	// The id becomes one argument, never a command: an id starting with '-' would be read as
	// an option by artisan, which is not something a caller of this endpoint may choose.
	if strings.HasPrefix(id, "-") {
		return fmt.Errorf("invalid failed job id %q", id)
	}

	ctx, cancel := context.WithTimeout(ctx, failedJobTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.PHP, p.Artisan, "queue:retry", id)
	cmd.Dir = p.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		detail := summarise(stderr.String())
		if detail == "" {
			detail = summarise(stdout.String())
		}
		return fmt.Errorf("%w: %v: %s", errUnavailable, err, detail)
	}

	output := stdout.String() + "\n" + stderr.String()
	if strings.Contains(output, "Unable to find failed job with ID") ||
		strings.Contains(output, "Unable to find any failed jobs with ID") {
		return fmt.Errorf("%w: %s", ErrFailedJobNotFound, summarise(output))
	}
	return nil
}
