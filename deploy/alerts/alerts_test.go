// Package alerts_test holds deploy/alerts.yml to docs/operations.md. An alert nobody can act on is noise, and a runbook entry nothing
// pages for is a procedure nobody will find at 3 a.m.: the two are one set, and a metric an alert or an entry names is a metric that exists.
package alerts_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"

	billingports "github.com/truongpx396/intel-payment/billing/ports"
	entports "github.com/truongpx396/intel-payment/entitlement/ports"
	"github.com/truongpx396/intel-payment/events"
	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// R114 (the way ports.Catalog is held equal to the contract's metric table): "an alert with no runbook entry, or an entry with no alert,
// fails."

type ruleFile struct {
	Groups []struct {
		Name  string `yaml:"name"`
		Rules []struct {
			Alert       string            `yaml:"alert"`
			Expr        string            `yaml:"expr"`
			For         string            `yaml:"for"`
			Labels      map[string]string `yaml:"labels"`
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

var (
	anchorOf    = regexp.MustCompile(`^docs/operations\.md#(alert-[a-z0-9-]+)$`)
	metricName  = regexp.MustCompile(`\b((?:metering|billing|entitlement|events)_[a-z][a-z0-9_]*)\b`)
	backticked  = regexp.MustCompile("`((?:metering|billing|entitlement|events)_[a-z][a-z0-9_]*)(?:\\{[^`]*\\})?`")
	heading     = regexp.MustCompile(`(?m)^## (Alert: .+)$`)
	histogramSf = []string{"_bucket", "_sum", "_count"}
)

// slug is the anchor GitHub gives a heading: lower case, punctuation dropped, spaces to hyphens.
func slug(h string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(h) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	return b.String()
}

// catalogue is every metric a deployment exports.
func catalogue() map[string]bool {
	out := map[string]bool{}
	for _, d := range mports.Catalog {
		out[d.Name] = true
	}
	for _, d := range billingports.MetricCatalog {
		out[d.Name] = true
	}
	for _, d := range entports.MetricCatalog {
		out[d.Name] = true
	}
	for _, d := range events.MetricCatalog {
		out[d.Name] = true
	}
	return out
}

func known(name string, metrics map[string]bool) bool {
	if metrics[name] {
		return true
	}
	for _, sf := range histogramSf {
		if base, ok := strings.CutSuffix(name, sf); ok && metrics[base] {
			return true
		}
	}
	return false
}

var createTable = regexp.MustCompile(`(?m)^CREATE TABLE (?:IF NOT EXISTS )?([a-z_]+)`)

// tables are the names the schema gives its tables. A runbook that says `entitlement_overrides` means the table, and several share a prefix
// with a metric's component.
func tables(t testing.TB) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations found: %v", err)
	}
	out := map[string]bool{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range createTable.FindAllStringSubmatch(string(raw), -1) {
			out[m[1]] = true
		}
	}
	return out
}

// check returns every way the alert rules and the runbook disagree, in a stable order.
func check(rules []byte, runbook string, metrics, notMetrics map[string]bool) []string {
	var problems []string
	var f ruleFile
	if err := yaml.Unmarshal(rules, &f); err != nil {
		return []string{"the alert rules are not YAML: " + err.Error()}
	}
	entries := map[string]string{} // anchor → heading
	for _, m := range heading.FindAllStringSubmatch(runbook, -1) {
		entries[slug(m[1])] = m[1]
	}
	if len(entries) == 0 {
		problems = append(problems, "found no `## Alert:` entries in the runbook: its format changed")
	}
	referenced := map[string]bool{}
	names := map[string]bool{}
	alerts := 0
	for _, g := range f.Groups {
		for _, r := range g.Rules {
			alerts++
			switch {
			case r.Alert == "":
				problems = append(problems, "a rule has no alert name")
				continue
			case names[r.Alert]:
				problems = append(problems, r.Alert+" is defined twice")
			}
			names[r.Alert] = true
			if strings.TrimSpace(r.Expr) == "" {
				problems = append(problems, r.Alert+" has no expression")
			}
			if sev := r.Labels["severity"]; sev != "page" && sev != "ticket" {
				problems = append(problems, fmt.Sprintf("%s has severity %q: it is `page` or `ticket`", r.Alert, sev))
			}
			if strings.TrimSpace(r.Annotations["summary"]) == "" {
				problems = append(problems, r.Alert+" has no summary")
			}
			m := anchorOf.FindStringSubmatch(r.Annotations["runbook"])
			switch {
			case m == nil:
				problems = append(problems, fmt.Sprintf("%s has no runbook link of the form docs/operations.md#alert-…: %q", r.Alert, r.Annotations["runbook"]))
			case entries[m[1]] == "":
				problems = append(problems, fmt.Sprintf("%s links to #%s, which is no entry in the runbook", r.Alert, m[1]))
			default:
				referenced[m[1]] = true
			}
			for _, mm := range metricName.FindAllStringSubmatch(r.Expr, -1) {
				if !known(mm[1], metrics) {
					problems = append(problems, fmt.Sprintf("%s reads %s, which no component exports", r.Alert, mm[1]))
				}
			}
		}
	}
	if alerts == 0 {
		problems = append(problems, "found no alerts in the rules: the file's shape changed")
	}
	for anchor, h := range entries {
		if !referenced[anchor] {
			problems = append(problems, fmt.Sprintf("the runbook entry %q has no alert: nothing pages for it", h))
		}
	}
	for _, mm := range backticked.FindAllStringSubmatch(runbook, -1) {
		if !known(mm[1], metrics) && !notMetrics[mm[1]] {
			problems = append(problems, fmt.Sprintf("the runbook names %s, which no component exports", mm[1]))
		}
	}
	sort.Strings(problems)
	return slices.Compact(problems)
}

func TestTheShippedAlertsAndTheRunbookAreOneSet(t *testing.T) {
	t.Parallel()
	rules, err := os.ReadFile("../alerts.yml")
	if err != nil {
		t.Fatalf("deploy/alerts.yml: %v", err)
	}
	runbook, err := os.ReadFile("../../docs/operations.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range check(rules, string(runbook), catalogue(), tables(t)) {
		t.Error(p)
	}
}

const goodRules = `
groups:
  - name: demo
    rules:
      - alert: DriftFinding
        expr: increase(metering_drift_findings_total[15m]) > 0
        for: 0m
        labels: {severity: page}
        annotations:
          summary: drift
          runbook: docs/operations.md#alert-drift-finding
`

const goodRunbook = "## Alert: drift finding\n\nSee `metering_drift_findings_total{outcome=\"drift\"}`.\n"

// The guard is shown able to fail: each of these is a way the two can disagree, and each is reported by name. An unarmed guard passes
// everything.
func TestTheCheckRejectsEveryWayTheAlertsAndTheRunbookCanDisagree(t *testing.T) {
	t.Parallel()
	metrics := catalogue()
	if got := check([]byte(goodRules), goodRunbook, metrics, nil); len(got) != 0 {
		t.Fatalf("a consistent pair is reported: %v", got)
	}
	for name, tc := range map[string]struct {
		rules, runbook, want string
	}{
		"an alert with no runbook entry": {goodRules, goodRunbook + "\n## Alert: something else\n", `the runbook entry "Alert: something else" has no alert`},
		"an entry with no alert":         {strings.Replace(goodRules, "#alert-drift-finding", "#alert-no-such-entry", 1), goodRunbook, "links to #alert-no-such-entry, which is no entry in the runbook"},
		"a runbook with no entries":      {goodRules, "# nothing here\n", "found no `## Alert:` entries"},
		"an alert with no link":          {strings.Replace(goodRules, "runbook: docs/operations.md#alert-drift-finding", "runbook: ''", 1), goodRunbook, "DriftFinding has no runbook link"},
		"a link to another file":         {strings.Replace(goodRules, "docs/operations.md#", "docs/other.md#", 1), goodRunbook, "DriftFinding has no runbook link"},
		"an alert on a metric that does not exist": {strings.Replace(goodRules, "metering_drift_findings_total", "metering_drift_finding_total", 1), goodRunbook,
			"DriftFinding reads metering_drift_finding_total, which no component exports"},
		"a runbook naming a metric that does not exist": {goodRules, goodRunbook + "Also `metering_admit_fail_open_total`.\n", "the runbook names metering_admit_fail_open_total, which no component exports"},
		"a severity nobody defined":                     {strings.Replace(goodRules, "severity: page", "severity: urgent", 1), goodRunbook, `DriftFinding has severity "urgent"`},
		"no severity":                                   {strings.Replace(goodRules, "{severity: page}", "{}", 1), goodRunbook, `DriftFinding has severity ""`},
		"no expression":                                 {strings.Replace(goodRules, "increase(metering_drift_findings_total[15m]) > 0", "''", 1), goodRunbook, "DriftFinding has no expression"},
		"no summary":                                    {strings.Replace(goodRules, "summary: drift", "summary: ''", 1), goodRunbook, "DriftFinding has no summary"},
		"two alerts of one name": {goodRules + `
      - alert: DriftFinding
        expr: metering_dead_letters > 0
        labels: {severity: page}
        annotations: {summary: x, runbook: docs/operations.md#alert-drift-finding}
`, goodRunbook, "DriftFinding is defined twice"},
		"rules that are not YAML": {"groups: [", goodRunbook, "the alert rules are not YAML"},
		"rules with no alerts":    {"groups: []", goodRunbook, "found no alerts in the rules"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := check([]byte(tc.rules), tc.runbook, metrics, nil)
			for _, p := range got {
				if strings.Contains(p, tc.want) {
					return
				}
			}
			t.Fatalf("not reported: want %q, got %v", tc.want, got)
		})
	}
}

