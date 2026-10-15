package domain

import (
	"fmt"
	"regexp"
	"strconv"
	"time"

	md "github.com/truongpx396/intel-payment/metering/domain"
)

// AutoRecharge is a scope's standing instruction: when the pool's balance crosses below Threshold, charge the saved
// payment method off-session for one pack of PlanCode. Credits still arrive only when the verified payment is
// processed (invariant 18). The method is the provider's reference — never card data.
//
// It also holds the day's ledger of charge slots, which is what bounds it: at most MaxPerDay distinct charges per UTC day
// however many times the trigger fires. The ledger changes only through Claim and Settle, which are pure, so every store
// applies the same rules and a rule is tested once.
type AutoRecharge struct {
	Scope             Scope
	Enabled           bool
	Pool              Pool
	Threshold         int64 // credits; the pool is recharged when it falls below this
	PlanCode          string
	Currency          string
	Account           string
	PaymentMethodRef  string
	MaxPerDay         int // distinct charges per scope per UTC day (1..50)
	LastTriggeredAt   *time.Time
	ConsecutiveFailed int    // failed charges since the last good one; three disable the instruction
	DisabledReason    string // why it was switched off, if it was

	// The day's ledger. Day is the UTC day the counts are for; ChargesToday is how many slots it has handed out, and
	// InflightN the one claimed and not yet settled (0 for none), since InflightSince.
	Day           string
	ChargesToday  int
	InflightN     int
	InflightSince *time.Time
}

// The limits of an instruction.
const (
	MinRechargesPerDay = 1
	MaxRechargesPerDay = 50
	// MaxConsecutiveRechargeFailures is how many failed charges in a row switch an instruction off.
	MaxConsecutiveRechargeFailures = 3
)

// RechargeClaim is a slot to charge a saved method in.
type RechargeClaim struct {
	Day     string // the UTC day, 2006-01-02
	N       int    // this day's nth charge, 1-based
	Resumed bool   // the slot was already claimed and not settled: a duplicate trigger, which must reuse it
}

// RechargeRefusal says why no slot was claimed.
type RechargeRefusal string

// The refusals; the zero value means a slot was claimed.
const (
	RechargeOK        RechargeRefusal = ""
	RechargeDisabled  RechargeRefusal = "disabled"
	RechargeMaxPerDay RechargeRefusal = "max_per_day"
)

// RechargeKey is the idempotency key of one charge slot: recharge:<realm>/<scope>:<day>:<n>. It is what the provider
// de-duplicates on, so a duplicated trigger that names the same slot charges once. Read from the right it is unambiguous
// however many colons a scope id has — the slot after the last colon, the ten-character day before it — so two slots can
// never share a key.
func RechargeKey(s Scope, day string, n int) string {
	return "recharge:" + s.Tag() + ":" + day + ":" + strconv.Itoa(n)
}

var currencyCode = regexp.MustCompile(`^[A-Z]{3}$`)

// Validate refuses an instruction that could not charge safely.
func (a AutoRecharge) Validate() error {
	if err := a.Scope.Validate(); err != nil {
		return err
	}
	switch {
	case a.Threshold < 1:
		return fmt.Errorf("%w: an auto top-up threshold is at least one credit, not %d", md.ErrInvalid, a.Threshold)
	case a.PlanCode == "":
		return fmt.Errorf("%w: an auto top-up buys a plan, and has none", md.ErrInvalid)
	case !currencyCode.MatchString(a.Currency):
		return fmt.Errorf("%w: auto top-up currency %q is not an upper-case ISO 4217 code", md.ErrInvalid, a.Currency)
	case a.Account == "":
		return fmt.Errorf("%w: an auto top-up charges a provider account, and has none", md.ErrInvalid)
	case a.PaymentMethodRef == "":
		return fmt.Errorf("%w: an auto top-up charges a saved payment method, and has none", md.ErrInvalid)
	case a.MaxPerDay < MinRechargesPerDay || a.MaxPerDay > MaxRechargesPerDay:
		return fmt.Errorf("%w: an auto top-up makes between %d and %d charges a day, not %d", md.ErrInvalid, MinRechargesPerDay, MaxRechargesPerDay, a.MaxPerDay)
	}
	return nil
}

// Claim reserves a charge slot at now. It returns the instruction as it stands after the claim, the slot, and — when no
// slot can be had — why not, with the instruction unchanged.
//
//   - A day hands out at most MaxPerDay slots, numbered 1, 2, 3…, in the UTC day of now. The day never goes back: a clock
//     that steps back across midnight keeps counting the later day, so it cannot hand out a day's charges twice.
//   - While a slot is in flight and younger than staleAfter, a claim RESUMES it: a duplicated trigger names the same slot,
//     so the same key, and the provider charges once. Resuming costs nothing, so it is allowed at the maximum.
//   - A slot claimed and never settled is replaced once it is stale — but it was spent, and still counts, so abandoning
//     slots cannot get round the maximum.
func (a AutoRecharge) Claim(now time.Time, staleAfter time.Duration) (AutoRecharge, RechargeClaim, RechargeRefusal) {
	held := a
	if !a.Enabled {
		return held, RechargeClaim{}, RechargeDisabled
	}
	day := now.UTC().Format(time.DateOnly)
	if day < a.Day {
		day = a.Day
	}
	if day != a.Day {
		a.Day, a.ChargesToday, a.InflightN, a.InflightSince = day, 0, 0, nil
	}
	if a.InflightN > 0 && a.InflightSince != nil && now.Sub(*a.InflightSince) < staleAfter {
		return a, RechargeClaim{Day: a.Day, N: a.InflightN, Resumed: true}, RechargeOK
	}
	if a.ChargesToday >= a.MaxPerDay {
		return held, RechargeClaim{}, RechargeMaxPerDay
	}
	at := now.UTC()
	a.ChargesToday++
	a.InflightN, a.InflightSince, a.LastTriggeredAt = a.ChargesToday, &at, &at
	return a, RechargeClaim{Day: a.Day, N: a.ChargesToday}, RechargeOK
}

// Settle ends the slot in flight with the charge's outcome. A good charge clears the count of failures; a failed one adds
// one, and the third in a row switches the instruction off and records why — the reason given, or a default so the
// customer can always be told. It reports whether THIS settling switched it off, so the news is made once. Nothing here
// switches an instruction back on: only its owner does, by writing a new one.
func (a AutoRecharge) Settle(ok bool, reason string) (AutoRecharge, bool) {
	a.InflightN, a.InflightSince = 0, nil
	if ok {
		a.ConsecutiveFailed = 0
		return a, false
	}
	a.ConsecutiveFailed++
	if !a.Enabled || a.ConsecutiveFailed < MaxConsecutiveRechargeFailures {
		return a, false
	}
	a.Enabled = false
	a.DisabledReason = reason
	if a.DisabledReason == "" {
		a.DisabledReason = "consecutive_failures"
	}
	return a, true
}
