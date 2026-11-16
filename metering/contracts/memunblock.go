package contracts

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// MemBooksUnblockDefects are the deliberate faults a MemBooksUnblock can be given. BooksUnblockContract is shown to reject each by name
// (unblock_test.go): a clearer that clears a neighbour, or says it cleared what it did not, has not been shown to be tested.
type MemBooksUnblockDefects struct {
	ClearsTheWholeRealm bool // clears every scope of the realm, not the one named
	ForgetsTheRealm     bool // matches kind and id in any realm
	ReportsAlways       bool // says it cleared a block whether or not there was one
	InventsAWatermark   bool // writes a watermark for a scope the books had none for
	ResetsProgress      bool // zeroes the applied sequence as it clears
	NotAtomic           bool // reads, looks away, then writes: two callers both see the block
	IgnoresCancel       bool // clears for a caller who has gone
	AcceptsAnyScope     bool // does not check that the scope is one
}

// MemBooksUnblock is an in-memory ports.BookUnblocker.
type MemBooksUnblock struct {
	Defects MemBooksUnblockDefects
	mu      sync.Mutex
	marks   map[domain.Scope]ports.Watermark
}

var _ ports.BookUnblocker = (*MemBooksUnblock)(nil)

// NewMemBooksUnblock returns an empty, honest one.
func NewMemBooksUnblock() *MemBooksUnblock {
	return &MemBooksUnblock{marks: map[domain.Scope]ports.Watermark{}}
}

// ClearBlocked implements ports.BookUnblocker.
func (m *MemBooksUnblock) ClearBlocked(ctx context.Context, s domain.Scope) (bool, error) {
	if !m.Defects.AcceptsAnyScope {
		if err := s.Validate(); err != nil {
			return false, err
		}
	}
	if !m.Defects.IgnoresCancel {
		if err := ctx.Err(); err != nil {
			return false, err
		}
	}
	s = s.Normalized()
	m.mu.Lock()
	w, ok := m.marks[s]
	if m.Defects.ForgetsTheRealm && !ok {
		for k, v := range m.marks {
			if k.Kind == s.Kind && k.ID == s.ID {
				s, w, ok = k, v, true
				break
			}
		}
	}
	if !ok {
		defer m.mu.Unlock()
		if m.Defects.InventsAWatermark {
			m.marks[s] = ports.Watermark{Exists: true}
		}
		return m.Defects.ReportsAlways, nil
	}
	if m.Defects.NotAtomic {
		m.mu.Unlock()
		time.Sleep(2 * time.Millisecond)
		m.mu.Lock()
	}
	defer m.mu.Unlock()
	held := w.Blocked
	if !held && !m.Defects.ReportsAlways {
		return false, nil
	}
	w.Blocked = false
	if m.Defects.ResetsProgress {
		w.AppliedSeq = 0
	}
	m.marks[s] = w
	if m.Defects.ClearsTheWholeRealm {
		for k, v := range m.marks {
			if k.Realm == s.Realm {
				v.Blocked = false
				m.marks[k] = v
			}
		}
	}
	return held || m.Defects.ReportsAlways, nil
}

// World is the honest or broken clearer as a BooksUnblockWorld.
func (m *MemBooksUnblock) World() BooksUnblockWorld {
	return BooksUnblockWorld{
		Books: m,
		Put: func(_ testing.TB, s domain.Scope, w ports.Watermark) {
			m.mu.Lock()
			m.marks[s.Normalized()] = w
			m.mu.Unlock()
		},
		Get: func(_ testing.TB, s domain.Scope) ports.Watermark {
			m.mu.Lock()
			defer m.mu.Unlock()
			return m.marks[s.Normalized()]
		},
	}
}

