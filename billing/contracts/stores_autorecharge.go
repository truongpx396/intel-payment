package contracts

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// AutoRechargesFactory builds the standing-instruction store under test, on a world that has the harness's provider accounts.
type AutoRechargesFactory func(t testing.TB) ports.AutoRecharges

const rechargeStale = 10 * time.Minute

func rechargeScope(n int) md.Scope {
	return md.Scope{Realm: md.DefaultRealm, Kind: "workspace", ID: fmt.Sprintf("r%d", n)}
}

// rechargeFor is a valid, enabled instruction for the nth scope: three charges a day of the pack, below 1000 credits.
func rechargeFor(n int) domain.AutoRecharge {
	return domain.AutoRecharge{Scope: rechargeScope(n), Enabled: true, Pool: md.GeneralPool, Threshold: 1000, PlanCode: PackCode, Currency: "USD",
		Account: AccountID, PaymentMethodRef: "pm_1", MaxPerDay: 3}
}

func put(t testing.TB, s ports.AutoRecharges, a domain.AutoRecharge) {
	t.Helper()
	if err := s.Put(Ctx(), a); err != nil {
		t.Fatalf("Put(%s): %v", a.Scope.Tag(), err)
	}
}

func getRecharge(t testing.TB, s ports.AutoRecharges, sc md.Scope) domain.AutoRecharge {
	t.Helper()
	a, ok, err := s.Get(Ctx(), sc)
	if err != nil || !ok {
		t.Fatalf("Get(%s) = %v, %v", sc.Tag(), ok, err)
	}
	return a
}

func claim(t testing.TB, s ports.AutoRecharges, sc md.Scope, at time.Time) (ports.RechargeClaim, ports.RechargeRefusal) {
	t.Helper()
	c, refusal, err := s.Claim(Ctx(), sc, at, rechargeStale)
	if err != nil {
		t.Fatalf("Claim(%s): %v", sc.Tag(), err)
	}
	return c, refusal
}

func settle(t testing.TB, s ports.AutoRecharges, sc md.Scope, ok bool, reason string, at time.Time) bool {
	t.Helper()
	disabled, err := s.Settle(Ctx(), sc, ok, reason, at)
	if err != nil {
		t.Fatalf("Settle(%s): %v", sc.Tag(), err)
	}
	return disabled
}

var rechargeDay = time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)

