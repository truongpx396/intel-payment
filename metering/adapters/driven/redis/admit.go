package redis

import (
	"context"
	"fmt"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// Balance returns the HOT balance per pool. A scope whose account is absent or was built in an
// older shard generation is ErrColdScope: the caller rehydrates it from the books and asks again,
// because "zero" would be a guess.
func (s *Store) Balance(ctx context.Context, sc domain.Scope) (map[domain.Pool]domain.Credits, error) {
	acct, err := s.Snapshot(ctx, sc)
	if err != nil {
		return nil, err
	}
	if acct.Cold {
		return nil, domain.ErrColdScope
	}
	return acct.Pools, nil
}

// Snapshot reads an account as one atomic read (ip_admit with no counters).
func (s *Store) Snapshot(ctx context.Context, sc domain.Scope) (ports.HotAccount, error) {
	ctx, cancel := s.withTimeout(ctx, s.cfg.AdmitTimeout)
	defer cancel()
	sc = sc.Normalized()
	sh := s.shard(sc)
	r, err := s.call(ctx, "ip_admit", true, []string{metaKey(sh), acctKey(sh, sc)})
	if err != nil {
		return ports.HotAccount{}, err
	}
	switch r.status() {
	case "OK":
	case "COLD_SCOPE":
		return ports.HotAccount{Cold: true}, nil
	default:
		return ports.HotAccount{}, statusError(r.status())
	}
	return parseAccount(r)
}

func parseAccount(r reply) (ports.HotAccount, error) {
	seq, err := r.num(1)
	if err != nil {
		return ports.HotAccount{}, err
	}
	blocked, err := r.num(2)
	if err != nil {
		return ports.HotAccount{}, err
	}
	if len(r) < 4 {
		return ports.HotAccount{}, fmt.Errorf("redis: short ip_admit reply %#v", []any(r))
	}
	pools, err := poolsOf(r[3])
	if err != nil {
		return ports.HotAccount{}, err
	}
	return ports.HotAccount{Seq: seq, Blocked: blocked == 1, Pools: pools}, nil
}

// Admit is the admission GATE: one read of the account and every relevant counter (one function
// call, one slot), then domain.Evaluate. It reserves nothing.
func (s *Store) Admit(ctx context.Context, sc domain.Scope, req domain.AdmitRequest) (domain.Admission, error) {
	if err := sc.Validate(); err != nil {
		return domain.Admission{}, err
	}
	if req.MaxCost < 0 {
		return domain.Admission{}, fmt.Errorf("%w: max_cost must be >= 0", domain.ErrInvalid)
	}
	sc = sc.Normalized()
	// Refuse a missing subject before any I/O: it is a caller error, not a hot-tier state.
	if err := domain.CheckSubjects(req.Limits, req.Resource, req.Subjects); err != nil {
		return domain.Admission{}, err
	}
	ctx, cancel := s.withTimeout(ctx, s.cfg.AdmitTimeout)
	defer cancel()

	sh := s.shard(sc)
	reads, err := planReads(sh, sc, req.Limits, req, s.clock.Now())
	if err != nil {
		return domain.Admission{}, err
	}
	keys := []string{metaKey(sh), acctKey(sh, sc)}
	for _, cr := range reads {
		keys = append(keys, cr.keys...)
	}
	r, err := s.call(ctx, "ip_admit", true, keys)
	if err != nil {
		return domain.Admission{}, err
	}
	if r.status() != "OK" {
		return domain.Admission{}, statusError(r.status())
	}
	acct, err := parseAccount(r)
	if err != nil {
		return domain.Admission{}, err
	}
	if len(r) < 5 {
		return domain.Admission{}, fmt.Errorf("redis: short ip_admit reply %#v", []any(r))
	}
	vals, ok := r[4].([]any)
	if !ok || len(vals) != len(keys)-2 {
		return domain.Admission{}, fmt.Errorf("redis: ip_admit returned %d counters for %d keys", len(vals), len(keys)-2)
	}

	// Sum each limit's buckets back together.
	counters := make(map[string]int64, len(reads))
	i := 0
	for _, cr := range reads {
		var sum int64
		for range cr.keys {
			n, err := toInt(vals[i])
			if err != nil {
				return domain.Admission{}, err
			}
			sum += n
			i++
		}
		counters[cr.limit] = sum
	}

	pools, err := s.eligible(ctx, sc.Realm, req.Resource)
	if err != nil {
		return domain.Admission{}, err
	}
	var eligible domain.Credits
	for _, p := range pools {
		if eligible, err = eligible.Add(acct.Pools[p]); err != nil {
			return domain.Admission{}, err
		}
	}
	return domain.Evaluate(req, req.Limits, domain.Snapshot{Eligible: eligible, Blocked: acct.Blocked, Counters: counters})
}
