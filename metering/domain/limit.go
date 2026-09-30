package domain

import (
	"fmt"
	"time"
)

// Window classifies a limit counter's reset behavior.
type Window int

const (
	Balance Window = iota // the scope's eligible pools; Limit.Max is the permitted OVERDRAFT (→ 402)
	Daily                 // calendar day in Limit.TZ (→ 409)
	Hourly                // calendar hour in Limit.TZ (→ 409)
	Rolling               // sliding window of Limit.Dur, bucketed; error ≤ one bucket (→ 409)

	// Job is a cumulative budget for ONE unit of work, keyed by Subjects["job"] and expiring
	// after Limit.Dur. It exists because no other window bounds a multi-step job: a daily ceiling
	// still permits one runaway agent loop to burn the day in an hour, and MaxCost bounds one
	// call. It is a COUNTER, not a balance: nothing is granted to the job and no ledger row is
	// written for it, so an abandoned job leaks only a TTL'd key.
	Job
)

var windowNames = [...]string{Balance: "balance", Daily: "daily", Hourly: "hourly", Rolling: "rolling", Job: "job"}

func (w Window) String() string {
	if w < 0 || int(w) >= len(windowNames) {
		return fmt.Sprintf("window(%d)", int(w))
	}
	return windowNames[w]
}

// ParseWindow is the inverse of String, for configuration rows (limits.window_kind).
func ParseWindow(s string) (Window, error) {
	for w, n := range windowNames {
		if n == s {
			return Window(w), nil
		}
	}
	return 0, fmt.Errorf("%w: unknown window %q", ErrInvalid, s)
}

// Deny codes: the two classes of refusal a host maps to its wire status.
const (
	DenyPaymentRequired = "payment_required" // 402: a Balance limit
	DenyLimitReached    = "limit_reached"    // 409: a window limit
	DenyAccountBlocked  = "account_blocked"  // block_and_flag, pending an operator
)

// Limit is one ceiling evaluated at admission and counted at settlement.
type Limit struct {
	Name string // stable, surfaced in the deny reason: "scope_balance" | "user_daily" | "run_cap"
	// Subject is the subject kind whose counter this is ("user", "api_key", "job"), or "" for
	// the charged scope itself. A call that does not carry that subject in Subjects is refused
	// (ErrMissingSubject, 400): a ceiling that silently does not apply is the worst kind.
	Subject  string
	Resource string // "" = every resource; otherwise only this resource counts and is gated
	Unit     Unit   // "credit", or a metered Unit whose quantities the counter sums
	// Max is the ceiling. For Window=Balance it is the permitted overdraft (0 = none). When the
	// realm's limit row names a max_entitlement, Max is resolved per scope from that entitlement
	// quota — a plan, a per-customer override or the free tier sizes it, with provenance (D30).
	Max      int64
	Window   Window
	Dur      time.Duration // Rolling and Job
	TZ       string        // IANA zone for Daily/Hourly resets; default "UTC"
	WarnAt   float64       // 0..1 near-limit fraction; default 0.8, operator-configurable
	DenyCode string        // "payment_required" (402) | "limit_reached" (409)

	// MaxEntitlement names the entitlement key whose quota sizes Max for a scope (−1 there means
	// unlimited). It is configuration: the Meter resolves it through the QuotaSource BEFORE the
	// ledger sees the limit, so the ledger only ever evaluates a concrete Max.
	MaxEntitlement string
}

// Applies reports whether the limit governs a call for resource.
func (l Limit) Applies(resource string) bool { return l.Resource == "" || l.Resource == resource }

// Validate checks a limit's shape, mirroring the constraints on the limits table.
func (l Limit) Validate() error {
	switch {
	case l.Name == "":
		return fmt.Errorf("%w: a limit needs a name", ErrInvalid)
	case l.Max < 0:
		return fmt.Errorf("%w: limit %q has a negative max", ErrInvalid, l.Name)
	case l.Window < Balance || l.Window > Job:
		return fmt.Errorf("%w: limit %q has an unknown window", ErrInvalid, l.Name)
	case (l.Window == Rolling || l.Window == Job) && l.Dur <= 0:
		return fmt.Errorf("%w: limit %q (%s) needs a duration", ErrInvalid, l.Name, l.Window)
	case l.Window == Balance && (l.Subject != "" || l.Unit != CreditUnit):
		return fmt.Errorf("%w: balance limit %q applies to the scope, in credits", ErrInvalid, l.Name)
	case l.Window == Job && l.Subject != SubjectJob:
		return fmt.Errorf("%w: job limit %q must be keyed by the %q subject", ErrInvalid, l.Name, SubjectJob)
	case l.WarnAt < 0 || l.WarnAt > 1:
		return fmt.Errorf("%w: limit %q warn_at must be in [0,1]", ErrInvalid, l.Name)
	case l.DenyCode != DenyPaymentRequired && l.DenyCode != DenyLimitReached:
		return fmt.Errorf("%w: limit %q has unknown deny code %q", ErrInvalid, l.Name, l.DenyCode)
	}
	if l.Unit == "" {
		return fmt.Errorf("%w: limit %q needs a unit", ErrInvalid, l.Name)
	}
	if l.Window == Daily || l.Window == Hourly {
		if _, err := l.Location(); err != nil {
			return err
		}
	}
	return nil
}

