package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// The read behind GetBalance and GET /v1/balance. A transport has no business reading the hot tier
// itself: it would meet a cold account — a fresh Redis, a bumped generation — and report it as an error
// where Admit and Record rebuild it. So the engine offers the read, and it heals the same way.

func reader(t *testing.T, m ports.Meter) ports.BalanceReader {
	t.Helper()
	br, ok := m.(ports.BalanceReader)
	if !ok {
		t.Fatal("the Meter the engine returns is not a ports.BalanceReader: GetBalance and GET /v1/balance have nothing to read with")
	}
	return br
}

func TestBalanceReadsThePoolsOfAHotAccount(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{})
	r.bal.balance = func(domain.Scope) (map[domain.Pool]domain.Credits, error) {
		return map[domain.Pool]domain.Credits{domain.GeneralPool: 700, "promo": 30, "debt": -50}, nil
	}
	got, err := reader(t, r.m).Balance(context.Background(), ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[domain.GeneralPool] != 700 || got["promo"] != 30 || got["debt"] != -50 {
		t.Fatalf("every pool, exactly: %v", got)
	}
}

func TestBalanceRebuildsAColdAccountFromTheBooksInsteadOfReportingIt(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{})
	r.books.state = ports.BookedState{
		Watermark: ports.Watermark{Exists: true, AppliedSeq: 41},
		Booked:    map[domain.Pool]domain.Credits{domain.GeneralPool: 500},
	}
	cold := true
	r.bal.rehyd = func(domain.Scope, ports.RehydrateState) (ports.RehydrateOutcome, error) {
		cold = false
		return ports.RehydrateInstalled, nil
	}
	r.bal.balance = func(domain.Scope) (map[domain.Pool]domain.Credits, error) {
		if cold {
			return nil, domain.ErrColdScope
		}
		return map[domain.Pool]domain.Credits{domain.GeneralPool: 500}, nil
	}
	got, err := reader(t, r.m).Balance(context.Background(), ws)
	if err != nil {
		t.Fatalf("a cold account is rebuilt, not reported: %v", err)
	}
	if got[domain.GeneralPool] != 500 || len(r.bal.rehydOn) != 1 || r.bal.rehydOn[0].AppliedSeq != 41 {
		t.Fatalf("rebuilt once, from what the books say: %v, %+v", got, r.bal.rehydOn)
	}
}

func TestBalanceOfAnAccountThatStaysColdIsAnOutageNotAZero(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{})
	r.bal.balance = func(domain.Scope) (map[domain.Pool]domain.Credits, error) { return nil, domain.ErrColdScope }
	got, err := reader(t, r.m).Balance(context.Background(), ws)
	if !errors.Is(err, domain.ErrHotStoreUnavailable) {
		t.Fatalf("a balance the engine could not establish is not an empty one: %v, %v", got, err)
	}
}

func TestBalanceNeverGuessesWhenTheHotTierIsDown(t *testing.T) {
	t.Parallel()
	for name, cause := range map[string]error{
		"unreachable":  domain.ErrHotStoreUnavailable,
		"frozen":       domain.ErrShardFrozen,
		"cold shard":   domain.ErrColdShard,
		"backpressure": domain.ErrBackpressure,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, rigOpt{})
			r.bal.balance = func(domain.Scope) (map[domain.Pool]domain.Credits, error) { return nil, cause }
			got, err := reader(t, r.m).Balance(context.Background(), ws)
			if !errors.Is(err, cause) || got != nil {
				t.Fatalf("the refusal reaches the caller as itself, with no number beside it: %v, %v", got, err)
			}
			if len(r.bal.rehydOn) != 0 {
				t.Fatal("an outage is not a cold account: nothing is rebuilt")
			}
		})
	}
}

func TestBalanceRefusesAMalformedScopeBeforeTouchingTheHotTier(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{})
	_, err := reader(t, r.m).Balance(context.Background(), domain.Scope{Realm: "t1", Kind: "Bad Kind", ID: "x"})
	if !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
	if len(r.bal.balanced) != 0 {
		t.Fatal("the hot tier was read for a scope that cannot exist")
	}
}

func TestBalanceReadsTheDefaultRealmWhenNoneIsNamed(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{})
	r.bal.balance = func(domain.Scope) (map[domain.Pool]domain.Credits, error) { return nil, nil }
	if _, err := reader(t, r.m).Balance(context.Background(), domain.Scope{Kind: "workspace", ID: "w1"}); err != nil {
		t.Fatal(err)
	}
	if got := r.bal.balanced; len(got) != 1 || got[0].Realm != domain.DefaultRealm {
		t.Fatalf("the same scope Admit and Record would use: %+v", got)
	}
}
