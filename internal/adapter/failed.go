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

// The phrases Laravel uses for an id it cannot find. They are read from the command's answer
// because the status alone is not the whole answer: `queue:retry` reports a missing id and
// still exits successfully, while `queue:forget` reports it and exits unsuccessfully.
var (
	retryMissing  = []string{"Unable to find failed job with ID", "Unable to find any failed jobs with ID"}
	forgetMissing = []string{"No failed job matches the ID"}
)

// RetryFailedJob asks the application to push one failed job back onto its queue, through
// `php artisan queue:retry` (docs/adr/0012). GoHorizon never reads or writes a Laravel queue
// key (handoff §13, docs/adr/0001): the application forgets the failed record and re-publishes
// the payload it stored, which is the same thing an operator running the command would get.
func (p Probe) RetryFailedJob(ctx context.Context, id string) error {
	return p.failedJobCommand(ctx, "queue:retry", retryMissing, id)
}

// ForgetFailedJob deletes one failed job through `php artisan queue:forget`
// (docs/adr/0012). As with a retry, the failed record is the application's: GoHorizon asks it
// to delete the job rather than reaching into the store itself.
func (p Probe) ForgetFailedJob(ctx context.Context, id string) error {
	return p.failedJobCommand(ctx, "queue:forget", forgetMissing, id)
}

// failedJobCommand runs one administrative queue command for id and reads its answer. It is
// shared by retry and forget because they differ only in the command and in the phrase a
// missing id leaves behind.
func (p Probe) failedJobCommand(ctx context.Context, command string, missing []string, id string) error {
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

	cmd := exec.CommandContext(ctx, p.PHP, p.Artisan, command, id)
	cmd.Dir = p.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	output := stdout.String() + "\n" + stderr.String()

	if matchesAny(output, missing) {
		return fmt.Errorf("%w: %s", ErrFailedJobNotFound, summarise(output))
	}
	if runErr != nil {
		detail := summarise(stderr.String())
		if detail == "" {
			detail = summarise(stdout.String())
		}
		return fmt.Errorf("%w: %v: %s", errUnavailable, runErr, detail)
	}
	return nil
}

func matchesAny(output string, phrases []string) bool {
	for _, phrase := range phrases {
		if strings.Contains(output, phrase) {
			return true
		}
	}
	return false
}
