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

// The application reports the versions its own commands emit, so the descriptor can compare them
// with the ones this service reads before a payload is read at all (docs/adr/0014).
func TestCapabilitiesCarryThePayloadVersionsTheApplicationReports(t *testing.T) {
	caps, err := decodeCapabilities([]byte(`{"schema":1,"payload_schemas":{"inspect":2,"failed_jobs":1}}`))
	if err != nil {
		t.Fatalf("decodeCapabilities: %v", err)
	}

	if caps.Payloads.Inspect != 2 || caps.Payloads.FailedJobs != 1 {
		t.Errorf("payload_schemas = %+v, want the application's 2 and 1", caps.Payloads)
	}
	if caps.InspectPayloadSupported() {
		t.Error("InspectPayloadSupported = true for schema 2")
	}
	if !caps.FailedJobsPayloadSupported() {
		t.Error("FailedJobsPayloadSupported = false for schema 1")
	}
}

// A package older than the field reports nothing. That is not a mismatch: refusing a package for
// a claim it never made would read as a deployment fault rather than as a version skew.
func TestCapabilitiesAssumeAPayloadVersionTheyDoNotReport(t *testing.T) {
	caps, err := decodeCapabilities([]byte(`{"schema":1}`))
	if err != nil {
		t.Fatalf("decodeCapabilities: %v", err)
	}

	if !caps.InspectPayloadSupported() || !caps.FailedJobsPayloadSupported() {
		t.Errorf("payloads = %+v, want an unreported version treated as compatible", caps.Payloads)
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
