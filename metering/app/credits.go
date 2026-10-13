package app

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

var _ ports.Service = (*meter)(nil)

// Credits builds the credits snapshot (FR-036): the whole of what a credits panel draws, in one read.
//
// It is built from the SAME limits Admit enforces — sized by the same QuotaSource — so the cap on the panel
// is the cap that refuses a call. A balance that is cold is rebuilt from the books first, as Admit does;
// an outage of the balance, the counters or the ledger is an error, never a half-built snapshot.
//
// A member's snapshot (SubjectKind and SubjectID) holds the scope-wide ceilings and that member's own, with
// that member's counters, and only the rows that member's spend produced: a member must not learn colleagues'
// usage from a credits screen. The unscoped view has only the scope's own ceilings — a member's ceiling has no
// single figure for the scope.
func (m *meter) Credits(ctx context.Context, q ports.SnapshotQuery) (ports.CreditsSnapshot, error) {
	if err := q.Scope.Validate(); err != nil {
		return ports.CreditsSnapshot{}, err
	}
	if (q.SubjectKind == "") != (q.SubjectID == "") {
		return ports.CreditsSnapshot{}, fmt.Errorf("%w: a member's snapshot needs both subject_kind and subject_id", domain.ErrInvalid)
	}
	if m.d.Usage == nil || m.d.Ledger == nil {
		return ports.CreditsSnapshot{}, errors.New("metering: the credits snapshot needs Deps.Usage and Deps.Ledger: it cannot be whole without the counters and the books")
	}
	s := q.Scope.Normalized()
	member := q.SubjectKind != ""

	bal, err := m.Balance(ctx, s)
	if err != nil {
		return ports.CreditsSnapshot{}, err
	}
	defs, err := m.d.Pools.Pools(ctx, s.Realm)
	if err != nil {
		return ports.CreditsSnapshot{}, fmt.Errorf("metering: realm pools: %w", err)
	}
	all, err := m.d.Limits.Limits(ctx, s.Realm)
	if err != nil {
		return ports.CreditsSnapshot{}, fmt.Errorf("metering: realm limits: %w", err)
	}
	limits, _, err := m.sizeLimits(ctx, s, all, func(domain.Limit) bool { return true })
	if err != nil {
		return ports.CreditsSnapshot{}, err
	}

	view, remaining, err := poolViews(defs, bal)
	if err != nil {
		return ports.CreditsSnapshot{}, err
	}

	// The ceilings this view shows, and the ones with a counter to read.
	var shown, counted []domain.Limit
	for _, l := range limits {
		switch {
		case l.Window == domain.Balance, l.Subject == "":
			shown = append(shown, l)
		case member && l.Subject == q.SubjectKind:
			shown = append(shown, l)
		default:
			continue // a member's ceiling in the scope's view, another kind's, or a job's budget
		}
		if l.Window != domain.Balance {
			counted = append(counted, l)
		}
	}
	var used map[string]int64
	if len(counted) > 0 {
		var subjects domain.Subjects
		if member {
			subjects = domain.Subjects{q.SubjectKind: q.SubjectID}
		}
		err := m.reh.heal(ctx, s, func() (e error) { used, e = m.d.Usage.Usage(ctx, s, subjects, counted); return })
		if err != nil {
			return ports.CreditsSnapshot{}, err
		}
	}
	snap := ports.CreditsSnapshot{Scope: s, Balance: ports.BalanceView{Remaining: remaining, Pools: view}}
	now := m.d.Clock.Now()
	for _, l := range shown {
		snap.Limits = append(snap.Limits, limitView(l, used, remaining, now, member))
	}
	snap.Balance.PrimaryLimitKey = primaryLimit(shown)

	page, err := m.d.Ledger.LedgerRows(ctx, ports.LedgerQuery{
		Scope: s, SubjectKind: q.SubjectKind, SubjectID: q.SubjectID, After: q.LedgerAfter, Limit: q.LedgerLimit,
	})
	if err != nil {
		return ports.CreditsSnapshot{}, err
	}
	snap.Ledger, snap.LedgerNext = page.Rows, page.Next
	if snap.Balance.Granted, err = m.d.Ledger.Granted(ctx, s); err != nil {
		return ports.CreditsSnapshot{}, err
	}
	return snap, nil
}