// AutoRechargeCases are the subtests of AutoRechargesContract (payments invariant 18).
func AutoRechargeCases() []StoreCase[AutoRechargesFactory] {
	return []StoreCase[AutoRechargesFactory]{
		{"an instruction round-trips its configuration, and an unknown scope has none", func(t testing.TB, mk AutoRechargesFactory) {
			s := mk(t)
			if _, ok, err := s.Get(Ctx(), rechargeScope(1)); ok || err != nil {
				t.Fatalf("an unknown scope: %v, %v", ok, err)
			}
			want := rechargeFor(1)
			want.Pool, want.Threshold, want.MaxPerDay, want.PaymentMethodRef = "promo", 2500, 7, "pm_other"
			put(t, s, want)
			got := getRecharge(t, s, rechargeScope(1))
			if got.Scope != want.Scope || !got.Enabled || got.Pool != "promo" || got.Threshold != 2500 || got.PlanCode != PackCode || got.Currency != "USD" ||
				got.Account != AccountID || got.PaymentMethodRef != "pm_other" || got.MaxPerDay != 7 {
				t.Fatalf("instruction = %+v, want %+v", got, want)
			}
			if got.ChargesToday != 0 || got.InflightN != 0 || got.ConsecutiveFailed != 0 || got.Day != "" {
				t.Fatalf("a new instruction starts with an empty ledger: %+v", got)
			}
		}},
		{"an instruction that could not charge safely is refused and not stored", func(t testing.TB, mk AutoRechargesFactory) {
			s := mk(t)
			for name, mutate := range map[string]func(*domain.AutoRecharge){
				"no charges a day":       func(a *domain.AutoRecharge) { a.MaxPerDay = 0 },
				"too many charges":       func(a *domain.AutoRecharge) { a.MaxPerDay = 51 },
				"a threshold of nothing": func(a *domain.AutoRecharge) { a.Threshold = 0 },
				"no saved method":        func(a *domain.AutoRecharge) { a.PaymentMethodRef = "" },
				"no scope":               func(a *domain.AutoRecharge) { a.Scope = md.Scope{} },
			} {
				a := rechargeFor(1)
				mutate(&a)
				if err := s.Put(Ctx(), a); !errors.Is(err, md.ErrInvalid) {
					t.Errorf("%s: err = %v, want ErrInvalid", name, err)
				}
			}
			if _, ok, _ := s.Get(Ctx(), rechargeScope(1)); ok {
				t.Fatal("a refused instruction was stored")
			}
		}},
		{"claiming numbers the day's slots, a duplicate resumes, and the day is bounded by max_per_day", func(t testing.TB, mk AutoRechargesFactory) {
			s := mk(t)
			sc := rechargeScope(1)
			put(t, s, rechargeFor(1))
			first, refusal := claim(t, s, sc, rechargeDay)
			if refusal != ports.RechargeOK || first != (ports.RechargeClaim{Day: "2026-03-01", N: 1}) {
				t.Fatalf("first claim = %+v %q", first, refusal)
			}
			for range 3 {
				if again, refusal := claim(t, s, sc, rechargeDay.Add(time.Minute)); refusal != ports.RechargeOK || again != (ports.RechargeClaim{Day: "2026-03-01", N: 1, Resumed: true}) {
					t.Fatalf("a duplicate trigger = %+v %q, want slot 1 again: two keys would charge twice (invariant 18)", again, refusal)
				}
			}
			for n := 1; n <= 3; n++ {
				if n > 1 {
					c, refusal := claim(t, s, sc, rechargeDay.Add(time.Duration(n)*time.Minute))
					if refusal != ports.RechargeOK || c.N != n || c.Resumed {
						t.Fatalf("claim %d = %+v %q", n, c, refusal)
					}
				}
				settle(t, s, sc, true, "", rechargeDay)
			}
			if c, refusal := claim(t, s, sc, rechargeDay.Add(time.Hour)); refusal != ports.RechargeMaxPerDay || c != (ports.RechargeClaim{}) {
				t.Fatalf("a fourth claim = %+v %q, want max_per_day", c, refusal)
			}
		}},
		{"an unknown scope or a disabled instruction claims nothing", func(t testing.TB, mk AutoRechargesFactory) {
			s := mk(t)
			if c, refusal := claim(t, s, rechargeScope(1), rechargeDay); refusal != ports.RechargeDisabled || c != (ports.RechargeClaim{}) {
				t.Fatalf("an unknown scope: %+v %q", c, refusal)
			}
			off := rechargeFor(2)
			off.Enabled, off.DisabledReason = false, "paused by the customer"
			put(t, s, off)
			if c, refusal := claim(t, s, rechargeScope(2), rechargeDay); refusal != ports.RechargeDisabled || c != (ports.RechargeClaim{}) {
				t.Fatalf("a disabled instruction: %+v %q", c, refusal)
			}
			if got := getRecharge(t, s, rechargeScope(2)); got.Enabled || got.DisabledReason != "paused by the customer" || got.ChargesToday != 0 {
				t.Fatalf("a refused claim changed the instruction: %+v", got)
			}
		}},
		{"the ledger is stored: what a claim did is what the next read says", func(t testing.TB, mk AutoRechargesFactory) {
			s := mk(t)
			sc := rechargeScope(1)
			put(t, s, rechargeFor(1))
			claim(t, s, sc, rechargeDay)
			got := getRecharge(t, s, sc)
			if got.Day != "2026-03-01" || got.ChargesToday != 1 || got.InflightN != 1 || got.InflightSince == nil || !got.InflightSince.Equal(rechargeDay) ||
				got.LastTriggeredAt == nil || !got.LastTriggeredAt.Equal(rechargeDay) {
				t.Fatalf("instruction = %+v", got)
			}
			settle(t, s, sc, true, "", rechargeDay.Add(time.Minute))
			if got = getRecharge(t, s, sc); got.InflightN != 0 || got.InflightSince != nil || got.ChargesToday != 1 {
				t.Fatalf("a settled slot is no longer in flight, and still spent: %+v", got)
			}
		}},
		{"concurrent triggers for one scope claim one slot", func(t testing.TB, mk AutoRechargesFactory) {
			s := mk(t)
			sc := rechargeScope(1)
			put(t, s, rechargeFor(1))
			const triggers = 32
			var wg sync.WaitGroup
			claims := make([]ports.RechargeClaim, triggers)
			for i := range triggers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					c, refusal, err := s.Claim(Ctx(), sc, rechargeDay, rechargeStale)
					if err != nil || refusal != ports.RechargeOK {
						t.Errorf("trigger %d: %v %q", i, err, refusal)
					}
					claims[i] = c
				}()
			}
			wg.Wait()
			fresh := 0
			for _, c := range claims {
				if c.N != 1 {
					t.Fatalf("a trigger got slot %d: all of them are the first slot", c.N)
				}
				if !c.Resumed {
					fresh++
				}
			}
			if fresh != 1 {
				t.Fatalf("%d triggers took a slot of their own: the provider would be charged %d times (invariant 18)", fresh, fresh)
			}
			if got := getRecharge(t, s, sc); got.ChargesToday != 1 {
				t.Fatalf("ChargesToday = %d", got.ChargesToday)
			}
		}},
		{"however many triggers fire at once, a day never hands out more than max_per_day slots", func(t testing.TB, mk AutoRechargesFactory) {
			s := mk(t)
			sc := rechargeScope(1)
			put(t, s, rechargeFor(1)) // three a day
			var mu sync.Mutex
			slots := map[int]int{}
			var wg sync.WaitGroup
			for i := range 24 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for range 6 {
						c, refusal, err := s.Claim(Ctx(), sc, rechargeDay.Add(time.Duration(i)*time.Second), rechargeStale)
						if err != nil {
							t.Errorf("claim: %v", err)
							return
						}
						if refusal != ports.RechargeOK {
							continue
						}
						if !c.Resumed {
							mu.Lock()
							slots[c.N]++
							mu.Unlock()
							if _, err := s.Settle(Ctx(), sc, true, "", rechargeDay); err != nil {
								t.Errorf("settle: %v", err)
							}
						}
					}
				}()
			}
			wg.Wait()
			var got []int
			for n, times := range slots {
				if times != 1 {
					t.Errorf("slot %d was handed out %d times", n, times)
				}
				got = append(got, n)
			}
			sort.Ints(got)
			if fmt.Sprint(got) != "[1 2 3]" {
				t.Fatalf("slots handed out = %v, want exactly [1 2 3]: more is a fourth charge in a day of three", got)
			}
		}},
		{"settling clears the slot, counts failures, switches off on the third and says so once", func(t testing.TB, mk AutoRechargesFactory) {
			s := mk(t)
			sc := rechargeScope(1)
			a := rechargeFor(1)
			a.MaxPerDay = 50
			put(t, s, a)
			for i := 1; i <= 3; i++ {
				claim(t, s, sc, rechargeDay.Add(time.Duration(i)*time.Minute))
				disabled := settle(t, s, sc, false, "card_declined", rechargeDay)
				if want := i == 3; disabled != want {
					t.Fatalf("failure %d: disabled = %v, want %v", i, disabled, want)
				}
				if got := getRecharge(t, s, sc); got.ConsecutiveFailed != i || got.InflightN != 0 {
					t.Fatalf("failure %d: %+v", i, got)
				}
			}
			got := getRecharge(t, s, sc)
			if got.Enabled || got.DisabledReason != "card_declined" {
				t.Fatalf("after three failures: %+v", got)
			}
			if _, refusal := claim(t, s, sc, rechargeDay.Add(time.Hour)); refusal != ports.RechargeDisabled {
				t.Fatalf("a switched-off instruction claimed a slot: %q", refusal)
			}
			if again := settle(t, s, sc, false, "expired_card", rechargeDay); again {
				t.Fatal("a late failure announced the switching-off a second time")
			}
			if got = getRecharge(t, s, sc); got.DisabledReason != "card_declined" || got.Enabled {
				t.Fatalf("a late failure changed why it was switched off, or switched it on: %+v", got)
			}
			// A success between failures starts the count again.
			b := rechargeFor(2)
			b.MaxPerDay = 50
			put(t, s, b)
			for _, ok := range []bool{false, false, true, false, false} {
				claim(t, s, rechargeScope(2), rechargeDay)
				settle(t, s, rechargeScope(2), ok, "card_declined", rechargeDay)
			}
			if got = getRecharge(t, s, rechargeScope(2)); !got.Enabled || got.ConsecutiveFailed != 2 {
				t.Fatalf("two failures, a success and two failures: %+v", got)
			}
		}},
		{"a new UTC day starts again from slot one", func(t testing.TB, mk AutoRechargesFactory) {
			s := mk(t)
			sc := rechargeScope(1)
			a := rechargeFor(1)
			a.MaxPerDay = 1
			put(t, s, a)
			claim(t, s, sc, rechargeDay)
			settle(t, s, sc, true, "", rechargeDay)
			if _, refusal := claim(t, s, sc, rechargeDay.Add(time.Hour)); refusal != ports.RechargeMaxPerDay {
				t.Fatalf("the day's one charge was spent: %q", refusal)
			}
			if c, refusal := claim(t, s, sc, time.Date(2026, 3, 2, 0, 0, 1, 0, time.UTC)); refusal != ports.RechargeOK || c != (ports.RechargeClaim{Day: "2026-03-02", N: 1}) {
				t.Fatalf("the next day = %+v %q", c, refusal)
			}
		}},
		{"editing the instruction never refills the day, and the owner enabling it clears the failures", func(t testing.TB, mk AutoRechargesFactory) {
			s := mk(t)
			sc := rechargeScope(1)
			a := rechargeFor(1)
			a.MaxPerDay = 2
			put(t, s, a)
			for range 2 {
				claim(t, s, sc, rechargeDay)
				settle(t, s, sc, false, "card_declined", rechargeDay)
			}
			// The customer changes the card and the threshold: the two charges already made today still count.
			a.PaymentMethodRef, a.Threshold = "pm_new", 5000
			put(t, s, a)
			got := getRecharge(t, s, sc)
			if got.PaymentMethodRef != "pm_new" || got.Threshold != 5000 {
				t.Fatalf("the edit was not stored: %+v", got)
			}
			if got.ChargesToday != 2 || got.Day != "2026-03-01" {
				t.Fatalf("editing the instruction changed the day's ledger: %+v — an edit must not hand out more charges", got)
			}
			if _, refusal := claim(t, s, sc, rechargeDay.Add(time.Hour)); refusal != ports.RechargeMaxPerDay {
				t.Fatalf("an edit refilled the day: %q", refusal)
			}
			if got.ConsecutiveFailed != 0 {
				t.Fatalf("enabling an instruction (its owner has fixed the card) clears the failures: %+v", got)
			}
		}},
		{"an instruction the owner switches off is off, with the reason, until the owner switches it on", func(t testing.TB, mk AutoRechargesFactory) {
			s := mk(t)
			sc := rechargeScope(1)
			a := rechargeFor(1)
			put(t, s, a)
			a.Enabled, a.DisabledReason = false, "paused by the customer"
			put(t, s, a)
			if got := getRecharge(t, s, sc); got.Enabled || got.DisabledReason != "paused by the customer" {
				t.Fatalf("instruction = %+v", got)
			}
			a.Enabled, a.DisabledReason = true, ""
			put(t, s, a)
			if got := getRecharge(t, s, sc); !got.Enabled || got.DisabledReason != "" {
				t.Fatalf("instruction = %+v", got)
			}
			if _, refusal := claim(t, s, sc, rechargeDay); refusal != ports.RechargeOK {
				t.Fatalf("a re-enabled instruction claims: %q", refusal)
			}
		}},
		{"scopes do not share a ledger", func(t testing.TB, mk AutoRechargesFactory) {
			s := mk(t)
			a, b := rechargeFor(1), rechargeFor(2)
			a.MaxPerDay = 1
			put(t, s, a)
			put(t, s, b)
			claim(t, s, a.Scope, rechargeDay)
			settle(t, s, a.Scope, true, "", rechargeDay)
			if _, refusal := claim(t, s, a.Scope, rechargeDay); refusal != ports.RechargeMaxPerDay {
				t.Fatalf("a's day is spent: %q", refusal)
			}
			if c, refusal := claim(t, s, b.Scope, rechargeDay); refusal != ports.RechargeOK || c.N != 1 {
				t.Fatalf("b's day is untouched: %+v %q", c, refusal)
			}
			settle(t, s, b.Scope, false, "card_declined", rechargeDay)
			if got := getRecharge(t, s, a.Scope); got.ConsecutiveFailed != 0 {
				t.Fatalf("b's failure counted against a: %+v", got)
			}
		}},
		{"settling a scope with no instruction is not found", func(t testing.TB, mk AutoRechargesFactory) {
			s := mk(t)
			if _, err := s.Settle(Ctx(), rechargeScope(9), true, "", rechargeDay); !errors.Is(err, md.ErrNotFound) {
				t.Fatalf("err = %v", err)
			}
		}},
	}
}

