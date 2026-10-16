package app_test

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/app"
	"github.com/truongpx396/intel-payment/billing/contracts"
	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
)

// R097a: PAYMENT_SUBJECT_PREFIX fits the service into a host's namespace, and the writer already publishes under it
// (`<prefix>.balance.low.<tag>`). The payment layer has to live in the same namespace: a prefix that changed only
// metering's subjects would have the writer announce a low balance on a subject auto top-up never listens to — a
// customer left to run dry with a standing instruction to be topped up — and announce payments where no consumer looks.

const prefix = "acme"

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// onlyUnder fails if anything was published outside the prefix.
func onlyUnder(t *testing.T, h *contracts.Harness, want string) {
	t.Helper()
	for _, m := range h.Bus.Messages() {
		if !strings.HasPrefix(m.Subject, want+".") {
			t.Errorf("published on %q: outside the configured prefix %q", m.Subject, want)
		}
	}
}

func prefixedProcessor(t *testing.T, h *contracts.Harness, p string) ports.WebhookProcessor {
	t.Helper()
	proc, err := app.NewProcessor(app.ProcessorDeps{
		Inbox: h.Inbox, Accounts: h.Accounts, Providers: h.Providers(), Customers: h.Customers, Catalog: h.Catalog,
		Payments: h.Payments, Subs: h.Subs, Meter: h.Meter, Lots: h.Lots, Clock: h.Clock, Metrics: h.Metrics, Bus: h.Bus, Recharges: h.Recharges,
		Log: quiet(), Grace: contracts.GraceWindow, SubjectPrefix: p,
	})
	if err != nil {
		t.Fatal(err)
	}
	return proc
}

func TestAPaymentIsAnnouncedUnderTheConfiguredPrefix(t *testing.T) {
	t.Parallel()
	h := contracts.NewHarness()
	p := prefixedProcessor(t, h, prefix)
	h.Persisted(h.PaymentSucceeded("evt_1", "pay_1"))
	h.Tick(t, p)

	if got := h.Bus.On(prefix + ".payment." + h.Scope.SubjectToken()); len(got) == 0 {
		t.Fatalf("the payment was not announced on %s.payment.<tag>; published: %v", prefix, h.Bus.Messages())
	}
	onlyUnder(t, h, prefix)
}

func TestAnEntitlementChangeIsAnnouncedUnderTheConfiguredPrefix(t *testing.T) {
	t.Parallel()
	h := contracts.NewHarness()
	p := prefixedProcessor(t, h, prefix)
	t1 := contracts.Start.Add(time.Hour)
	h.GivenProviderSub("sub-1", domain.Canceled, t1)
	h.Persisted(h.SubscriptionEvent("evt_a", "sub-1", domain.SubscriptionDeleted, t1))
	h.Tick(t, p)

	if got := h.Bus.On(prefix + ".entitlement." + h.Scope.SubjectToken()); len(got) == 0 {
		t.Fatalf("the change was not announced on %s.entitlement.<tag>; published: %v", prefix, h.Bus.Messages())
	}
	onlyUnder(t, h, prefix)
}

func TestWithNoPrefixTheSubjectsAreTheDocumentedBillingOnes(t *testing.T) {
	t.Parallel()
	h := contracts.NewHarness()
	p := prefixedProcessor(t, h, "")
	h.Persisted(h.PaymentSucceeded("evt_1", "pay_1"))
	h.Tick(t, p)
	if len(h.Bus.On("billing.payment."+h.Scope.SubjectToken())) == 0 {
		t.Fatalf("the default prefix is billing; published: %v", h.Bus.Messages())
	}
	onlyUnder(t, h, "billing")
}

