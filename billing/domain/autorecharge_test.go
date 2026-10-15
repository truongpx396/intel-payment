package domain_test

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	domain "github.com/truongpx396/intel-payment/billing/domain"
)

func workspace(id string) domain.Scope {
	return domain.Scope{Realm: "default", Kind: "workspace", ID: id}
}

var dayStart = time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)

func instruction(maxPerDay int) domain.AutoRecharge {
	return domain.AutoRecharge{Scope: workspace("w1"), Enabled: true, Pool: "general", Threshold: 1000, PlanCode: "pack", Currency: "USD",
		Account: "stripe-a", PaymentMethodRef: "pm_1", MaxPerDay: maxPerDay}
}

const stale = 10 * time.Minute

// settleInflight settles the slot that is in flight, which is what a handler does with the slot it has just claimed.
func settleInflight(a domain.AutoRecharge, ok bool, reason string) (domain.AutoRecharge, bool) {
	return a.Settle(domain.RechargeClaim{Day: a.Day, N: a.InflightN}, ok, reason)
}

// ------------------------------------------------------------------- the key --

func TestARechargeKeyNamesTheScopeTheDayAndTheSlot(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		scope domain.Scope
		day   string
		n     int
		want  string
	}{
		{workspace("w1"), "2026-03-01", 1, "recharge:default/workspace:w1:2026-03-01:1"},
		{workspace("w1"), "2026-03-01", 2, "recharge:default/workspace:w1:2026-03-01:2"},
		{workspace("w1"), "2026-03-02", 1, "recharge:default/workspace:w1:2026-03-02:1"},
		{domain.Scope{Kind: "user", ID: "u:9"}, "2026-03-01", 12, "recharge:default/user:u:9:2026-03-01:12"}, // an empty realm is the default realm
		{domain.Scope{Realm: "acme", Kind: "workspace", ID: "w1"}, "2026-03-01", 1, "recharge:acme/workspace:w1:2026-03-01:1"},
	} {
		if got := domain.RechargeKey(tc.scope, tc.day, tc.n); got != tc.want {
			t.Errorf("RechargeKey(%+v, %s, %d) = %q, want %q", tc.scope, tc.day, tc.n, got, tc.want)
		}
	}
}

// parseKey reads a recharge key from the RIGHT, which is how it is unambiguous however many colons a scope id has: the
// slot is after the last colon, the day is the ten characters before it.
func parseKey(key string) (tag, day string, n int, ok bool) {
	rest, found := strings.CutPrefix(key, "recharge:")
	if !found {
		return "", "", 0, false
	}
	i := strings.LastIndex(rest, ":")
	if i < 0 {
		return "", "", 0, false
	}
	n, err := strconv.Atoi(rest[i+1:])
	rest = rest[:i]
	if err != nil || len(rest) < 11 || rest[len(rest)-11] != ':' {
		return "", "", 0, false
	}
	return rest[:len(rest)-11], rest[len(rest)-10:], n, true
}

func FuzzARechargeKeyNeverSharesAKeyBetweenTwoSlots(f *testing.F) {
	f.Add("w1", "w1", 1, 2, true)
	f.Add("a", "a:2026-03-01", 1, 1, false)
	f.Add("a:2026-03-01:1", "a", 1, 1, true)
	f.Add("a:", "a", 5, 5, false)
	f.Add("", "x", 1, 1, true)
	f.Fuzz(func(t *testing.T, id1, id2 string, n1, n2 int, sameDay bool) {
		s1, s2 := workspace(id1), workspace(id2)
		if s1.Validate() != nil || s2.Validate() != nil || n1 < 1 || n2 < 1 {
			t.Skip()
		}
		d1, d2 := "2026-03-01", "2026-03-01"
		if !sameDay {
			d2 = "2026-12-31"
		}
		k1, k2 := domain.RechargeKey(s1, d1, n1), domain.RechargeKey(s2, d2, n2)
		// The key can be read back to exactly what made it...
		if tag, day, n, ok := parseKey(k1); !ok || tag != s1.Tag() || day != d1 || n != n1 {
			t.Fatalf("%q reads back as (%q, %q, %d, %v), want (%q, %q, %d)", k1, tag, day, n, ok, s1.Tag(), d1, n1)
		}
		// ...so two different slots can never share one: a duplicated trigger names the same key, and no two charges do.
		if k1 == k2 && (s1 != s2 || d1 != d2 || n1 != n2) {
			t.Fatalf("slots (%+v, %s, %d) and (%+v, %s, %d) share the key %q: one would be taken for the other", s1, d1, n1, s2, d2, n2, k1)
		}
	})
}

