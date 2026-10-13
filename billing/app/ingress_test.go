package app_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/app"
	"github.com/truongpx396/intel-payment/billing/contracts"
	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// ingressUnderTest builds the ingress over a harness's collaborators.
var ingressUnderTest contracts.IngressFactory = func(t testing.TB, h *contracts.Harness) contracts.Ingress {
	t.Helper()
	g, err := app.NewIngress(app.IngressDeps{
		Providers: h.Providers(), Accounts: h.Accounts, Inbox: h.Inbox, Metrics: h.Metrics, Clock: h.Clock,
		MaxBody: contracts.MaxWebhookBody, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestIngressContract(t *testing.T) {
	t.Parallel()
	contracts.IngressContract(t, ingressUnderTest)
}

// Stage 1 is held to the contract by showing the contract rejects an ingress that breaks it: each variant is the
// ingress under test with ONE defect, and each must be rejected by a subtest that names the invariant.
func TestIngressContractRejectsEveryBrokenIngressByName(t *testing.T) {
	t.Parallel()
	const (
		verifiesFirst = "verifies before anything"
		forged        = "a forged delivery is 400, stores nothing and is counted as a security event"
		persists      = "persists before acknowledging"
		replay        = "a replay answers 200 and persists once"
		noMoney       = "ingress never touches money"
		unknown       = "an unknown, inactive or foreign account is 404 and nothing is verified"
		parks         = "unavailable verification parks the raw body and answers 202"
		rejects       = "a parked delivery that fails verification is rejected, never processed"
		oversized     = "an oversized body is refused before it is verified"
	)
	broken := []struct {
		name     string
		build    contracts.IngressFactory
		caughtBy string
	}{
		{"touches the inbox before verifying", func(t testing.TB, h *contracts.Harness) contracts.Ingress {
			return preTouch{ingressUnderTest(t, h), h}
		}, verifiesFirst},
		{"grants credits at the door", func(t testing.TB, h *contracts.Harness) contracts.Ingress {
			return grantsAtTheDoor{ingressUnderTest(t, h), h}
		}, noMoney},
		{"serves any account name as the real one", func(t testing.TB, h *contracts.Harness) contracts.Ingress {
			return anyAccount{ingressUnderTest(t, h)}
		}, unknown},
		{"trusts a provider that skips verification", func(t testing.TB, h *contracts.Harness) contracts.Ingress {
			h.Provider = h.Provider.WithDefect(contracts.DefectSkipVerification)
			return ingressUnderTest(t, h)
		}, forged},
		{"acknowledges a persist that did not happen", func(t testing.TB, h *contracts.Harness) contracts.Ingress {
			h.Inbox.LiePersist = true
			return ingressUnderTest(t, h)
		}, persists},
		{"fails a replay", func(t testing.TB, h *contracts.Harness) contracts.Ingress {
			h.Inbox.ErrOnDuplicate = true
			return ingressUnderTest(t, h)
		}, replay},
		{"loses the parked body", func(t testing.TB, h *contracts.Harness) contracts.Ingress {
			h.Inbox.DropParkedBody = true
			return ingressUnderTest(t, h)
		}, parks},
		{"promotes a parked delivery without verifying it", func(t testing.TB, h *contracts.Harness) contracts.Ingress {
			return promotesUnverified{ingressUnderTest(t, h), h}
		}, rejects},
	}
	for _, b := range broken {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			var rejected []string
			for _, c := range contracts.IngressCases() {
				if failed, _ := contracts.Probe(20*time.Second, func(t testing.TB) { c.Run(t, b.build) }); failed {
					rejected = append(rejected, c.Name)
				}
			}
			if !slices.Contains(rejected, b.caughtBy) {
				t.Fatalf("the suite PASSED an ingress that %s on %q (rejected only by %q)", b.name, b.caughtBy, rejected)
			}
		})
	}
}

func TestIngressContractPassesEveryCaseOnTheIngressUnderTest(t *testing.T) {
	t.Parallel()
	for _, c := range contracts.IngressCases() {
		if failed, said := contracts.Probe(20*time.Second, func(t testing.TB) { c.Run(t, ingressUnderTest) }); failed {
			t.Errorf("%q: %v", c.Name, said)
		}
	}
}

// ---- the defects, as wrappers around the ingress under test ----

type preTouch struct {
	contracts.Ingress
	h *contracts.Harness
}

func (p preTouch) Handle(ctx context.Context, provider, account string, body []byte, hdr http.Header) ports.IngressResult {
	_ = p.h.Inbox.Park(ctx, domain.Provider(provider), account, "touch", body, hdr) // a store write before the signature is checked
	return p.Ingress.Handle(ctx, provider, account, body, hdr)
}

type grantsAtTheDoor struct {
	contracts.Ingress
	h *contracts.Harness
}

func (g grantsAtTheDoor) Handle(ctx context.Context, provider, account string, body []byte, hdr http.Header) ports.IngressResult {
	res := g.Ingress.Handle(ctx, provider, account, body, hdr)
	if res.Status == 200 {
		_, _ = g.h.Meter.Grant(ctx, md.Grant{Scope: g.h.Scope, Amount: 1, IdemKey: "door-" + account, Reason: "purchase"})
	}
	return res
}

type anyAccount struct{ contracts.Ingress }

func (a anyAccount) Handle(ctx context.Context, _, _ string, body []byte, hdr http.Header) ports.IngressResult {
	return a.Ingress.Handle(ctx, "fake", contracts.AccountID, body, hdr)
}

type promotesUnverified struct {
	contracts.Ingress
	h *contracts.Harness
}

func (p promotesUnverified) Retry(ctx context.Context) error {
	lax := p.h.Provider.WithDefect(contracts.DefectSkipVerification)
	ds, err := p.h.Inbox.LeaseParked(ctx, 100, time.Minute)
	if err != nil {
		return err
	}
	for _, d := range ds {
		acct, _, _ := p.h.Accounts.Account(ctx, d.Account)
		e, err := lax.VerifyWebhook(ctx, acct, d.Body, d.Header)
		if err != nil {
			return err
		}
		if _, err := p.h.Inbox.Promote(ctx, d.ID, e); err != nil {
			return err
		}
	}
	return nil
}
