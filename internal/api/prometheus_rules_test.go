package api

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	yaml "github.com/knadh/koanf/parsers/yaml"
)

// The shipped rules and the runbook they point at, relative to this package's directory. The
// point of reading them from disk is that the files an operator deploys are the ones under test
// (docs/adr/0014).
const (
	alertingRulesPath = "../../deployments/prometheus/gohorizon.rules.yml"
	runbookPath       = "../../docs/runbook.md"
)

// alertMetricPattern is how a rule's expression is scanned for the samples it names: every
// gohorizon_* token in it has to be a family this service actually writes.
var alertMetricPattern = regexp.MustCompile(`\bgohorizon_[a-z0-9_]+`)

// alertingRule is the part of a rule the tests check: the expression that has to name real
// samples, and the annotations an operator follows.
type alertingRule struct {
	Alert    string
	Expr     string
	For      string
	Severity string
	Summary  string
	Runbook  string
}

// loadAlertingRules reads the shipped rule file. YAML decoding goes through the parser the
// configuration already depends on, so the test adds no dependency of its own.
func loadAlertingRules(t *testing.T) []alertingRule {
	t.Helper()

	data, err := os.ReadFile(alertingRulesPath)
	if err != nil {
		t.Fatalf("read %s: %v", alertingRulesPath, err)
	}
	document, err := yaml.Parser().Unmarshal(data)
	if err != nil {
		t.Fatalf("parse %s: %v", alertingRulesPath, err)
	}

	var rules []alertingRule
	for _, group := range asSlice(t, document["groups"]) {
		for _, entry := range asSlice(t, asMap(t, group)["rules"]) {
			rule := asMap(t, entry)
			labels := asMap(t, rule["labels"])
			annotations := asMap(t, rule["annotations"])
			rules = append(rules, alertingRule{
				Alert:    asString(t, rule["alert"]),
				Expr:     asString(t, rule["expr"]),
				For:      asString(t, rule["for"]),
				Severity: asString(t, labels["severity"]),
				Summary:  asString(t, annotations["summary"]),
				Runbook:  asString(t, annotations["runbook"]),
			})
		}
	}
	if len(rules) == 0 {
		t.Fatalf("%s has no rules", alertingRulesPath)
	}
	return rules
}

// TestAlertingRulesNameOnlyTheSamplesThisServiceWrites is the half a rule file usually gets wrong
// after a rename: an expression that names a family the exporter no longer writes silently never
// fires. The scrape rendered here is the ground truth.
func TestAlertingRulesNameOnlyTheSamplesThisServiceWrites(t *testing.T) {
	exposed := map[string]bool{}
	for _, line := range strings.Split(exposition(prometheusReport()), "\n") {
		if !strings.HasPrefix(line, "# TYPE ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 3 {
			exposed[fields[2]] = true
		}
	}

	for _, rule := range loadAlertingRules(t) {
		metrics := alertMetricPattern.FindAllString(rule.Expr, -1)
		if len(metrics) == 0 {
			t.Errorf("%s: expression %q names no sample", rule.Alert, rule.Expr)
			continue
		}
		for _, metric := range metrics {
			if !exposed[metric] {
				t.Errorf("%s: expression names %s, which /metrics does not write", rule.Alert, metric)
			}
		}
	}
}

// TestAlertingRulesCanBeActedOn checks the parts an operator depends on and a validator would
// not: a debounce-safe hold time, a severity a routing tree can match, a summary, and a runbook
// section that exists and says what to do.
func TestAlertingRulesCanBeActedOn(t *testing.T) {
	runbook := readRunbook(t)
	sections := runbookSections(runbook)

	want := map[string]bool{
		"GoHorizonCapabilitiesReadOnly":        false,
		"GoHorizonCapabilitiesReadOnlyTooLong": false,
		"GoHorizonCapabilitiesContradiction":   false,
		"GoHorizonCapabilitiesLoopStale":       false,
	}
	for _, rule := range loadAlertingRules(t) {
		seen, known := want[rule.Alert]
		if !known {
			t.Errorf("unexpected rule %q: the states are read-only, its age, the contradiction and a stale loop", rule.Alert)
			continue
		}
		if seen {
			t.Errorf("%s: declared twice", rule.Alert)
		}
		want[rule.Alert] = true

		if hold, err := time.ParseDuration(rule.For); err != nil || hold < time.Minute {
			t.Errorf("%s: for = %q, want at least a minute so a single slow read cannot fire it", rule.Alert, rule.For)
		}
		if rule.Severity != "warning" && rule.Severity != "critical" {
			t.Errorf("%s: severity = %q, want warning or critical", rule.Alert, rule.Severity)
		}
		if strings.TrimSpace(rule.Summary) == "" {
			t.Errorf("%s: no summary annotation", rule.Alert)
		}

		// The fragment is the heading, lowercased the way a Markdown anchor resolves: an alert
		// whose link lands nowhere is an alert with no instruction.
		if fragment := "#" + strings.ToLower(rule.Alert); !strings.HasSuffix(rule.Runbook, fragment) {
			t.Errorf("%s: runbook = %q, want it to end with %q", rule.Alert, rule.Runbook, fragment)
		}
		body, ok := sections[rule.Alert]
		if !ok {
			t.Errorf("%s: %s has no %q section", rule.Alert, runbookPath, rule.Alert)
			continue
		}
		for _, marker := range []string{"**Means:**", "**Do:**"} {
			if !strings.Contains(body, marker) {
				t.Errorf("%s: section has no %s line", rule.Alert, marker)
			}
		}
	}

	for alert, seen := range want {
		if !seen {
			t.Errorf("%s: in the expected states but not in %s", alert, alertingRulesPath)
		}
	}
}