func TestATableNameInTheRunbookIsNotMistakenForAMetric(t *testing.T) {
	t.Parallel()
	metrics := catalogue()
	rb := goodRunbook + "Look in `entitlement_overrides`.\n"
	if got := check([]byte(goodRules), rb, metrics, tables(t)); len(got) != 0 {
		t.Fatalf("a table is not a metric: %v", got)
	}
	if got := check([]byte(goodRules), rb, metrics, nil); len(got) != 1 {
		t.Fatalf("without the schema it cannot tell: %v", got)
	}
	if !tables(t)["entitlement_overrides"] || !tables(t)["dead_letters"] || tables(t)["metering_dead_letters"] {
		t.Fatal("the tables are read from the migrations")
	}
}

func TestAHistogramsSuffixedSeriesAreTheMetricsOwn(t *testing.T) {
	t.Parallel()
	metrics := catalogue()
	rules := strings.Replace(goodRules, "increase(metering_drift_findings_total[15m]) > 0", "histogram_quantile(0.99, rate(metering_admit_latency_seconds_bucket[5m])) > 0.005", 1)
	if got := check([]byte(rules), goodRunbook, metrics, nil); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

func TestTheAnchorIsTheOneGitHubGivesAHeading(t *testing.T) {
	t.Parallel()
	for h, want := range map[string]string{
		"Alert: drift finding":                                              "alert-drift-finding",
		"Alert: regression, or a shard frozen":                              "alert-regression-or-a-shard-frozen",
		"Alert: a replica's entitlement cache is not hearing announcements": "alert-a-replicas-entitlement-cache-is-not-hearing-announcements",
		"Alert: suspense open, late replays, idempotency conflicts":         "alert-suspense-open-late-replays-idempotency-conflicts",
		"Alert: DLQ dead count":                                             "alert-dlq-dead-count",
	} {
		if got := slug(h); got != want {
			t.Errorf("%q → %q, want %q", h, got, want)
		}
	}
}
