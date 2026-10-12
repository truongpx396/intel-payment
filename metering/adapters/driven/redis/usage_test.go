//go:build integration

package redis_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering"
	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// What a credits panel draws: how much of each ceiling a scope (and a member) has used. It is the read Admit
// makes of the window counters, without the verdict — and it changes nothing.
func TestUsageReadsEveryWindowCounterWithoutChangingAnything(t *testing.T) {
	t.Parallel()
	limits := []domain.Limit{
		{Name: "scope_balance", Unit: domain.CreditUnit, Window: domain.Balance, DenyCode: domain.DenyPaymentRequired},
		{Name: "user_cap", Subject: "user", Unit: domain.CreditUnit, Max: 100_000, Window: domain.Rolling, Dur: time.Hour, DenyCode: domain.DenyLimitReached},
		{Name: "org_daily", Unit: domain.CreditUnit, Max: 1_000_000, Window: domain.Daily, DenyCode: domain.DenyLimitReached},
		{Name: "api_hourly", Unit: "api_call", Max: 100, Window: domain.Hourly, DenyCode: domain.DenyLimitReached},
		{Name: "chat_daily", Resource: "llm.chat", Unit: domain.CreditUnit, Max: 1_000_000, Window: domain.Daily, DenyCode: domain.DenyLimitReached},
		{Name: "run_cap", Subject: "job", Unit: domain.CreditUnit, Max: 5000, Window: domain.Job, Dur: time.Hour, DenyCode: domain.DenyLimitReached},
	}
	for mode := range servers {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			rdb := servers[mode].Client()
			defer func() { _ = rdb.Close() }()
			cfg := metering.Config{}.WithDefaults()
			st := newStore(t, rdb, contracts.NewFakeClock(epoch), cfg, contracts.LedgerOptions{Limits: limits})
			h := healing{Store: st, cfg: cfg}
			s := domain.Scope{Realm: "usage", Kind: "workspace", ID: fmt.Sprintf("w-%d", time.Now().UnixNano())}

			if _, err := h.Grant(ctx, domain.Grant{Scope: s, Amount: 1_000_000, IdemKey: "seed", Reason: "purchase"}); err != nil {
				t.Fatal(err)
			}
			charge := func(key string, amount domain.Credits, resource string, subj domain.Subjects, qs ...domain.Quantity) {
				t.Helper()
				if _, err := h.Debit(ctx, domain.Charge{Scope: s, Amount: amount, IdemKey: key, Reason: "query", Resource: resource, Subjects: subj, Quantities: qs}); err != nil {
					t.Fatal(err)
				}
			}
			charge("c1", 300, "llm.chat", domain.Subjects{"user": "u1", "job": "r1"}, domain.Quantity{Unit: "api_call", Amount: 2})
			charge("c2", 500, "llm.chat", domain.Subjects{"user": "u2"}, domain.Quantity{Unit: "api_call", Amount: 1})
			charge("c3", 70, "gpu.job", domain.Subjects{"user": "u1"})

			beforeBal, _ := h.Balance(ctx, s)
			beforeAcct, _ := st.Snapshot(ctx, s)
			u1, err := st.Usage(ctx, s, domain.Subjects{"user": "u1", "job": "r1"}, limits)
			if err != nil {
				t.Fatal(err)
			}
			for name, want := range map[string]int64{
				"user_cap":   370, // u1's own spend: 300 + 70
				"org_daily":  870, // the scope's: 300 + 500 + 70
				"api_hourly": 3,   // a metered unit sums quantities, not credits: 2 + 1
				"chat_daily": 800, // only the llm.chat charges count toward a resource-scoped limit
				"run_cap":    300, // job r1's budget
			} {
				if u1[name] != want {
					t.Fatalf("%s = %d, want %d (all: %v)", name, u1[name], want, u1)
				}
			}
			if _, has := u1["scope_balance"]; has {
				t.Fatalf("a balance has no counter: %v", u1)
			}
			u2, err := st.Usage(ctx, s, domain.Subjects{"user": "u2", "job": "r2"}, limits)
			if err != nil || u2["user_cap"] != 500 || u2["org_daily"] != 870 || u2["run_cap"] != 0 {
				t.Fatalf("a member has a counter of their own: %v %v", u2, err)
			}
			nobody, err := st.Usage(ctx, s, domain.Subjects{"user": "u9", "job": "r9"}, limits)
			if err != nil || nobody["user_cap"] != 0 || nobody["org_daily"] != 870 {
				t.Fatalf("%v %v", nobody, err)
			}

			// Reading changes nothing: not the balance, not the sequence.
			afterBal, _ := h.Balance(ctx, s)
			afterAcct, _ := st.Snapshot(ctx, s)
			if afterAcct.Seq != beforeAcct.Seq || len(afterBal) != len(beforeBal) {
				t.Fatalf("a usage read moved the account: %+v → %+v", beforeAcct, afterAcct)
			}
		})
	}
}

func TestUsageRefusesWhatItCannotHonestlyAnswer(t *testing.T) {
	t.Parallel()
	limits := []domain.Limit{{Name: "user_cap", Subject: "user", Unit: domain.CreditUnit, Max: 100, Window: domain.Daily, DenyCode: domain.DenyLimitReached}}
	for mode := range servers {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			rdb := servers[mode].Client()
			defer func() { _ = rdb.Close() }()
			cfg := metering.Config{}.WithDefaults()
			st := newStore(t, rdb, contracts.NewFakeClock(epoch), cfg, contracts.LedgerOptions{Limits: limits})
			h := healing{Store: st, cfg: cfg}
			s := domain.Scope{Realm: "usage", Kind: "workspace", ID: fmt.Sprintf("r-%d", time.Now().UnixNano())}
			if _, err := h.Grant(ctx, domain.Grant{Scope: s, Amount: 10, IdemKey: "seed", Reason: "purchase"}); err != nil {
				t.Fatal(err)
			}
			// A member's counter without saying which member: zero would be a guess.
			if _, err := st.Usage(ctx, s, nil, limits); !errors.Is(err, domain.ErrMissingSubject) {
				t.Fatalf("want ErrMissingSubject, got %v", err)
			}
			// A scope that was never built is cold: the caller rehydrates it and asks again.
			cold := domain.Scope{Realm: "usage", Kind: "workspace", ID: "never-built-" + fmt.Sprint(time.Now().UnixNano())}
			if err := h.ensure(ctx, cold); err != nil {
				t.Fatal(err)
			}
			if _, err := st.Usage(ctx, cold, domain.Subjects{"user": "u1"}, limits); !errors.Is(err, domain.ErrColdScope) {
				t.Fatalf("want ErrColdScope, got %v", err)
			}
			// A malformed scope is invalid, before any I/O.
			if _, err := st.Usage(ctx, domain.Scope{Realm: "usage", Kind: "Bad Kind", ID: "x"}, domain.Subjects{"user": "u1"}, limits); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
		})
	}
}
