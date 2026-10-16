package ports_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	mports "github.com/truongpx396/intel-payment/metering/ports"

	"github.com/truongpx396/intel-payment/billing/ports"
)

// The billing metrics an exporter registers and the rows a host writes its alerts against are one list — the way
// metering's Catalog is held equal to the same table.
func TestBillingCatalogMatchesTheObservabilityContract(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../specs/001-metering-billing-core/contracts/metering-ports.md")
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile("(?m)^\\| `(billing_[a-z_]+)` \\| (counter|gauge|histogram)(?: \\(([^)]*)\\))? \\|")
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
		t.Fatal("found no billing metrics in the contract: the table format changed")
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

func TestTheMetricNamesTheIngressSuiteChecksAreTheCataloguedOnes(t *testing.T) {
	t.Parallel()
	for _, name := range []string{ports.MetricWebhookReceived, ports.MetricWebhookVerifyFailed, ports.MetricWebhookInboxAge} {
		found := false
		for _, c := range ports.MetricCatalog {
			found = found || c.Name == name
		}
		if !found {
			t.Errorf("%s is not in the metric catalogue", name)
		}
	}
	if ports.MetricWebhookReceived != "billing_webhook_received_total" || ports.MetricWebhookVerifyFailed != "billing_webhook_verify_failed_total" {
		t.Fatal("the contracts package checks these names as literals: they must not drift apart")
	}
}