// ---------------------------------------------------------------- validation --

func TestAnInstructionMustBeOneThatCouldChargeSafely(t *testing.T) {
	t.Parallel()
	if err := instruction(3).Validate(); err != nil {
		t.Fatalf("a good instruction was refused: %v", err)
	}
	for name, mutate := range map[string]func(*domain.AutoRecharge){
		"a scope with no kind":            func(a *domain.AutoRecharge) { a.Scope.Kind = "" },
		"a scope with no id":              func(a *domain.AutoRecharge) { a.Scope.ID = "" },
		"a threshold of nothing":          func(a *domain.AutoRecharge) { a.Threshold = 0 },
		"a negative threshold":            func(a *domain.AutoRecharge) { a.Threshold = -1 },
		"no plan":                         func(a *domain.AutoRecharge) { a.PlanCode = "" },
		"no currency":                     func(a *domain.AutoRecharge) { a.Currency = "" },
		"a currency in lower case":        func(a *domain.AutoRecharge) { a.Currency = "usd" },
		"a currency of two letters":       func(a *domain.AutoRecharge) { a.Currency = "US" },
		"a currency of four letters":      func(a *domain.AutoRecharge) { a.Currency = "USDX" },
		"no provider account":             func(a *domain.AutoRecharge) { a.Account = "" },
		"no saved payment method":         func(a *domain.AutoRecharge) { a.PaymentMethodRef = "" },
		"no charges a day":                func(a *domain.AutoRecharge) { a.MaxPerDay = 0 },
		"a negative number of charges":    func(a *domain.AutoRecharge) { a.MaxPerDay = -1 },
		"more charges a day than allowed": func(a *domain.AutoRecharge) { a.MaxPerDay = 51 },
	} {
		a := instruction(3)
		mutate(&a)
		if err := a.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	for _, edge := range []struct {
		name   string
		mutate func(*domain.AutoRecharge)
	}{
		{"one charge a day", func(a *domain.AutoRecharge) { a.MaxPerDay = 1 }},
		{"fifty charges a day", func(a *domain.AutoRecharge) { a.MaxPerDay = 50 }},
		{"a threshold of one credit", func(a *domain.AutoRecharge) { a.Threshold = 1 }},
		{"a disabled instruction", func(a *domain.AutoRecharge) { a.Enabled = false; a.DisabledReason = "consecutive_failures" }},
	} {
		a := instruction(3)
		edge.mutate(&a)
		if err := a.Validate(); err != nil {
			t.Errorf("%s was refused: %v", edge.name, err)
		}
	}
}

// ----------------------------------------------------------------- claiming --

func TestClaimingTheFirstSlotOfADay(t *testing.T) {
	t.Parallel()
	a := instruction(3)
	next, c, refusal := a.Claim(dayStart, stale)
	if refusal != domain.RechargeOK || c != (domain.RechargeClaim{Day: "2026-03-01", N: 1}) {
		t.Fatalf("claim = %+v refusal = %q", c, refusal)
	}
	if next.Day != "2026-03-01" || next.ChargesToday != 1 || next.InflightN != 1 || next.InflightSince == nil || !next.InflightSince.Equal(dayStart) ||
		next.LastTriggeredAt == nil || !next.LastTriggeredAt.Equal(dayStart) {
		t.Fatalf("state = %+v", next)
	}
	if a.ChargesToday != 0 || a.InflightN != 0 || a.InflightSince != nil || a.LastTriggeredAt != nil {
		t.Fatalf("the receiver changed: %+v — a claim is a new state, not an edit of the old one", a)
	}
}

// A trigger that fires twice, or two replicas that handle it at once, must name the same key: the provider then charges once.
func TestADuplicatedTriggerResumesTheSlotInsteadOfTakingAnother(t *testing.T) {
	t.Parallel()
	a := instruction(3)
	a, first, _ := a.Claim(dayStart, stale)
	a, again, refusal := a.Claim(dayStart.Add(time.Minute), stale)
	if refusal != domain.RechargeOK || again != (domain.RechargeClaim{Day: first.Day, N: first.N, Resumed: true}) {
		t.Fatalf("second claim = %+v refusal = %q, want slot %d again, resumed", again, refusal, first.N)
	}
	if a.ChargesToday != 1 {
		t.Fatalf("ChargesToday = %d: a duplicate was counted as a charge", a.ChargesToday)
	}
	for range 20 {
		var c domain.RechargeClaim
		if a, c, _ = a.Claim(dayStart.Add(2*time.Minute), stale); c.N != 1 || !c.Resumed {
			t.Fatalf("claim = %+v", c)
		}
	}
	if a.ChargesToday != 1 {
		t.Fatalf("ChargesToday = %d after twenty duplicates", a.ChargesToday)
	}
}

func TestAfterASlotIsSettledTheNextClaimIsTheNextSlot(t *testing.T) {
	t.Parallel()
	a := instruction(3)
	a, _, _ = a.Claim(dayStart, stale)
	a, _ = settleInflight(a, true, "")
	a, c, refusal := a.Claim(dayStart.Add(time.Minute), stale)
	if refusal != domain.RechargeOK || c != (domain.RechargeClaim{Day: "2026-03-01", N: 2}) {
		t.Fatalf("claim = %+v refusal = %q", c, refusal)
	}
	if a.ChargesToday != 2 || a.InflightN != 2 {
		t.Fatalf("state = %+v", a)
	}
}

func TestADayHoldsAtMostMaxPerDayCharges(t *testing.T) {
	t.Parallel()
	for _, perDay := range []int{1, 2, 3, 50} {
		a := instruction(perDay)
		for n := 1; n <= perDay; n++ {
			var c domain.RechargeClaim
			var refusal domain.RechargeRefusal
			if a, c, refusal = a.Claim(dayStart.Add(time.Duration(n)*time.Minute), stale); refusal != domain.RechargeOK || c.N != n || c.Resumed {
				t.Fatalf("perDay %d, claim %d = %+v refusal = %q", perDay, n, c, refusal)
			}
			a, _ = settleInflight(a, true, "")
		}
		before := a
		next, c, refusal := a.Claim(dayStart.Add(time.Hour), stale)
		if refusal != domain.RechargeMaxPerDay || c != (domain.RechargeClaim{}) {
			t.Fatalf("perDay %d: claim %d = %+v refusal = %q, want max_per_day", perDay, perDay+1, c, refusal)
		}
		if !reflect.DeepEqual(next, before) {
			t.Fatalf("a refused claim changed the state:\nbefore %+v\nafter  %+v", before, next)
		}
	}
}

func TestResumingASlotIsAllowedAtTheDailyMaximum(t *testing.T) {
	t.Parallel()
	a := instruction(2)
	a, _, _ = a.Claim(dayStart, stale)
	a, _ = settleInflight(a, true, "")
	a, last, _ := a.Claim(dayStart.Add(time.Minute), stale) // the second and last of the day, still in flight
	_, c, refusal := a.Claim(dayStart.Add(2*time.Minute), stale)
	if refusal != domain.RechargeOK || c != (domain.RechargeClaim{Day: last.Day, N: 2, Resumed: true}) {
		t.Fatalf("claim = %+v refusal = %q: a duplicate of the last slot is not a new charge", c, refusal)
	}
}

// A charge that was claimed and never settled — the process died, or the webhook never came — does not hold the slot for
// ever; but the slot it took is spent, so abandoning slots cannot get around the daily maximum.
func TestAnAbandonedSlotIsReplacedButStillCounts(t *testing.T) {
	t.Parallel()
	a := instruction(2)
	a, _, _ = a.Claim(dayStart, stale)
	a, c, refusal := a.Claim(dayStart.Add(stale-time.Second), stale)
	if refusal != domain.RechargeOK || !c.Resumed || c.N != 1 {
		t.Fatalf("a second before it is stale: %+v %q", c, refusal)
	}
	a, c, refusal = a.Claim(dayStart.Add(stale), stale)
	if refusal != domain.RechargeOK || c.Resumed || c.N != 2 || a.ChargesToday != 2 {
		t.Fatalf("exactly stale: %+v %q state %+v, want a new slot 2", c, refusal, a)
	}
	_, _, refusal = a.Claim(dayStart.Add(3*stale), stale)
	if refusal != domain.RechargeMaxPerDay {
		t.Fatalf("both slots abandoned and the day's maximum reached: refusal = %q", refusal)
	}
}

func TestADisabledInstructionClaimsNothing(t *testing.T) {
	t.Parallel()
	a := instruction(3)
	a.Enabled, a.DisabledReason = false, "consecutive_failures"
	next, c, refusal := a.Claim(dayStart, stale)
	if refusal != domain.RechargeDisabled || c != (domain.RechargeClaim{}) || !reflect.DeepEqual(next, a) {
		t.Fatalf("claim = %+v refusal = %q state %+v", c, refusal, next)
	}
}

func TestANewUTCDayStartsFromOne(t *testing.T) {
	t.Parallel()
	a := instruction(2)
	a, _, _ = a.Claim(dayStart, stale)
	a, _ = settleInflight(a, true, "")
	a, _, _ = a.Claim(dayStart.Add(time.Minute), stale)
	// Still in flight from yesterday: it is yesterday's, and its key names yesterday.
	tomorrow := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	a, c, refusal := a.Claim(tomorrow, stale)
	if refusal != domain.RechargeOK || c != (domain.RechargeClaim{Day: "2026-03-02", N: 1}) {
		t.Fatalf("claim = %+v refusal = %q", c, refusal)
	}
	if a.ChargesToday != 1 || a.Day != "2026-03-02" {
		t.Fatalf("state = %+v", a)
	}
	if key := domain.RechargeKey(a.Scope, c.Day, c.N); key != "recharge:default/workspace:w1:2026-03-02:1" {
		t.Fatalf("key = %q", key)
	}
}

// A slot claimed just before midnight and still fresh just after it belongs to the day before, and its key says so. A trigger
// after midnight is the new day's first, not a duplicate of it: carrying the slot across would name yesterday's charge under
// today's key.
func TestASlotStillInFlightAtMidnightIsNotCarriedIntoTheNewDay(t *testing.T) {
	t.Parallel()
	a := instruction(3)
	before := time.Date(2026, 3, 1, 23, 58, 0, 0, time.UTC)
	a, yesterdays, _ := a.Claim(before, stale)
	next, c, refusal := a.Claim(before.Add(4*time.Minute), stale) // 00:02, younger than the staleness window
	if refusal != domain.RechargeOK || c != (domain.RechargeClaim{Day: "2026-03-02", N: 1}) {
		t.Fatalf("claim = %+v refusal = %q, want slot 1 of 2026-03-02, not a resumption of %+v", c, refusal, yesterdays)
	}
	if next.ChargesToday != 1 || next.InflightN != 1 {
		t.Fatalf("state = %+v", next)
	}
	if domain.RechargeKey(a.Scope, yesterdays.Day, yesterdays.N) == domain.RechargeKey(a.Scope, c.Day, c.N) {
		t.Fatal("yesterday's charge and today's share a key")
	}
}

func TestTheDayIsTheUTCDayWhateverZoneTheClockIsIn(t *testing.T) {
	t.Parallel()
	bangkok := time.FixedZone("ICT", 7*3600)
	late := time.Date(2026, 3, 2, 5, 0, 0, 0, bangkok) // 2026-03-01 22:00 UTC
	_, c, _ := instruction(3).Claim(late, stale)
	if c.Day != "2026-03-01" {
		t.Fatalf("day = %q, want the UTC day 2026-03-01", c.Day)
	}
}

// A clock that steps back across midnight must not hand out a second day's worth of charges.
func TestAClockThatRunsBackwardsDoesNotResetTheDay(t *testing.T) {
	t.Parallel()
	a := instruction(2)
	today := time.Date(2026, 3, 2, 0, 5, 0, 0, time.UTC)
	a, _, _ = a.Claim(today, stale)
	a, _ = settleInflight(a, true, "")
	a, _, _ = a.Claim(today.Add(time.Minute), stale)
	a, _ = settleInflight(a, true, "")
	yesterday := today.Add(-time.Hour) // 2026-03-01 23:05
	_, _, refusal := a.Claim(yesterday, stale)
	if refusal != domain.RechargeMaxPerDay {
		t.Fatalf("refusal = %q: stepping the clock back reset the count", refusal)
	}
}

// ----------------------------------------------------------------- settling --

func TestASuccessClearsTheSlotAndTheFailures(t *testing.T) {
	t.Parallel()
	a := instruction(3)
	a.ConsecutiveFailed = 2
	a, _, _ = a.Claim(dayStart, stale)
	next, disabled := settleInflight(a, true, "")
	if disabled || next.InflightN != 0 || next.InflightSince != nil || next.ConsecutiveFailed != 0 || !next.Enabled {
		t.Fatalf("state = %+v disabled = %v", next, disabled)
	}
	if next.ChargesToday != 1 {
		t.Fatalf("ChargesToday = %d: settling does not give the slot back", next.ChargesToday)
	}
}

func TestThreeFailuresInARowSwitchTheInstructionOffWithTheReason(t *testing.T) {
	t.Parallel()
	a := instruction(50)
	for i := 1; i <= 3; i++ {
		a, _, _ = a.Claim(dayStart.Add(time.Duration(i)*time.Minute), stale)
		var disabled bool
		a, disabled = settleInflight(a, false, "card_declined")
		if want := i == 3; disabled != want || a.Enabled == want {
			t.Fatalf("failure %d: disabled = %v enabled = %v", i, disabled, a.Enabled)
		}
		if a.ConsecutiveFailed != i {
			t.Fatalf("failure %d: ConsecutiveFailed = %d", i, a.ConsecutiveFailed)
		}
	}
	if a.DisabledReason != "card_declined" {
		t.Fatalf("DisabledReason = %q", a.DisabledReason)
	}
	if _, _, refusal := a.Claim(dayStart.Add(time.Hour), stale); refusal != domain.RechargeDisabled {
		t.Fatalf("a disabled instruction claimed a slot: %q", refusal)
	}
}

func TestASuccessBetweenFailuresStartsTheCountAgain(t *testing.T) {
	t.Parallel()
	a := instruction(50)
	for _, ok := range []bool{false, false, true, false, false} {
		a, _, _ = a.Claim(dayStart, stale)
		a, _ = settleInflight(a, ok, "card_declined")
	}
	if !a.Enabled || a.ConsecutiveFailed != 2 {
		t.Fatalf("state = %+v: two failures, a success and two failures is two in a row", a)
	}
}

func TestADisabledInstructionIsNotDisabledAgainAndKeepsItsFirstReason(t *testing.T) {
	t.Parallel()
	a := instruction(3)
	a.ConsecutiveFailed = 2
	a, disabled := settleInflight(a, false, "card_declined")
	if !disabled {
		t.Fatal("the third failure did not disable it")
	}
	a, disabled = settleInflight(a, false, "expired_card")
	if disabled || a.DisabledReason != "card_declined" {
		t.Fatalf("disabled = %v reason = %q: a late failure must neither announce again nor overwrite why it was switched off", disabled, a.DisabledReason)
	}
	if a, _ = settleInflight(a, true, ""); a.Enabled {
		t.Fatal("a late success switched a disabled instruction back on: only its owner may do that")
	}
}

func TestAFailureWithNoReasonIsStillRecordedWhyItWasDisabled(t *testing.T) {
	t.Parallel()
	a := instruction(3)
	a.ConsecutiveFailed = 2
	if a, _ = settleInflight(a, false, ""); a.Enabled || a.DisabledReason == "" {
		t.Fatalf("state = %+v: the customer must be able to be told why", a)
	}
}

// A settlement says WHICH slot it is about. A webhook can be late: if slot one's arrives after slot two was claimed, it must not
// release slot two, or a duplicate trigger would claim a third slot under a new key and the customer would be charged twice for
// one low balance.
func TestASettlementReleasesOnlyTheSlotItNames(t *testing.T) {
	t.Parallel()
	a := instruction(5)
	a, one, _ := a.Claim(dayStart, stale)
	a, _ = a.Settle(one, true, "")
	a, two, _ := a.Claim(dayStart.Add(time.Minute), stale)
	inflight := *a.InflightSince

	// Slot one's report arrives again, late — a replayed webhook — and fails.
	late, disabled := a.Settle(one, false, "card_declined")
	if disabled || late.InflightN != two.N || late.InflightSince == nil || !late.InflightSince.Equal(inflight) {
		t.Fatalf("a late report of slot %d released slot %d: %+v", one.N, two.N, late)
	}
	if late.ConsecutiveFailed != 1 {
		t.Fatalf("a late failure still counts as a failure: %+v", late)
	}
	// A late success clears the failures and still leaves the slot in flight alone.
	late.ConsecutiveFailed = 2
	if late, _ = late.Settle(one, true, ""); late.ConsecutiveFailed != 0 || late.InflightN != two.N {
		t.Fatalf("a late success: %+v", late)
	}
	// A report of a slot from another day is not this day's slot either.
	other := domain.RechargeClaim{Day: "2026-02-28", N: two.N}
	if late, _ = late.Settle(other, true, ""); late.InflightN != two.N {
		t.Fatalf("a report of another day's slot %d released today's: %+v", other.N, late)
	}
	// The slot's own report releases it.
	if done, _ := late.Settle(two, true, ""); done.InflightN != 0 || done.InflightSince != nil {
		t.Fatalf("slot %d's own report did not release it: %+v", two.N, done)
	}
}

func TestARechargeKeyCanBeReadBack(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		scope domain.Scope
		day   string
		n     int
	}{
		{workspace("w1"), "2026-03-01", 1},
		{workspace("w:1:2026-03-01"), "2026-03-01", 12},
		{domain.Scope{Realm: "acme", Kind: "user", ID: "u/9"}, "2026-12-31", 50},
		{domain.Scope{Kind: "workspace", ID: "x"}, "2026-03-01", 3},
	} {
		key := domain.RechargeKey(tc.scope, tc.day, tc.n)
		tag, day, n, ok := domain.ParseRechargeKey(key)
		if !ok || tag != tc.scope.Tag() || day != tc.day || n != tc.n {
			t.Errorf("ParseRechargeKey(%q) = (%q, %q, %d, %v), want (%q, %q, %d)", key, tag, day, n, ok, tc.scope.Tag(), tc.day, tc.n)
		}
	}
	for _, bad := range []string{"", "recharge:", "recharge:default/workspace:w1", "recharge:default/workspace:w1:2026-03-01", "recharge:default/workspace:w1:2026-03-01:x",
		"recharge:default/workspace:w1:2026-03-01:0", "recharge:default/workspace:w1:2026-03-01:-1", "recharge:default/workspace:w1:2026-3-01:1",
		"recharge:default/workspace:w1:2026-03-01:", "payment-pi_123", "refund:re_1", ":2026-03-01:1", "recharge::2026-03-01:1"} {
		if tag, day, n, ok := domain.ParseRechargeKey(bad); ok {
			t.Errorf("ParseRechargeKey(%q) = (%q, %q, %d), want not ok: it is not a recharge key", bad, tag, day, n)
		}
	}
}