func TestTheReceivedHintIsPublishedUnderTheConfiguredPrefix(t *testing.T) {
	t.Parallel()
	h := contracts.NewHarness()
	g, err := app.NewIngress(app.IngressDeps{
		Providers: h.Providers(), Accounts: h.Accounts, Inbox: h.Inbox, Metrics: h.Metrics, Clock: h.Clock, Bus: h.Bus,
		MaxBody: contracts.MaxWebhookBody, Log: quiet(), SubjectPrefix: prefix,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, hdr := h.Deliver(h.PaymentSucceeded("evt_1", "pay_1"))
	if res := g.Handle(contracts.Ctx(), string(contracts.ProviderName), contracts.AccountID, body, hdr); res.Status != 200 {
		t.Fatalf("the delivery was answered %+v", res)
	}
	if got := h.Bus.On(prefix + ".webhook.received"); len(got) != 1 {
		t.Fatalf("the hint was published %d times on %s.webhook.received; published: %v", len(got), prefix, h.Bus.Messages())
	}
	onlyUnder(t, h, prefix)
}

func TestAutoTopUpListensUnderTheConfiguredPrefixAndOnlyThere(t *testing.T) {
	t.Parallel()
	w := contracts.NewRechargeWorld()
	h, err := app.NewAutoRecharge(app.AutoRechargeDeps{
		Instructions: w.Recharges, Accounts: w.Accounts, Providers: w.Providers(), Catalog: w.Catalog, Customers: w.Customers,
		Payments: w.Payments, Balances: w.Balances, Bus: w.Bus, Metrics: w.Metrics, Clock: w.Clock, Log: quiet(), SubjectPrefix: prefix,
	})
	if err != nil {
		t.Fatal(err)
	}
	subject, payload := contracts.LowBalance(w.Scope, "general", 1000, 1)
	own := strings.Replace(subject, "billing.", prefix+".", 1)

	// The message the writer publishes under this host's prefix is acted on.
	if err := h.Handle(contracts.Ctx(), own, payload); err != nil {
		t.Fatalf("a low-balance message on %q was not acknowledged: %v", own, err)
	}
	if n := len(w.Provider.ChargeCalls()); n != 1 {
		t.Fatalf("the instruction charged %d times for a low balance published under its prefix, want 1", n)
	}
	// One under another namespace is not this deployment's, and is refused rather than acted on.
	if err := h.Handle(contracts.Ctx(), subject, payload); err == nil {
		t.Fatalf("a low-balance message on %q, outside the prefix %q, was acknowledged", subject, prefix)
	}
	if n := len(w.Provider.ChargeCalls()); n != 1 {
		t.Fatalf("a message from another namespace charged a customer: %d payments", n)
	}
}

func TestAutoTopUpTellsTheCustomerWhyItSwitchedOffUnderTheConfiguredPrefix(t *testing.T) {
	t.Parallel()
	w := contracts.NewRechargeWorld()
	h, err := app.NewAutoRecharge(app.AutoRechargeDeps{
		Instructions: w.Recharges, Accounts: w.Accounts, Providers: w.Providers(), Catalog: w.Catalog, Customers: w.Customers,
		Payments: w.Payments, Balances: w.Balances, Bus: w.Bus, Metrics: w.Metrics, Clock: w.Clock, Log: quiet(), SubjectPrefix: prefix,
	})
	if err != nil {
		t.Fatal(err)
	}
	w.Provider.DeclineCharges(true)
	subject, payload := contracts.LowBalance(w.Scope, "general", 1000, 1)
	own := strings.Replace(subject, "billing.", prefix+".", 1)
	for range 3 {
		if err := h.Handle(contracts.Ctx(), own, payload); err != nil {
			t.Fatalf("a refusal is known, and acknowledged: %v", err)
		}
	}
	if got := w.Bus.On(prefix + ".payment." + w.Scope.SubjectToken()); len(got) == 0 {
		t.Fatalf("the switch-off was not announced on %s.payment.<tag>; published: %v", prefix, w.Bus.Messages())
	}
	onlyUnder(t, w.Harness, prefix)
}

func TestWithNoPrefixTheReceivedHintIsOnBillingToo(t *testing.T) {
	t.Parallel()
	h := contracts.NewHarness()
	g, err := app.NewIngress(app.IngressDeps{
		Providers: h.Providers(), Accounts: h.Accounts, Inbox: h.Inbox, Metrics: h.Metrics, Clock: h.Clock, Bus: h.Bus,
		MaxBody: contracts.MaxWebhookBody, Log: quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}
	body, hdr := h.Deliver(h.PaymentSucceeded("evt_1", "pay_1"))
	if res := g.Handle(contracts.Ctx(), string(contracts.ProviderName), contracts.AccountID, body, hdr); res.Status != 200 {
		t.Fatalf("the delivery was answered %+v", res)
	}
	if got := h.Bus.On("billing.webhook.received"); len(got) != 1 {
		t.Fatalf("the hint was published %d times on billing.webhook.received; published: %v", len(got), h.Bus.Messages())
	}
}
