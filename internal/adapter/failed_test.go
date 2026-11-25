package adapter

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// TestRetryHelperProcess stands in for `php artisan`: it echoes the arguments it was given so a
// test can read the command that was built, prints GOHORIZON_TEST_RETRY_OUTPUT, and exits with
// GOHORIZON_TEST_RETRY_EXIT when it is 1 — the status a command that could not start at all
// leaves behind.
func TestRetryHelperProcess(t *testing.T) {
	if os.Getenv("GOHORIZON_TEST_RETRY") != "1" {
		t.Skip("helper process only; launched by the failed-job retry tests")
	}
	os.Stdout.WriteString("args: " + strings.Join(os.Args, " ") + "\n")
	os.Stdout.WriteString(os.Getenv("GOHORIZON_TEST_RETRY_OUTPUT"))
	if os.Getenv("GOHORIZON_TEST_RETRY_EXIT") == "1" {
		os.Exit(1)
	}
	os.Exit(0)
}

func retryProbe(t *testing.T) Probe {
	t.Helper()
	t.Setenv("GOHORIZON_TEST_RETRY", "1")
	return Probe{
		PHP:     os.Args[0],
		Artisan: "-test.run=TestRetryHelperProcess",
		Dir:     t.TempDir(),
	}
}

func TestRetryFailedJobRunsQueueRetryForTheId(t *testing.T) {
	probe := retryProbe(t)
	t.Setenv("GOHORIZON_TEST_RETRY_EXIT", "1")
	t.Setenv("GOHORIZON_TEST_RETRY_OUTPUT", "artisan could not start\n")

	err := probe.RetryFailedJob(context.Background(), "job-1")
	if !errors.Is(err, errUnavailable) {
		t.Fatalf("error = %v, want errUnavailable", err)
	}
	// The command, the subcommand and the id are all in the error because the fake echoed its
	// arguments: this is what proves the adapter built `queue:retry <id>` and not something else.
	for _, want := range []string{"queue:retry", "job-1", "artisan could not start"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
}

func TestRetryFailedJobAcceptsTheApplicationAnswer(t *testing.T) {
	probe := retryProbe(t)
	t.Setenv("GOHORIZON_TEST_RETRY_OUTPUT", "Pushing failed queue jobs back onto the queue.\n")

	if err := probe.RetryFailedJob(context.Background(), "job-1"); err != nil {
		t.Fatalf("RetryFailedJob: %v", err)
	}
}

func TestRetryFailedJobReportsAnIdTheApplicationCannotFind(t *testing.T) {
	// `queue:retry` writes both of these and still exits successfully, so a caller that reads
	// only the status would report a retry that never happened.
	outputs := []string{
		"  ERROR  Unable to find failed job with ID [job-1].\n",
		"  ERROR  Unable to find any failed jobs with ID [job-1].\n",
	}
	for _, output := range outputs {
		probe := retryProbe(t)
		t.Setenv("GOHORIZON_TEST_RETRY_OUTPUT", output)

		err := probe.RetryFailedJob(context.Background(), "job-1")
		if !errors.Is(err, ErrFailedJobNotFound) {
			t.Errorf("output %q: error = %v, want ErrFailedJobNotFound", output, err)
		}
	}
}

func TestRetryFailedJobRefusesAnIdThatIsNotAnArgument(t *testing.T) {
	probe := retryProbe(t)

	// An id starting with '-' would be read as an artisan option rather than a failed job, so
	// it never reaches the command. The empty id is the same refusal one step earlier.
	for _, id := range []string{"", "   ", "--queue=all"} {
		if err := probe.RetryFailedJob(context.Background(), id); err == nil {
			t.Errorf("RetryFailedJob(%q) = nil, want a refusal", id)
		}
	}
}