func FuzzParseRechargeKeyReadsBackWhatRechargeKeyWrote(f *testing.F) {
	f.Add("w1", 1)
	f.Add("a:2026-03-01:1", 7)
	f.Add("", 3)
	f.Add("w/1", 50)
	f.Fuzz(func(t *testing.T, id string, n int) {
		s := workspace(id)
		if s.Validate() != nil || n < 1 {
			t.Skip()
		}
		key := domain.RechargeKey(s, "2026-03-01", n)
		if tag, day, got, ok := domain.ParseRechargeKey(key); !ok || tag != s.Tag() || day != "2026-03-01" || got != n {
			t.Fatalf("%q reads back as (%q, %q, %d, %v)", key, tag, day, got, ok)
		}
	})
}

// FuzzParseRechargeKeyNeverReadsWhatIsNotAKey holds the reader to what the writer promises, on arbitrary text: whatever it
// accepts is exactly a key the writer would have made.
func FuzzParseRechargeKeyNeverReadsWhatIsNotAKey(f *testing.F) {
	f.Add("recharge:default/workspace:w1:2026-03-01:1")
	f.Add("recharge:default/workspace:w1:2026-03-01:01")
	f.Add("recharge:default/workspace:w1:2026-03-01:+1")
	f.Add("recharge:default/workspace:w1:2026-13-45:1")
	f.Fuzz(func(t *testing.T, key string) {
		tag, day, n, ok := domain.ParseRechargeKey(key)
		if !ok {
			return
		}
		if _, err := time.Parse(time.DateOnly, day); err != nil || n < 1 || key != "recharge:"+tag+":"+day+":"+strconv.Itoa(n) {
			t.Fatalf("%q was read as (%q, %q, %d), which is not what would write it", key, tag, day, n)
		}
	})
}

