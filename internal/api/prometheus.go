package api

import (
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/gohorizon/gohorizon/internal/health"
)

// prometheusContentType is the exposition format's own media type. A bare text/plain is
// accepted too, but announcing the version lets a scraper pick the right parser.
const prometheusContentType = "text/plain; version=0.0.4; charset=utf-8"

// The metric names are part of the contract an alerting rule or a panel is written
// against, so they are fixed and renamed only as a breaking change.
const (
	metricUp              = "gohorizon_up"
	metricLockHeld        = "gohorizon_lock_held"
	metricTelemetryEvents = "gohorizon_telemetry_events_total"
	metricQueueJobs       = "gohorizon_queue_jobs"
	metricWorkers         = "gohorizon_workers"
	metricCrashLooping    = "gohorizon_crash_looping_workers"
)

// labelValueEscaper escapes a label value as the text format requires: a backslash, the
// quote that would otherwise end the value, and the line feed that would otherwise split
// the sample. HELP text is a constant here, so it cannot hold a character that needs this.
var labelValueEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// metricsExposition serves the state /api/v1/health reports in the Prometheus text
// format. There is no client library: the body is six fixed families, fewer lines than the
// dependency would add. Like health it is read-only and unauthenticated, and it reads no
// store, so a disabled database changes nothing here.
func metricsExposition(provider Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", prometheusContentType)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, exposition(provider.Report()))
	}
}

// exposition renders one report as the format's families: every sample of a family
// together, each preceded by its HELP and TYPE lines.
func exposition(report health.Report) string {
	var b strings.Builder

	writeFamily(&b, metricUp, "gauge",
		"1 while GoHorizon answers a scrape; a failed scrape is the absence of this sample.")
	writeSample(&b, metricUp, 1)

	held := 0
	if report.Lock.Held {
		held = 1
	}
	writeFamily(&b, metricLockHeld, "gauge",
		"1 when this instance holds the supervisor lock, 0 when it has lost it or cannot confirm it.")
	writeSample(&b, metricLockHeld, held)

	// These are counters since start, so zero is a fact about the run and is always written.
	writeFamily(&b, metricTelemetryEvents, "counter",
		"Events the telemetry consumer has handled since start, by result.")
	writeSample(&b, metricTelemetryEvents, report.Telemetry.Events, "result", "event")
	writeSample(&b, metricTelemetryEvents, report.Telemetry.Duplicates, "result", "duplicate")
	writeSample(&b, metricTelemetryEvents, report.Telemetry.Invalid, "result", "invalid")
	writeSample(&b, metricTelemetryEvents, report.Telemetry.Errors, "result", "error")

	// A depth the driver could not report has no sample: writing it as 0 would claim the
	// queue is empty when nobody knows.
	writeFamily(&b, metricQueueJobs, "gauge",
		"Jobs waiting in each queue by state, from the last inspection.")
	for _, depth := range sortedDepths(report.Queues.Depths) {
		if depth.Pending != nil {
			writeSample(&b, metricQueueJobs, *depth.Pending, "queue", depth.Queue, "state", "pending")
		}
		if depth.Reserved != nil {
			writeSample(&b, metricQueueJobs, *depth.Reserved, "queue", depth.Queue, "state", "reserved")
		}
		if depth.Delayed != nil {
			writeSample(&b, metricQueueJobs, *depth.Delayed, "queue", depth.Queue, "state", "delayed")
		}
	}

	writeFamily(&b, metricWorkers, "gauge",
		"Workers a supervisor has in each state it currently occupies.")
	for _, supervisor := range report.Supervisors {
		counts := make(map[string]int, len(supervisor.Workers))
		for _, worker := range supervisor.Workers {
			counts[worker.State]++
		}
		for _, state := range slices.Sorted(maps.Keys(counts)) {
			writeSample(&b, metricWorkers, counts[state], "supervisor", supervisor.ID, "state", state)
		}
	}

	// Every supervisor gets a sample: an alert on this gauge needs a zero to compare, and
	// the count is known whether or not anyone is crash looping.
	writeFamily(&b, metricCrashLooping, "gauge",
		"Workers currently crash looping, per supervisor.")
	for _, supervisor := range report.Supervisors {
		crashLooping := 0
		for _, worker := range supervisor.Workers {
			if worker.CrashLooping {
				crashLooping++
			}
		}
		writeSample(&b, metricCrashLooping, crashLooping, "supervisor", supervisor.ID)
	}

	return b.String()
}

// sortedDepths orders the queues by name, so two scrapes of the same state are the same
// bytes whatever order the inspection command printed.
func sortedDepths(depths []health.QueueDepth) []health.QueueDepth {
	ordered := slices.Clone(depths)
	slices.SortFunc(ordered, func(a, b health.QueueDepth) int { return strings.Compare(a.Queue, b.Queue) })
	return ordered
}

func writeFamily(b *strings.Builder, name, kind, help string) {
	b.WriteString("# HELP " + name + " " + help + "\n")
	b.WriteString("# TYPE " + name + " " + kind + "\n")
}

// writeSample writes one sample. labels are alternating name and value pairs, in the order
// the family documents them.
func writeSample(b *strings.Builder, name string, value int, labels ...string) {
	b.WriteString(name)
	if len(labels) > 0 {
		b.WriteString("{")
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(labels[i])
			b.WriteString(`="`)
			b.WriteString(labelValueEscaper.Replace(labels[i+1]))
			b.WriteString(`"`)
		}
		b.WriteString("}")
	}
	b.WriteString(" ")
	b.WriteString(strconv.Itoa(value))
	b.WriteString("\n")
}