// AutoRechargesContract is the specification of the store of standing instructions to top up (invariant 18): the day's
// slots are numbered and bounded however many triggers fire at once; a duplicate resumes the slot in flight, so one key is
// charged once; three failures in a row switch the instruction off and say so once; and editing it never refills the day.
func AutoRechargesContract(t *testing.T, mk AutoRechargesFactory) {
	t.Helper()
	runStoreCases(t, AutoRechargeCases(), mk)
}

// MemAutoRecharges is the store of standing instructions in memory. It applies the domain's rules, as every store does;
// the exported flags break it in the ways the contract must be able to see.
type MemAutoRecharges struct {
	// Racy makes Claim read the instruction, pause and write it back without holding the lock: two triggers read the same
	// state and both take the same slot.
	Racy bool
	// ResetLedgerOnPut makes Put write the ledger it was given (empty), so editing an instruction refills the day.
	ResetLedgerOnPut bool
	// NeverDisables makes Settle count failures and never switch the instruction off.
	NeverDisables bool
	// ForgetLedger makes Claim answer with a slot but not remember it: every trigger is the first.
	ForgetLedger bool

	mu   sync.Mutex
	log  *CallLog
	rows map[string]domain.AutoRecharge
}

// NewMemAutoRecharges is an empty store.
func NewMemAutoRecharges(log *CallLog) *MemAutoRecharges {
	return &MemAutoRecharges{log: log, rows: map[string]domain.AutoRecharge{}}
}

