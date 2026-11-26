package adapter

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// TestFailedListHelperProcess stands in for `php artisan`: it echoes the arguments it was
// given so a test can read the command that was built, prints GOHORIZON_TEST_FAILED_JOBS,
// and exits with GOHORIZON_TEST_FAILED_JOBS_EXIT when it is 1.
func TestFailedListHelperProcess(t *testing.T) {
	if os.Getenv("GOHORIZON_TEST_FAILED_LIST") != "1" {
		t.Skip("helper process only; launched by the failed-job listing tests")
	}
	os.Stdout.WriteString("args: " + strings.Join(os.Args, " ") + "\n")
	os.Stdout.WriteString(os.Getenv("GOHORIZON_TEST_FAILED_JOBS"))
	if os.Getenv("GOHORIZON_TEST_FAILED_JOBS_EXIT") == "1" {
		os.Exit(1)
	}
	os.Exit(0)
}

func failedListProbe(t *testing.T) Probe {
	t.Helper()
	t.Setenv("GOHORIZON_TEST_FAILED_LIST", "1")
	return Probe{
		PHP:     os.Args[0],
		Artisan: "-test.run=TestFailedListHelperProcess",
		Dir:     t.TempDir(),
	}
}

const failedJobsJSON = `{"schema":1,"application":"production","environment":"production",` +
	`"failed_jobs":[` +
	`{"id":"17","uuid":"e0a1c2d3-4b5f-4a6b-8c7d-9e0f1a2b3c4d","connection":"redis","queue":"emails",` +
	`"job":"App\\Jobs\\SendInvoice","failed_at":"2026-11-26 09:30:00",` +
	`"exception":"RuntimeException: could not reach the mail server"},` +
	`{"id":"16","uuid":"","connection":"redis","queue":"default","job":"App\\Jobs\\SyncOrders",` +
	`"failed_at":"2026-11-26 09:00:00","exception":""}],` +
	`"next_before":"16","generated_at":"2026-11-26T10:11:12Z"}`

func TestProbeDecodesAFailedJobsPage(t *testing.T) {
	probe := failedListProbe(t)
	t.Setenv("GOHORIZON_TEST_FAILED_JOBS", failedJobsJSON)

	page, err := probe.FailedJobs(context.Background(), FailedJobQuery{Limit: 2, Before: "42"})
	if err != nil {
		t.Fatalf("FailedJobs: %v", err)
	}
	if len(page.FailedJobs) != 2 {
		t.Fatalf("failed_jobs = %+v, want both records", page.FailedJobs)
	}
	first := page.FailedJobs[0]
	if first.ID != "17" || first.UUID != "e0a1c2d3-4b5f-4a6b-8c7d-9e0f1a2b3c4d" ||
		first.Connection != "redis" || first.Queue != "emails" ||
		first.Job != `App\Jobs\SendInvoice` || first.FailedAt != "2026-11-26 09:30:00" ||
		first.Exception != "RuntimeException: could not reach the mail server" {
		t.Errorf("first = %+v, want the reported record", first)
	}
	// A record without an exception is empty, not a made-up message.
	if page.FailedJobs[1].Exception != "" || page.FailedJobs[1].UUID != "" {
		t.Errorf("second = %+v, want the empty fields it reported", page.FailedJobs[1])
	}
	if page.NextBefore != "16" {
		t.Errorf("next_before = %q, want the cursor", page.NextBefore)
	}
}

func TestFailedJobsBuildsTheListingCommand(t *testing.T) {
	probe := failedListProbe(t)
	t.Setenv("GOHORIZON_TEST_FAILED_JOBS_EXIT", "1")
	t.Setenv("GOHORIZON_TEST_FAILED_JOBS", "artisan could not start\n")

	_, err := probe.FailedJobs(context.Background(), FailedJobQuery{Limit: 25, Before: "42"})
	if !errors.Is(err, errUnavailable) {
		t.Fatalf("error = %v, want errUnavailable", err)
	}
	// The arguments were echoed by the fake, so the error proves the adapter asked for the
	// listing command with the page it was given.
	for _, want := range []string{"gohorizon:failed:list", "--json", "--limit=25", "--before=42", "artisan could not start"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
}

func TestFailedJobsRejectsAPageItCannotAskFor(t *testing.T) {
	probe := failedListProbe(t)

	// The cursor becomes an artisan argument, so anything but digits is refused before the
	// command is built; the limit has the same bound the endpoint enforces.
	for _, query := range []FailedJobQuery{
		{Limit: 0},
		{Limit: MaxFailedJobsLimit + 1},
		{Limit: 10, Before: "-1"},
		{Limit: 10, Before: "42; rm -rf /"},
	} {
		if _, err := probe.FailedJobs(context.Background(), query); err == nil {
			t.Errorf("FailedJobs(%+v) = nil, want a refusal", query)
		}
	}
}

func TestFailedJobsReportsAnUnavailableListing(t *testing.T) {
	t.Run("no output", func(t *testing.T) {
		probe := failedListProbe(t)
		t.Setenv("GOHORIZON_TEST_FAILED_JOBS", "")

		if _, err := probe.FailedJobs(context.Background(), FailedJobQuery{Limit: 10}); !errors.Is(err, errUnavailable) {
			t.Fatalf("error = %v, want errUnavailable", err)
		}
	})

	t.Run("unknown schema", func(t *testing.T) {
		probe := failedListProbe(t)
		t.Setenv("GOHORIZON_TEST_FAILED_JOBS", `{"schema":2,"failed_jobs":[]}`)

		_, err := probe.FailedJobs(context.Background(), FailedJobQuery{Limit: 10})
		if !errors.Is(err, errUnavailable) {
			t.Fatalf("error = %v, want errUnavailable", err)
		}
		if !strings.Contains(err.Error(), "schema 2") {
			t.Errorf("error = %v, want it to report the unknown schema", err)
		}
	})
}