// poolViews lists the realm's pools in draw order — by priority then name, general after them — and then any
// pool that holds credits but is no longer configured, so the parts always sum to the total. remaining IS that
// sum, computed with overflow refused rather than wrapped.
func poolViews(defs []domain.PoolDef, bal map[domain.Pool]domain.Credits) ([]ports.PoolView, domain.Credits, error) {
	configured := make([]domain.PoolDef, 0, len(defs))
	known := map[domain.Pool]bool{domain.GeneralPool: true}
	for _, d := range defs {
		if d.Name != domain.GeneralPool {
			configured = append(configured, d)
			known[d.Name] = true
		}
	}
	sort.SliceStable(configured, func(i, j int) bool {
		if configured[i].Priority != configured[j].Priority {
			return configured[i].Priority < configured[j].Priority
		}
		return configured[i].Name < configured[j].Name
	})
	var strays []domain.Pool
	for p := range bal {
		if !known[p] {
			strays = append(strays, p)
		}
	}
	sort.Slice(strays, func(i, j int) bool { return strays[i] < strays[j] })

	var out []ports.PoolView
	var total domain.Credits
	add := func(key domain.Pool, applies []string) error {
		v := bal[key]
		var err error
		if total, err = total.Add(v); err != nil {
			return fmt.Errorf("metering: balance total: %v", err) //nolint:errorlint // deliberately not wrapped: ErrAmountOutOfRange would blame the caller
		}
		out = append(out, ports.PoolView{Key: string(key), Remaining: v, AppliesTo: strings.Join(applies, ",")})
		return nil
	}
	for _, d := range configured {
		if err := add(d.Name, d.AppliesTo); err != nil {
			return nil, 0, err
		}
	}
	if err := add(domain.GeneralPool, nil); err != nil {
		return nil, 0, err
	}
	for _, p := range strays {
		if err := add(p, nil); err != nil {
			return nil, 0, err
		}
	}
	return out, total, nil
}

// windowView is the kernel's Window in the view's vocabulary — the one place the two are mapped.
var windowView = map[domain.Window]string{
	domain.Balance: "none", domain.Daily: "day", domain.Hourly: "hour", domain.Rolling: "period", domain.Job: "job",
}

func limitView(l domain.Limit, used map[string]int64, remaining domain.Credits, now time.Time, member bool) ports.LimitView {
	ceiling := l.Max
	v := ports.LimitView{Key: l.Name, Cap: &ceiling, Window: windowView[l.Window], WarnAtPct: int(math.Round(l.WarnAt * 100)), DenyCode: l.DenyCode}
	if l.Window == domain.Balance {
		// A balance has no counter: what it has "used" is the overdraft it is into.
		switch {
		case remaining < -math.MaxInt64:
			v.Used = math.MaxInt64
		case remaining < 0:
			v.Used = int64(-remaining)
		}
	} else {
		v.Used = used[l.Name]
	}
	if at, ok := l.ResetsAt(now); ok {
		v.ResetsAt = &at
	}
	if member {
		v.ScopeHint = "all members"
		if l.Subject != "" {
			v.ScopeHint = "you"
		}
	}
	return v
}

// primaryLimit is the limit the hero mirrors: the first balance, else the first ceiling shown.
func primaryLimit(shown []domain.Limit) string {
	for _, l := range shown {
		if l.Window == domain.Balance {
			return l.Name
		}
	}
	if len(shown) > 0 {
		return shown[0].Name
	}
	return ""
}