// Get implements ports.AutoRecharges.
func (m *MemAutoRecharges) Get(_ context.Context, s domain.Scope) (domain.AutoRecharge, bool, error) {
	m.log.Add("autorecharge.Get")
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.rows[s.Tag()]
	return a, ok, nil
}

// Put implements ports.AutoRecharges: the configuration is stored and the ledger is left alone. Enabling clears the
// failures; disabling records the reason given.
func (m *MemAutoRecharges) Put(_ context.Context, a domain.AutoRecharge) error {
	m.log.Add("autorecharge.Put")
	if err := a.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.rows[a.Scope.Tag()]; ok && !m.ResetLedgerOnPut {
		a.Day, a.ChargesToday, a.InflightN, a.InflightSince, a.LastTriggeredAt = cur.Day, cur.ChargesToday, cur.InflightN, cur.InflightSince, cur.LastTriggeredAt
		a.ConsecutiveFailed = cur.ConsecutiveFailed
	} else {
		a.Day, a.ChargesToday, a.InflightN, a.InflightSince, a.LastTriggeredAt, a.ConsecutiveFailed = "", 0, 0, nil, nil, 0
	}
	if a.Enabled {
		a.ConsecutiveFailed, a.DisabledReason = 0, ""
	} else if a.DisabledReason == "" {
		a.DisabledReason = "disabled"
	}
	m.rows[a.Scope.Tag()] = a
	return nil
}