// TestAlertingRulesCoverTheAgeFromTheDescriptor is the point of shipping the rules with the
// metrics: the read-only age has an expression an operator can copy, computed from the instant
// the transition stamped, rather than a gauge everyone has to guess how to use.
func TestAlertingRulesCoverTheAgeFromTheDescriptor(t *testing.T) {
	ageRule, found := "", false
	for _, rule := range loadAlertingRules(t) {
		if strings.Contains(rule.Expr, metricCapsReadOnlySince) {
			ageRule, found = rule.Expr, true
		}
	}
	if !found {
		t.Fatalf("no rule uses %s", metricCapsReadOnlySince)
	}
	for _, want := range []string{"time()", "time() - " + metricCapsReadOnlySince, metricCapsReadOnlySince + " > 0"} {
		if !strings.Contains(ageRule, want) {
			t.Errorf("age expression %q has no %q", ageRule, want)
		}
	}

	body := exposition(prometheusReport())
	if !strings.Contains(body, "# TYPE "+metricCapsReadOnlySince+" gauge") {
		t.Errorf("/metrics does not publish %s", metricCapsReadOnlySince)
	}
}

// TestLoopStaleLeavesANeverAnsweredLoopToReadOnly pins the guard on the stale rule. last_success
// is 0 until a read answers, and a deployment whose reads never answered is read-only, which the
// read-only alert already reports: comparing an age from zero would alert twice on one cause and
// state an age nothing measured. The runbook has to say which alert owns that case, or the two
// rules read as one gap (docs/adr/0014).
func TestLoopStaleLeavesANeverAnsweredLoopToReadOnly(t *testing.T) {
	sections := runbookSections(readRunbook(t))

	for _, rule := range loadAlertingRules(t) {
		if rule.Alert != "GoHorizonCapabilitiesLoopStale" {
			continue
		}
		if !strings.Contains(rule.Expr, metricCapsLastSuccess+" > 0") {
			t.Errorf("expression %q has no %s > 0 guard: a loop that never answered has no age", rule.Expr, metricCapsLastSuccess)
		}
		if !strings.Contains(rule.Expr, "time() - "+metricCapsLastSuccess) {
			t.Errorf("expression %q does not compare the instant's age", rule.Expr)
		}
		if body := sections[rule.Alert]; !strings.Contains(body, "GoHorizonCapabilitiesReadOnly") {
			t.Errorf("runbook section %q does not name the alert that covers a loop which never answered", rule.Alert)
		}
		return
	}
	t.Fatal("GoHorizonCapabilitiesLoopStale is missing")
}

func readRunbook(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Clean(runbookPath))
	if err != nil {
		t.Fatalf("read %s: %v", runbookPath, err)
	}
	return string(data)
}

// runbookSections splits the runbook on its second-level headings, which are the alert names the
// rule file links to.
func runbookSections(runbook string) map[string]string {
	sections := map[string]string{}
	current := ""
	for _, line := range strings.Split(runbook, "\n") {
		if heading, ok := strings.CutPrefix(line, "## "); ok {
			current = strings.TrimSpace(heading)
			sections[current] = ""
			continue
		}
		if current != "" {
			sections[current] += line + "\n"
		}
	}
	return sections
}

func asMap(t *testing.T, value any) map[string]any {
	t.Helper()

	switch typed := value.(type) {
	case nil:
		return map[string]any{}
	case map[string]any:
		return typed
	case map[any]any:
		out := make(map[string]any, len(typed))
		for key, entry := range typed {
			out[asString(t, key)] = entry
		}
		return out
	default:
		t.Fatalf("value %v (%T) is not a mapping", value, value)
		return nil
	}
}

func asSlice(t *testing.T, value any) []any {
	t.Helper()

	switch typed := value.(type) {
	case nil:
		return nil
	case []any:
		return typed
	default:
		t.Fatalf("value %v (%T) is not a list", value, value)
		return nil
	}
}

func asString(t *testing.T, value any) string {
	t.Helper()

	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	default:
		t.Fatalf("value %v (%T) is not a string", value, value)
		return ""
	}
}
