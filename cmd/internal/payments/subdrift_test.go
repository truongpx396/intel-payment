package payments_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/truongpx396/intel-payment/billing/ports"
	"github.com/truongpx396/intel-payment/cmd/internal/payments"
)

// R110a: the work of `billing.subdrift.tick`. It sweeps the one account its payload names or, with none, every account; it says how many
// subscriptions it repaired, so a sweep that found everything agreeing counts as idle; and it reports what it did even when it also failed.

// sweeper is a processor that can also sweep subscriptions, and remembers what it was asked to sweep.
type sweeper struct {
	*processor
	mu    sync.Mutex
	asked []string
	rep   ports.DriftReport
	err   error
}

func (s *sweeper) SweepSubscriptions(_ context.Context, account string) (ports.DriftReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, account)
	return s.rep, s.err
}

func (s *sweeper) SweepAllSubscriptions(context.Context) (ports.DriftReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, "*")
	return s.rep, s.err
}

func driftLoops(t *testing.T, sw ports.WebhookProcessor) (*payments.Loops, *rig) {
	t.Helper()
	r := newRig()
	r.deps.Processor = sw
	l, err := payments.NewLoops(r.deps)
	if err != nil {
		t.Fatal(err)
	}
	return l, r
}

func TestADriftTickSweepsTheAccountItNamesOrEveryAccountAndCountsWhatItRepaired(t *testing.T) {
	t.Parallel()
	sw := &sweeper{processor: &processor{}, rep: ports.DriftReport{Accounts: 2, Checked: 9, Repaired: 2, Unchanged: 7}}
	l, r := driftLoops(t, sw)
	for _, account := range []string{"", "acct", "acct-b"} {
		if n, err := l.Subdrift(t.Context(), account); err != nil || n != 2 {
			t.Fatalf("%q: repaired %d, err %v, want 2 and none", account, n, err)
		}
	}
	if got := strings.Join(sw.asked, ","); got != "*,acct,acct-b" {
		t.Fatalf("swept %s, want everything, then acct, then acct-b", got)
	}
	if out := r.logs.String(); !strings.Contains(out, "the subscription drift sweep") || !strings.Contains(out, "repaired=2") || !strings.Contains(out, "checked=9") {
		t.Fatalf("a sweep that repaired something said nothing: %q", out)
	}
}

func TestADriftTickThatFoundEverythingInAgreementIsIdleAndQuiet(t *testing.T) {
	t.Parallel()
	sw := &sweeper{processor: &processor{}, rep: ports.DriftReport{Accounts: 1, Checked: 40, Unchanged: 40}}
	l, r := driftLoops(t, sw)
	if n, err := l.Subdrift(t.Context(), ""); err != nil || n != 0 {
		t.Fatalf("repaired %d, err %v, want an idle tick", n, err)
	}
	if out := r.logs.String(); out != "" {
		t.Fatalf("a sweep that changed nothing logged: %q", out)
	}
}

func TestADriftTickThatFailedReturnsTheFailureWithWhatItRepairedAndSaysSo(t *testing.T) {
	t.Parallel()
	boom := errors.New("subscription sub-9: the provider said no")
	sw := &sweeper{processor: &processor{}, rep: ports.DriftReport{Accounts: 1, Checked: 3, Repaired: 1, Failed: 1}, err: boom}
	l, r := driftLoops(t, sw)
	n, err := l.Subdrift(t.Context(), "acct")
	if !errors.Is(err, boom) || n != 1 {
		t.Fatalf("repaired %d, err %v, want 1 and the failure", n, err)
	}
	if out := r.logs.String(); !strings.Contains(out, "failed=1") {
		t.Fatalf("a failing sweep did not say what it did: %q", out)
	}
	sw.rep = ports.DriftReport{}
	if _, err := l.Subdrift(t.Context(), ""); !errors.Is(err, boom) {
		t.Fatalf("a failure that repaired nothing was swallowed: %v", err)
	}
}

func TestAProcessorThatCannotSweepSubscriptionsIsAnErrorAndNotAnIdleTick(t *testing.T) {
	t.Parallel()
	l, _ := driftLoops(t, &processor{})
	if n, err := l.Subdrift(t.Context(), ""); err == nil || n != 0 {
		t.Fatalf("repaired %d, err %v, want an error: a worker that cannot sweep must not report that it did", n, err)
	}
}
