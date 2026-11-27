package adapter

import (
	"fmt"
	"strings"
	"testing"
)

// The published payload schemas are only worth publishing if the decoders enforce them, so these
// pin the refusal behind the number in the descriptor: a payload declaring another version is
// refused with both numbers rather than read with fields that may mean something else
// (docs/adr/0014).
func TestDecodeInspectionRefusesAPayloadVersionItDoesNotUnderstand(t *testing.T) {
	payload := fmt.Sprintf(`{"schema":%d,"connection":"redis","queues":[{"queue":"default","pending":1}]}`,
		InspectionSchema+1)

	_, err := decodeInspection([]byte(payload))
	if err == nil {
		t.Fatalf("decodeInspection accepted schema %d", InspectionSchema+1)
	}
	for _, want := range []string{
		fmt.Sprintf("schema %d", InspectionSchema+1),
		fmt.Sprintf("schema %d", InspectionSchema),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
}

func TestDecodeFailedJobsRefusesAPayloadVersionItDoesNotUnderstand(t *testing.T) {
	payload := fmt.Sprintf(`{"schema":%d,"failed_jobs":[],"next_before":null}`, FailedJobsSchema+1)

	_, err := decodeFailedJobs([]byte(payload))
	if err == nil {
		t.Fatalf("decodeFailedJobs accepted schema %d", FailedJobsSchema+1)
	}
	for _, want := range []string{
		fmt.Sprintf("schema %d", FailedJobsSchema+1),
		fmt.Sprintf("schema %d", FailedJobsSchema),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
}