// AdmitRequest is the admission-gate input.
type AdmitRequest struct {
	Resource string   // selects resource-scoped limits and the eligible pools
	Subjects Subjects // MUST cover every configured subject limit for this resource
	// Limits are caller-supplied ceilings, merged with the realm's configured ones. A caller can
	// only TIGHTEN: for the same Name the smaller Max wins. A per-run Job budget is passed here.
	Limits []Limit
	// MaxCost is the caller's UPPER BOUND on what the work it is about to start can cost. With it
	// the gate refuses when `eligible balance + overdraft − MaxCost < 0`, so the overshoot bound in
	// invariant 3 is ENFORCED rather than asserted. Still a gate, not a reservation: concurrent
	// admits can overshoot by (in-flight concurrency × MaxCost) — a bound the operator can compute.
	MaxCost Credits
}

// Admission is the gate decision. Allowed=false blocks BEFORE any expensive work.
type Admission struct {
	Allowed  bool
	Exceeded *Limit // which ceiling blocked (nil when Allowed)
	Warning  *Limit // a ceiling crossed WarnAt but not Max — drives the near-limit notification
	// Headroom is the remaining allowance on the binding credit-denominated limit, so a caller
	// can size the work (cap max_tokens) instead of starting work it cannot afford to finish.
	Headroom Credits
	Blocked  bool // held by block_and_flag pending an operator (deny code "account_blocked")
}

// MergeLimits merges the caller's limits into the realm's configured ones. A caller can only
// TIGHTEN: for the same Name the smaller Max wins and every other field stays the configured
// one; a caller limit whose Name is not configured is appended. Order is stable: configured
// limits first, in their given order, then new caller limits.
func MergeLimits(configured, caller []Limit) []Limit {
	out := make([]Limit, len(configured), len(configured)+len(caller))
	copy(out, configured)
	pos := make(map[string]int, len(out))
	for i, l := range out {
		pos[l.Name] = i
	}
	for _, c := range caller {
		if i, ok := pos[c.Name]; ok {
			if c.Max < out[i].Max {
				out[i].Max = c.Max
			}
			continue
		}
		pos[c.Name] = len(out)
		out = append(out, c)
	}
	return out
}

// Snapshot is what a hot tier reports for one admission: the state Evaluate needs and nothing else.
type Snapshot struct {
	// Eligible is the balance across the pools eligible for the resource (GPU-only credits do not
	// admit an LLM call). It may be negative.
	Eligible Credits
	Blocked  bool
	// Counters is the current value of each window limit's counter for this request's subject,
	// keyed by limit name. A missing entry is zero.
	Counters map[string]int64
}

// CheckSubjects refuses a call that omits a subject a configured limit needs (invariant 17). It
// looks only at limits that govern the resource.
func CheckSubjects(limits []Limit, resource string, subjects Subjects) error {
	for _, l := range limits {
		if l.Subject == "" || !l.Applies(resource) {
			continue
		}
		if subjects[l.Subject] == "" {
			return fmt.Errorf("%w: limit %q needs subject %q", ErrMissingSubject, l.Name, l.Subject)
		}
	}
	return nil
}

// Evaluate decides an admission from a snapshot. It is pure — no clock, no I/O — so every
// BalanceStore evaluates identically and the decision can be replayed.
//
// A Balance limit passes when `eligible + overdraft ≥ MaxCost`, and — with no MaxCost — when there
// is anything left at all. A window limit passes when `counter + MaxCost ≤ Max` for a
// credit-denominated counter, and `counter < Max` for a metered unit whose next quantity is not
// known. The first ceiling exceeded, in the order given, is reported.
func Evaluate(req AdmitRequest, limits []Limit, snap Snapshot) (Admission, error) {
	if err := CheckSubjects(limits, req.Resource, req.Subjects); err != nil {
		return Admission{}, err
	}
	if snap.Blocked {
		return Admission{
			Allowed: false, Blocked: true,
			Exceeded: &Limit{Name: DenyAccountBlocked, DenyCode: DenyAccountBlocked},
		}, nil
	}

	adm := Admission{Allowed: true}
	var headroom Credits
	haveHeadroom := false
	note := func(h Credits) {
		if h < 0 {
			h = 0
		}
		if !haveHeadroom || h < headroom {
			headroom, haveHeadroom = h, true
		}
	}

	for i := range limits {
		l := limits[i]
		if !l.Applies(req.Resource) {
			continue
		}
		switch l.Window {
		case Balance:
			avail, err := snap.Eligible.Add(Credits(l.Max))
			if err != nil {
				return Admission{}, err
			}
			note(avail)
			if avail < req.MaxCost || (req.MaxCost == 0 && avail <= 0) {
				if adm.Exceeded == nil {
					lc := l
					adm.Exceeded = &lc
				}
			}
		default:
			used := snap.Counters[l.Name]
			if l.Unit == CreditUnit {
				note(Credits(l.Max) - Credits(used))
			}
			cost := int64(0)
			if l.Unit == CreditUnit {
				cost = int64(req.MaxCost)
			}
			exceeded := used+cost > l.Max || (cost == 0 && used >= l.Max)
			switch {
			case exceeded:
				if adm.Exceeded == nil {
					lc := l
					adm.Exceeded = &lc
				}
			case l.Max > 0 && l.WarnAt > 0 && adm.Warning == nil &&
				float64(used+cost) >= l.WarnAt*float64(l.Max):
				lc := l
				adm.Warning = &lc
			}
		}
	}
	adm.Headroom = headroom
	adm.Allowed = adm.Exceeded == nil
	if !adm.Allowed {
		adm.Warning = nil
	}
	return adm, nil
}