// Claim implements ports.AutoRecharges: read, decide and write under one lock.
func (m *MemAutoRecharges) Claim(_ context.Context, s domain.Scope, now time.Time, staleAfter time.Duration) (ports.RechargeClaim, ports.RechargeRefusal, error) {
	m.log.Add("autorecharge.Claim")
	if m.Racy {
		return m.claimRacy(s, now, staleAfter)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.rows[s.Tag()]
	if !ok {
		return ports.RechargeClaim{}, ports.RechargeDisabled, nil
	}
	next, c, refusal := cur.Claim(now, staleAfter)
	if refusal == ports.RechargeOK && !m.ForgetLedger {
		m.rows[s.Tag()] = next
	}
	return c, refusal, nil
}

// claimRacy reads, pauses and writes without holding the lock between: the defect Racy stands for.
func (m *MemAutoRecharges) claimRacy(s domain.Scope, now time.Time, staleAfter time.Duration) (ports.RechargeClaim, ports.RechargeRefusal, error) {
	m.mu.Lock()
	cur, ok := m.rows[s.Tag()]
	m.mu.Unlock()
	if !ok {
		return ports.RechargeClaim{}, ports.RechargeDisabled, nil
	}
	next, c, refusal := cur.Claim(now, staleAfter)
	time.Sleep(2 * time.Millisecond)
	if refusal == ports.RechargeOK {
		m.mu.Lock()
		m.rows[s.Tag()] = next
		m.mu.Unlock()
	}
	return c, refusal, nil
}

// Settle implements ports.AutoRecharges.
func (m *MemAutoRecharges) Settle(_ context.Context, s domain.Scope, ok bool, reason string, _ time.Time) (bool, error) {
	m.log.Add("autorecharge.Settle")
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, found := m.rows[s.Tag()]
	if !found {
		return false, fmt.Errorf("%w: no auto top-up for %s", md.ErrNotFound, s.Tag())
	}
	next, disabled := cur.Settle(ok, reason)
	if m.NeverDisables && disabled {
		next.Enabled, next.DisabledReason, disabled = true, "", false
	}
	m.rows[s.Tag()] = next
	return disabled, nil
}

var _ ports.AutoRecharges = (*MemAutoRecharges)(nil)