// ------------------------------------------------------------ the whole ledger --

// FuzzTheLedgerHoldsItsBoundsWhateverTheOrderOfEvents drives the claim and the settlement with arbitrary interleavings —
// duplicated triggers, settlements that arrive late or twice, a clock that jumps both ways across midnight — and holds
// them to what invariant 18 promises: no day hands out more than max_per_day distinct slots; the slots of a day are 1, 2, 3…
// without gaps or repeats; a duplicate while one is in flight is that slot again; a switched-off instruction stays off.
func FuzzTheLedgerHoldsItsBoundsWhateverTheOrderOfEvents(f *testing.F) {
	f.Add([]byte{0, 0, 0, 1, 0, 0, 1, 0, 0, 1, 0, 0}, uint8(2), uint8(5))
	f.Add([]byte{0, 2, 0, 2, 0, 2, 0, 0, 1, 0}, uint8(50), uint8(1))
	f.Add([]byte{0, 3, 0, 0x7f, 0, 0x80, 0, 3, 3, 0, 0, 1}, uint8(1), uint8(3))
	f.Add([]byte{0, 0, 1, 1, 1, 2, 2, 2, 0, 3, 0, 0}, uint8(3), uint8(29))
	f.Fuzz(func(t *testing.T, ops []byte, maxByte, staleByte uint8) {
		perDay := int(maxByte)%50 + 1
		staleAfter := time.Duration(int(staleByte)%30+1) * time.Minute
		a := instruction(perDay)
		now := dayStart
		handed := map[string]map[int]bool{} // day → the slots handed out afresh
		everEnabledFalse := false
		for i, b := range ops {
			before := a
			switch b % 6 {
			case 0:
				next, c, refusal := a.Claim(now, staleAfter)
				switch {
				case refusal != domain.RechargeOK:
					if c != (domain.RechargeClaim{}) || !reflect.DeepEqual(next, before) {
						t.Fatalf("op %d: refused claim %q returned %+v and changed the state", i, refusal, c)
					}
					if refusal == domain.RechargeDisabled && before.Enabled {
						t.Fatalf("op %d: an enabled instruction was refused as disabled", i)
					}
					if refusal == domain.RechargeMaxPerDay && before.ChargesToday < perDay {
						t.Fatalf("op %d: refused at the maximum with only %d of %d spent", i, before.ChargesToday, perDay)
					}
				case c.Resumed:
					if !handed[c.Day][c.N] || next.InflightN != c.N || next.ChargesToday != before.ChargesToday && before.Day == c.Day {
						t.Fatalf("op %d: resumed %+v which was never handed out, or counted it again (%+v → %+v)", i, c, before, next)
					}
				default:
					if handed[c.Day] == nil {
						handed[c.Day] = map[int]bool{}
					}
					if handed[c.Day][c.N] || c.N != len(handed[c.Day])+1 || c.N > perDay {
						t.Fatalf("op %d: slot %+v is a repeat, leaves a gap or passes the maximum of %d (handed out so far: %v)", i, c, perDay, handed[c.Day])
					}
					handed[c.Day][c.N] = true
					if next.ChargesToday != c.N || next.InflightN != c.N {
						t.Fatalf("op %d: state %+v does not say slot %d is the day's latest and in flight", i, next, c.N)
					}
				}
				a = next
			case 1:
				a, _ = settleInflight(a, true, "")
			case 2:
				a, _ = settleInflight(a, false, "card_declined")
			case 4, 5:
				// A late report about an earlier slot: it counts, and never releases the slot in flight.
				late := domain.RechargeClaim{Day: a.Day, N: int(b>>3)%perDay + 1}
				if late.N == a.InflightN {
					late.N = a.InflightN%perDay + 1
					if late.N == a.InflightN { // a day of one slot: the only other slot is another day's
						late.Day = "1999-01-01"
					}
				}
				next, _ := a.Settle(late, b%6 == 4, "card_declined")
				if next.InflightN != a.InflightN || (a.InflightSince == nil) != (next.InflightSince == nil) {
					t.Fatalf("op %d: a report of slot %+v released the slot in flight (%d): %+v", i, late, a.InflightN, next)
				}
				a = next
			case 3:
				// A step of up to a day in either direction, in units that land on both sides of midnight.
				now = now.Add(time.Duration(int8(b>>2)) * 23 * time.Minute)
			}
			if !a.Enabled {
				everEnabledFalse = true
			} else if everEnabledFalse {
				t.Fatalf("op %d: an instruction that was switched off is on again: %+v", i, a)
			}
			if a.ConsecutiveFailed >= domain.MaxConsecutiveRechargeFailures && a.Enabled {
				t.Fatalf("op %d: %d failures in a row and still enabled", i, a.ConsecutiveFailed)
			}
			if a.ChargesToday > perDay || a.InflightN > a.ChargesToday {
				t.Fatalf("op %d: state %+v breaks its own bounds", i, a)
			}
		}
	})
}
