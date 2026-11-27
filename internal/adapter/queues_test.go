package adapter

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const inspectionJSON = `{"schema":1,"application":"production","environment":"production",` +
	`"connection":"redis","queues":[` +
	`{"queue":"default","driver":"RedisQueue","pending":12,"delayed":3,"reserved":1,"oldest_pending_at":1763600000},` +
	`{"queue":"emails","driver":"RedisQueue","pending":0,"delayed":0,"reserved":0,` +
	`"oldest_pending_error":"RedisException"}],` +
	`"generated_at":"2026-11-20T10:11:12Z"}`

func TestProbeDecodesInspection(t *testing.T) {
	tests := []struct{ name, payload string }{
		{"documented payload", inspectionJSON},
		{"application output before the json line", "PHP Deprecated: something happened\n" + inspectionJSON},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GOHORIZON_TEST_CAPABILITIES", tc.payload)

			inspection, err := testProbe(t).Queues(context.Background(), "redis", []string{"default", "emails"})
			if err != nil {
				t.Fatalf("Queues: %v", err)
			}
			if inspection.Schema != InspectionSchema || inspection.Connection != "redis" {
				t.Errorf("inspection = %+v, want schema %d on redis", inspection, InspectionSchema)
			}
			if len(inspection.Queues) != 2 {
				t.Fatalf("queues = %+v, want both asked-for queues", inspection.Queues)
			}
			defaultQueue := inspection.Queues[0]
			if defaultQueue.Queue != "default" ||
				defaultQueue.Pending == nil || *defaultQueue.Pending != 12 ||
				defaultQueue.Reserved == nil || *defaultQueue.Reserved != 1 ||
				defaultQueue.Delayed == nil || *defaultQueue.Delayed != 3 {
				t.Errorf("default = %+v, want the reported counts", defaultQueue)
			}
			if defaultQueue.OldestPendingAt == nil || *defaultQueue.OldestPendingAt != 1763600000 {
				t.Errorf("oldest_pending_at = %v, want the reported timestamp", defaultQueue.OldestPendingAt)
			}
			// The backlog's age can fail on its own: the error survives without a timestamp.
			emails := inspection.Queues[1]
			if emails.OldestPendingAt != nil || emails.OldestPendingError == nil ||
				*emails.OldestPendingError != "RedisException" {
				t.Errorf("emails = %+v, want the failed age with no timestamp", emails)
			}
		})
	}
}

func TestProbeKeepsUnreportedCountsDistinctFromZero(t *testing.T) {
	// A driver that cannot answer the counts omits the keys entirely.
	t.Setenv("GOHORIZON_TEST_CAPABILITIES",
		`{"schema":1,"connection":"sync","queues":[{"queue":"default","driver":"SyncQueue"}]}`)

	inspection, err := testProbe(t).Queues(context.Background(), "sync", []string{"default"})
	if err != nil {
		t.Fatalf("Queues: %v", err)
	}
	depth := inspection.Queues[0]
	if depth.Pending != nil || depth.Reserved != nil || depth.Delayed != nil ||
		depth.OldestPendingAt != nil || depth.OldestPendingError != nil {
		t.Errorf("depth = %+v, want nil counts for a driver that cannot report them", depth)
	}
}

func TestProbeReportsAnUnavailableInspection(t *testing.T) {
	t.Run("command fails", func(t *testing.T) {
		t.Setenv("GOHORIZON_TEST_CAPABILITIES", "Could not open input file: artisan\n")
		t.Setenv("GOHORIZON_TEST_CAPABILITIES_EXIT", "1")

		_, err := testProbe(t).Queues(context.Background(), "redis", []string{"default"})
		if !errors.Is(err, errUnavailable) {
			t.Fatalf("error = %v, want errUnavailable", err)
		}
		if !strings.Contains(err.Error(), "Could not open input file: artisan") {
			t.Errorf("error = %v, want the artisan output in the message", err)
		}
	})

	t.Run("no queues reported", func(t *testing.T) {
		t.Setenv("GOHORIZON_TEST_CAPABILITIES", `{"schema":1,"connection":"redis","queues":[]}`)

		if _, err := testProbe(t).Queues(context.Background(), "redis", nil); !errors.Is(err, errUnavailable) {
			t.Fatalf("error = %v, want errUnavailable", err)
		}
	})

	t.Run("unknown schema", func(t *testing.T) {
		t.Setenv("GOHORIZON_TEST_CAPABILITIES", `{"schema":2,"connection":"redis","queues":[{"queue":"default"}]}`)

		_, err := testProbe(t).Queues(context.Background(), "redis", []string{"default"})
		if !errors.Is(err, errUnavailable) {
			t.Fatalf("error = %v, want errUnavailable", err)
		}
		if !strings.Contains(err.Error(), "schema 2") {
			t.Errorf("error = %v, want it to report the unknown schema", err)
		}
	})
}
