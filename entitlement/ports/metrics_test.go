package ports_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/truongpx396/intel-payment/entitlement/ports"
	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// The entitlement metrics an exporter registers and the rows a host writes its alerts against are one list — the way
// metering's Catalog and billing's MetricCatalog are held equal to their tables.
func TestEntitlementCatalogMatchesTheObservabilityContract(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../specs/001-metering-billing-core/contracts/entitlement-ports.md")
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile("(?m)^\\| `(entitlement_[a-z_]+)` \\| (counter|gauge|histogram)(?: \\(([^)]*)\\))? \\|")
	doc := map[string]mports.MetricDef{}
	for _, m := range row.FindAllStringSubmatch(string(raw), -1) {
		d := mports.MetricDef{Name: m[1], Kind: mports.MetricKind(m[2])}
		for _, l := range strings.Split(m[3], ",") {
			if l = strings.Trim(strings.TrimSpace(l), "`"); l != "" {
				d.Labels = append(d.Labels, l)
			}
		}
		doc[d.Name] = d
	}
	if len(doc) == 0 {
		t.Fatal("found no entitlement metrics in the contract: the table format changed")
	}
	seen := map[string]bool{}
	for _, c := range ports.MetricCatalog {
		if seen[c.Name] {
			t.Errorf("%s is in the metric catalogue twice", c.Name)
		}
		seen[c.Name] = true
		d, ok := doc[c.Name]
		switch {
		case !ok:
			t.Errorf("%s is in the metric catalogue but not in the observability contract", c.Name)
		case d.Kind != c.Kind:
			t.Errorf("%s is a %s in the metric catalogue and a %s in the contract", c.Name, c.Kind, d.Kind)
		case strings.Join(d.Labels, ",") != strings.Join(c.Labels, ","):
			t.Errorf("%s labels: catalogue %v, contract %v", c.Name, c.Labels, d.Labels)
		}
		if c.Unit == "" || c.Help == "" {
			t.Errorf("%s needs a unit and a description", c.Name)
		}
	}
	for name := range doc {
		if !seen[name] {
			t.Errorf("%s is in the observability contract but not in the metric catalogue", name)
		}
	}
}

// The names the Entitler counts under are the catalogued ones, and the contracts a host alerts on do not drift.
func TestTheMetricNamesAreTheContractsOwn(t *testing.T) {
	t.Parallel()
	if ports.MetricDecisions != "entitlement_decisions_total" || ports.MetricInvalid != "entitlement_invalid_total" {
		t.Fatal("a host writes alerts against these names: they must not change")
	}
}
