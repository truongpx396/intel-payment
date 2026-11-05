package contracts

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// MemDeadLetterDefects are the deliberate faults a MemDeadLetters can be given. DeadLetterContract is shown to reject each by
// name (deadletters_test.go): a suite that passes a store that parks twice, or forgets that a letter was replayed, has not been
// shown to test anything.
type MemDeadLetterDefects struct {
	ParkEveryCall      bool // Park ignores (Subject, SourceID) and parks every call as a new letter
	KeyOnSourceAlone   bool // the letter's identity is its SourceID: another subject's letter with the same id is "already parked"
	OverwriteOnRepark  bool // a later Park of the same letter replaces what the first said
	TrimPayload        bool // the payload loses its trailing zero bytes
	ForgetReplayed     bool // a finished replay can be claimed again
	IgnoreLiveClaim    bool // a claim succeeds while another replay's claim is live
	NeverLapse         bool // a claim whose lease has run out is never taken again
	KeepClaimOnRelease bool // ReleaseReplay does nothing
	DeleteOnFinish     bool // a finished replay deletes the letter
	UnreplayOnRelease  bool // ReleaseReplay after a finished replay un-replays it
	CountReplayed      bool // Open counts replayed letters too
	OmitEmptyRealms    bool // Open leaves out a realm with nothing open
}

type memLetter struct {
	l           ports.DeadLetter
	claimedTill time.Time
}

// MemDeadLetters is an in-memory ports.DeadLetterStore: the honest reference the contract is written against, and what the
// use case's own tests run on.
type MemDeadLetters struct {
	Defects MemDeadLetterDefects

	mu      sync.Mutex
	clock   ports.Clock
	n       int
	letters map[string]*memLetter
	keys    map[[2]string]string
}

var _ ports.DeadLetterStore = (*MemDeadLetters)(nil)

// NewMemDeadLetters builds a store that reads time from clock.
func NewMemDeadLetters(clock ports.Clock) *MemDeadLetters {
	return &MemDeadLetters{clock: clock, letters: map[string]*memLetter{}, keys: map[[2]string]string{}}
}

func (m *MemDeadLetters) Park(_ context.Context, l ports.DeadLetter) (ports.DeadLetter, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := [2]string{l.Subject, l.SourceID}
	if m.Defects.KeyOnSourceAlone {
		key = [2]string{"", l.SourceID}
	}
	if id, ok := m.keys[key]; ok && !m.Defects.ParkEveryCall {
		if m.Defects.OverwriteOnRepark {
			p := m.letters[id]
			p.l.Payload, p.l.Attempts, p.l.LastError = append([]byte(nil), l.Payload...), l.Attempts, l.LastError
		}
		return clone(m.letters[id].l), false, nil
	}
	m.n++
	l.ID = fmt.Sprintf("00000000-0000-4000-8000-%012d", m.n)
	l.Payload = append([]byte(nil), l.Payload...)
	if m.Defects.TrimPayload {
		for len(l.Payload) > 0 && l.Payload[len(l.Payload)-1] == 0 {
			l.Payload = l.Payload[:len(l.Payload)-1]
		}
	}
	l.ParkedAt, l.ReplayedAt = m.clock.Now(), nil
	m.letters[l.ID] = &memLetter{l: l}
	m.keys[key] = l.ID
	return clone(l), true, nil
}

func (m *MemDeadLetters) Get(_ context.Context, id string) (ports.DeadLetter, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.letters[id]
	if !ok {
		return ports.DeadLetter{}, fmt.Errorf("%w: dead letter %s", domain.ErrNotFound, id)
	}
	return clone(p.l), nil
}

func (m *MemDeadLetters) ClaimReplay(_ context.Context, id string, lease time.Duration) (ports.DeadLetter, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.letters[id]
	if !ok {
		return ports.DeadLetter{}, false, fmt.Errorf("%w: dead letter %s", domain.ErrNotFound, id)
	}
	now := m.clock.Now()
	if p.l.ReplayedAt != nil && !m.Defects.ForgetReplayed {
		return clone(p.l), false, nil
	}
	live := !p.claimedTill.IsZero() && (m.Defects.NeverLapse || now.Before(p.claimedTill))
	if live && !m.Defects.IgnoreLiveClaim {
		return clone(p.l), false, nil
	}
	p.claimedTill = now.Add(lease)
	return clone(p.l), true, nil
}

func (m *MemDeadLetters) FinishReplay(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.letters[id]
	if !ok {
		return fmt.Errorf("%w: dead letter %s", domain.ErrNotFound, id)
	}
	if m.Defects.DeleteOnFinish {
		delete(m.letters, id)
		return nil
	}
	if p.l.ReplayedAt == nil {
		now := m.clock.Now()
		p.l.ReplayedAt = &now
	}
	return nil
}

func (m *MemDeadLetters) ReleaseReplay(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.letters[id]
	if !ok {
		return fmt.Errorf("%w: dead letter %s", domain.ErrNotFound, id)
	}
	if m.Defects.UnreplayOnRelease {
		p.l.ReplayedAt = nil
	}
	if !m.Defects.KeepClaimOnRelease {
		p.claimedTill = time.Time{}
	}
	return nil
}

func (m *MemDeadLetters) Open(context.Context) (map[string]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int64{}
	for _, p := range m.letters {
		if p.l.ReplayedAt == nil || m.Defects.CountReplayed {
			out[p.l.Realm]++
		} else if _, seen := out[p.l.Realm]; !seen && !m.Defects.OmitEmptyRealms {
			out[p.l.Realm] = 0
		}
	}
	return out, nil
}

func clone(l ports.DeadLetter) ports.DeadLetter {
	l.Payload = append([]byte(nil), l.Payload...)
	if l.ReplayedAt != nil {
		t := *l.ReplayedAt
		l.ReplayedAt = &t
	}
	return l
}
