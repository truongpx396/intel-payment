package ports_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/truongpx396/intel-payment/metering/ports"
)

// The catalogue an exporter registers and the table a host writes its alerts against are one list.
func TestCatalogMatchesTheObservabilityContract(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../specs/001-metering-billing-core/contracts/metering-ports.md")
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile("(?m)^\\| `(metering_[a-z_]+)` \\| (counter|gauge|histogram)(?: \\(([^)]*)\\))? \\|")
	doc := map[string]ports.MetricDef{}
	for _, m := range row.FindAllStringSubmatch(string(raw), -1) {
		d := ports.MetricDef{Name: m[1], Kind: ports.MetricKind(m[2])}
		for _, l := range strings.Split(m[3], ",") {
			if l = strings.Trim(strings.TrimSpace(l), "`"); l != "" {
				d.Labels = append(d.Labels, l)
			}
		}
		doc[d.Name] = d
	}
	if len(doc) == 0 {
		t.Fatal("found no metrics in the contract: the table format changed")
	}

	seen := map[string]bool{}
	for _, c := range ports.Catalog {
		if seen[c.Name] {
			t.Errorf("%s is in the catalogue twice", c.Name)
		}
		seen[c.Name] = true
		d, ok := doc[c.Name]
		switch {
		case !ok:
			t.Errorf("%s is in the catalogue but not in the observability contract", c.Name)
		case d.Kind != c.Kind:
			t.Errorf("%s is a %s in the catalogue and a %s in the contract", c.Name, c.Kind, d.Kind)
		case strings.Join(d.Labels, ",") != strings.Join(c.Labels, ","):
			t.Errorf("%s labels: catalogue %v, contract %v", c.Name, c.Labels, d.Labels)
		}
		if c.Unit == "" || c.Help == "" {
			t.Errorf("%s needs a unit and a description", c.Name)
		}
	}
	for name := range doc {
		if !seen[name] {
			t.Errorf("%s is in the observability contract but not in the catalogue", name)
		}
	}
}
