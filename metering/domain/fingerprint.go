package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"time"
)

// A fingerprint is SHA-256 over the canonical form of an operation. The hot guard and the durable
// guard both store it, so "the same key with a different request" is ErrIdemConflict on either
// side rather than a silent no-op (invariant 2, D32).
//
// The canonical form is length-prefixed, so no choice of field values can make two different
// operations encode identically, and it covers what IDENTIFIES the operation — never the audit-only
// data (Ref, Attributes) that a retry may legitimately change, such as a fresh trace id.

type canon struct{ b []byte }

func (c *canon) s(v string) *canon {
	c.b = strconv.AppendInt(c.b, int64(len(v)), 10)
	c.b = append(c.b, ':')
	c.b = append(c.b, v...)
	return c
}
func (c *canon) i(v int64) *canon { return c.s(strconv.FormatInt(v, 10)) }
func (c *canon) t(v time.Time) *canon {
	if v.IsZero() {
		return c.s("")
	}
	return c.s(v.UTC().Format(time.RFC3339Nano))
}
func (c *canon) sum() string {
	h := sha256.Sum256(c.b)
	return hex.EncodeToString(h[:])
}

func (c *canon) quantities(qs []Quantity) *canon {
	// A multiset: the pricer sums quantities, so no order of them is a different request — not even
	// when one unit is named twice, which is why the amount breaks the tie.
	sorted := append([]Quantity(nil), qs...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Unit != sorted[j].Unit {
			return sorted[i].Unit < sorted[j].Unit
		}
		return sorted[i].Amount < sorted[j].Amount
	})
	c.i(int64(len(sorted)))
	for _, q := range sorted {
		c.s(string(q.Unit)).i(q.Amount)
	}
	return c
}

func (c *canon) subjects(s Subjects) *canon {
	kinds := s.Kinds()
	c.i(int64(len(kinds)))
	for _, k := range kinds {
		c.s(k).s(s[k])
	}
	return c
}

// Fingerprint identifies a usage charge. The amount is part of it: a reused key with a different
// price is a different request. (A retry after a rate card was published in between therefore
// conflicts rather than silently keeping the first price — see the Meter's handling.)
func (c Charge) Fingerprint() string {
	return (&canon{}).s("usage").s(c.Scope.Tag()).i(int64(c.Amount)).s(c.Reason).s(c.Resource).
		s(c.RateKey).quantities(c.Quantities).subjects(c.Subjects).t(c.OccurredAt).sum()
}

// Fingerprint identifies a grant.
func (g Grant) Fingerprint() string {
	c := (&canon{}).s("grant").s(g.Scope.Tag()).s(string(g.Pool.Or())).i(int64(g.Amount)).s(g.Reason)
	if g.ExpiresAt != nil {
		c.t(*g.ExpiresAt)
	} else {
		c.s("")
	}
	return c.sum()
}

// Fingerprint identifies a transfer.
func (t Transfer) Fingerprint() string {
	return (&canon{}).s("transfer").s(t.From.Tag()).s(string(t.FromPool.Or())).s(t.To.Tag()).
		s(string(t.ToPool.Or())).i(int64(t.Amount)).s(t.Reason).i(int64(t.MaxDestBalance)).sum()
}

// CorrectionFingerprint identifies a writer-issued hot mutation (transfer_in, correction, expiry),
// which is idempotent on its own key.
func CorrectionFingerprint(op IntentOp, scope Scope, pool Pool, delta Credits, key string) string {
	return (&canon{}).s(string(op)).s(scope.Tag()).s(string(pool.Or())).i(int64(delta)).s(key).sum()
}
