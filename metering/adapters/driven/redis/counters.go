package redis

import (
	"fmt"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// Window counters (metering-ports.md § Window counters). This file decides, purely, WHICH counter
// keys a settlement increments and a gate reads, so it is unit-testable without Redis and the
// one-slot property can be asserted directly.

// counterIncr is one INCRBY (with a PEXPIRE ... NX) inside ip_debit.
type counterIncr struct {
	key string
	by  int64
	ttl time.Duration
}

// jobLimitKey is the limit segment of a job counter: one per unit, independent of any limit's name,
// because a per-run budget is passed at Admit while settlement sees only Subjects["job"].
func jobLimitKey(u domain.Unit) string { return "job." + string(u) }

// subjectID is the counter's subject segment: the subject id for a subject limit, "-" for the scope.
func subjectID(l domain.Limit, subjects domain.Subjects) string {
	if l.Subject == "" {
		return "-"
	}
	return subjects[l.Subject]
}

// amountFor is what a charge adds to a counter measured in unit u.
func amountFor(c domain.Charge, u domain.Unit) int64 {
	if u == domain.CreditUnit {
		return int64(c.Amount)
	}
	var n int64
	for _, q := range c.Quantities {
		if q.Unit == u {
			n += q.Amount
		}
	}
	return n
}

// planIncrements lists every counter a charge increments: the configured daily/hourly/rolling
// limits that govern it, and — whenever the charge carries a job — the job counters in credits and in
// each metered unit. A zero increment is skipped: it would only write a key.
func planIncrements(sh domain.Shard, limits []domain.Limit, c domain.Charge, now time.Time) ([]counterIncr, error) {
	var out []counterIncr
	for _, l := range limits {
		if l.Window == domain.Balance || l.Window == domain.Job || !l.Applies(c.Resource) {
			continue
		}
		if l.Subject != "" && c.Subjects[l.Subject] == "" {
			continue // the gate refuses such a call; a charge that got here counts what it can
		}
		by := amountFor(c, l.Unit)
		if by == 0 {
			continue
		}
		b, err := l.WriteBucket(now)
		if err != nil {
			return nil, err
		}
		out = append(out, counterIncr{ctrKey(sh, c.Scope, l.Name, subjectID(l, c.Subjects), b.Key), by, b.TTL})
	}
	if job := c.Subjects[domain.SubjectJob]; job != "" {
		units := []domain.Unit{domain.CreditUnit}
		seen := map[domain.Unit]bool{}
		for _, q := range c.Quantities {
			if !seen[q.Unit] {
				seen[q.Unit] = true
				units = append(units, q.Unit)
			}
		}
		for _, u := range units {
			if by := amountFor(c, u); by != 0 {
				out = append(out, counterIncr{ctrKey(sh, c.Scope, jobLimitKey(u), job, "-"), by, domain.DefaultJobTTL})
			}
		}
	}
	return out, nil
}

// counterRead is one limit's counter to sum at admission: the keys of its buckets.
type counterRead struct {
	limit string   // the Limit.Name whose counter this is
	keys  []string // one per bucket; the counter is their sum
}

// planReads lists the counters an admission must read. A limit that cannot be counted (a caller's
// window limit no configured limit backs, checked by the Meter) still reads its key — it is simply
// zero. Balance limits read no counter.
func planReads(sh domain.Shard, s domain.Scope, limits []domain.Limit, req domain.AdmitRequest, now time.Time) ([]counterRead, error) {
	var out []counterRead
	for _, l := range limits {
		if l.Window == domain.Balance || !l.Applies(req.Resource) {
			continue
		}
		subj := subjectID(l, req.Subjects)
		if l.Subject != "" && subj == "" {
			return nil, fmt.Errorf("%w: limit %q needs subject %q", domain.ErrMissingSubject, l.Name, l.Subject)
		}
		buckets, err := l.ReadBuckets(now)
		if err != nil {
			return nil, err
		}
		limitKey := l.Name
		if l.Window == domain.Job {
			limitKey = jobLimitKey(l.Unit)
		}
		keys := make([]string, len(buckets))
		for i, b := range buckets {
			keys[i] = ctrKey(sh, s, limitKey, subj, b)
		}
		out = append(out, counterRead{limit: l.Name, keys: keys})
	}
	return out, nil
}
