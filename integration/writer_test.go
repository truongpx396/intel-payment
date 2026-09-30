//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	pgadapter "github.com/truongpx396/intel-payment/metering/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// harness adapts a wstack to the shape the contracts drive.
func harness(t *testing.T, o contracts.LedgerOptions) contracts.LedgerHarness {
	t.Helper()
	s := newWriterStack(t, wopts{LedgerOptions: o})
	return s.ledgerHarness()
}

func (s *wstack) ledgerHarness() contracts.LedgerHarness {
	return contracts.LedgerHarness{
		Ledger: ledger{s},
		Clock:  s.clock,
		Drain:  func(t *testing.T) { t.Helper(); s.drain() },
		Booked: func(t *testing.T, sc domain.Scope, p domain.Pool) domain.Credits { t.Helper(); return s.booked(sc, p) },
		HasRow: func(t *testing.T, sc domain.Scope, op string, d domain.Credits) bool {
			t.Helper()
			return s.hasRow(sc, op, d)
		},
		InTransit: func(t *testing.T, r domain.Realm) domain.Credits { t.Helper(); return s.inTransit(r) },
		LedgerSum: func(t *testing.T, r domain.Realm) domain.Credits { t.Helper(); return s.ledgerSum(r) },
	}
}

// The hot-tier suite again, this time with the writer wired: its book-half subtests run instead of
// skipping, and the books must agree with the hot tier (invariants 12 and 14).
func TestLedgerContractWithTheWriter(t *testing.T) {
	t.Parallel()
	contracts.LedgerContract(t, harness)
}

func (s *wstack) workspace(id string) domain.Scope {
	return domain.Scope{Realm: "t1", Kind: "workspace", ID: fmt.Sprintf("%s-%d", id, seq.Add(1))}
}

// FR-017e: a usage charge journaled before the hot tier had it is settled by the very transaction
// that books its intent — the two can never disagree about whether it was billed.
func TestBookingSettlesTheJournalEntryInTheSameTransaction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newWriterStack(t, wopts{})
	l := ledger{s}
	sc := s.workspace("j")
	_, err := l.Grant(ctx, domain.Grant{Scope: sc, Amount: 1000, IdemKey: "g1", Reason: "purchase"})
	must(t, err)

	c := domain.Charge{Scope: sc, Amount: 100, IdemKey: "journaled", Reason: "query", Resource: "llm.chat", RateKey: "gpt",
		OccurredAt: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}
	journal := pgadapter.NewJournal(s.rawPool)
	created, err := journal.Put(ctx, ports.JournalEntry{Charge: c, Fingerprint: c.Fingerprint()})
	must(t, err)
	if !created {
		t.Fatal("the entry is new")
	}
	if n, _ := journal.Pending(ctx, "t1"); n != 1 {
		t.Fatalf("pending %d", n)
	}
	_, err = l.Debit(ctx, c) // what the replay tick does through the hot function
	must(t, err)

	s.drain()
	if n, _ := journal.Pending(ctx, "t1"); n != 0 {
		t.Fatalf("booking the intent must settle the journal entry: %d still pending", n)
	}
	if s.booked(sc, domain.GeneralPool) != 900 {
		t.Fatalf("booked %d", s.booked(sc, domain.GeneralPool))
	}
}

type watches []ports.BalanceWatch

func (w watches) Watches(context.Context, domain.Scope) ([]ports.BalanceWatch, error) { return w, nil }

// FR-007: the writer publishes billing.balance.low.<tag> for the mutation that crossed a watched
// threshold downward — once, however many times the intent is delivered.
func TestTheWriterPublishesBalanceLowOnceWhenAThresholdIsCrossed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newWriterStack(t, wopts{watches: watches{{Pool: domain.GeneralPool, Threshold: 500, Key: "auto_recharge"}}})
	l := ledger{s}
	sc := s.workspace("low")
	_, err := l.Grant(ctx, domain.Grant{Scope: sc, Amount: 1000, IdemKey: "g1", Reason: "purchase"})
	must(t, err)
	charge := func(key string) domain.Charge {
		return domain.Charge{Scope: sc, Amount: 300, IdemKey: key, Reason: "query", OccurredAt: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}
	}
	for _, k := range []string{"u1", "u2", "u3"} { // 700, 400 (crosses 500), 100
		_, err := l.Debit(ctx, charge(k))
		must(t, err)
	}
	s.drain()
	prefix := s.cfg.SubjectPrefix + ".balance.low."
	if n := s.bus.count(prefix); n != 1 {
		t.Fatalf("one crossing, one event: %d", n)
	}
	for _, in := range s.intents(sc) {
		s.inject(in) // a redelivery of every intent
	}
	s.drain()
	if n := s.bus.count(prefix); n != 1 {
		t.Fatalf("a redelivery must not notify again: %d", n)
	}
}