// MemHotUnblockDefects are the deliberate faults a MemHotUnblock can be given. HotUnblockContract is shown to reject each by name.
type MemHotUnblockDefects struct {
	ClearsAStaleAccount bool // reaches into an account of an older generation and says it cleared it
	ClearsWhileFrozen   bool // clears in a frozen shard, with no complaint
	ReportsAlways       bool // says it cleared a block whether or not there was one
	MakesAnAccount      bool // brings an account into being for a scope that had none
	NotAtomic           bool // reads, looks away, then writes: two callers both see the block
	MovesTheSequence    bool // advances the account's sequence as it clears
	RefusesACold        bool // answers a scope with no account as an error
}

type memHotAccount struct {
	live, blocked bool
	seq           int64
	pools         map[domain.Pool]domain.Credits
}

// MemHotUnblock is an in-memory ports.HotUnblocker whose shards are its scopes: each scope has a shard of its own.
type MemHotUnblock struct {
	Defects MemHotUnblockDefects
	mu      sync.Mutex
	acct    map[domain.Scope]*memHotAccount
	stale   map[domain.Scope]bool
	frozen  map[domain.Scope]bool
}

var _ ports.HotUnblocker = (*MemHotUnblock)(nil)

// NewMemHotUnblock returns an empty, honest one.
func NewMemHotUnblock() *MemHotUnblock {
	return &MemHotUnblock{acct: map[domain.Scope]*memHotAccount{}, stale: map[domain.Scope]bool{}, frozen: map[domain.Scope]bool{}}
}

// ClearBlocked implements ports.HotUnblocker.
func (m *MemHotUnblock) ClearBlocked(_ context.Context, s domain.Scope) (bool, error) {
	if err := s.Validate(); err != nil {
		return false, err
	}
	s = s.Normalized()
	m.mu.Lock()
	if m.frozen[s] && !m.Defects.ClearsWhileFrozen {
		m.mu.Unlock()
		return false, domain.ErrShardFrozen
	}
	a, ok := m.acct[s]
	switch {
	case !ok || !a.live:
		if m.Defects.MakesAnAccount {
			m.acct[s] = &memHotAccount{live: true}
		}
		m.mu.Unlock()
		if m.Defects.RefusesACold {
			return false, domain.ErrColdScope
		}
		return m.Defects.ReportsAlways, nil
	case m.stale[s] && !m.Defects.ClearsAStaleAccount:
		m.mu.Unlock()
		return m.Defects.ReportsAlways, nil
	}
	held := a.blocked
	if m.Defects.NotAtomic {
		m.mu.Unlock()
		time.Sleep(2 * time.Millisecond)
		m.mu.Lock()
	}
	defer m.mu.Unlock()
	a.blocked = false
	if m.Defects.MovesTheSequence {
		a.seq++
	}
	return held || m.Defects.ReportsAlways, nil
}

// World is the honest or broken clearer as a HotUnblockWorld.
func (m *MemHotUnblock) World() HotUnblockWorld {
	return HotUnblockWorld{
		Hot:      m,
		NewScope: func(testing.TB) domain.Scope { return ubScope("t1") },
		Install: func(_ testing.TB, s domain.Scope, blocked bool) {
			m.mu.Lock()
			m.acct[s.Normalized()] = &memHotAccount{live: true, blocked: blocked, seq: 41, pools: map[domain.Pool]domain.Credits{"general": 500}}
			m.mu.Unlock()
		},
		Account: func(_ testing.TB, s domain.Scope) ports.HotAccount {
			m.mu.Lock()
			defer m.mu.Unlock()
			s = s.Normalized()
			a, ok := m.acct[s]
			if !ok || !a.live || m.stale[s] {
				return ports.HotAccount{Cold: true}
			}
			pools := make(map[domain.Pool]domain.Credits, len(a.pools))
			for p, v := range a.pools {
				pools[p] = v
			}
			return ports.HotAccount{Seq: a.seq, Blocked: a.blocked, Pools: pools}
		},
		Stale: func(_ testing.TB, s domain.Scope) {
			m.mu.Lock()
			m.stale[s.Normalized()] = true
			m.mu.Unlock()
		},
		Freeze: func(_ testing.TB, s domain.Scope, frozen bool) {
			m.mu.Lock()
			m.frozen[s.Normalized()] = frozen
			m.mu.Unlock()
		},
	}
}
