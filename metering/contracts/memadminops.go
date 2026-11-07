package contracts

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// MemAdminOpDefects are the deliberate faults a MemAdminOps can be given. AdminOpContract is shown to reject each by name
// (adminops_test.go): a queue that makes an adjustment twice, hands one operation to two workers or records an outcome for a worker that
// lost its claim has not been shown to be tested.
type MemAdminOpDefects struct {
	NewOperationOnReplay bool // Submit makes a new operation for every call, whatever the key
	IgnoresRealmInKey    bool // a key is global: another realm's request under it is the first realm's operation
	ReadsAcrossRealms    bool // Get ignores the realm it is asked in
	AcceptsKeyReuse      bool // a key reused for a different request returns the first operation instead of a conflict
	ClaimsNewestFirst    bool // Claim takes the newest eligible operation
	ClaimsLiveLease      bool // Claim takes an operation another worker holds under a live lease
	NeverLapses          bool // a running operation is never claimed again, whatever its lease
	ForgetsAttempts      bool // Claim does not count the attempt
	FinishesAnyAttempt   bool // Finish records an outcome for an attempt that no longer holds the claim
	FinishesTwice        bool // Finish records an outcome for an operation that already has one
	RetriesFailed        bool // a failed operation is claimed again
	CountsDoneInBacklog  bool // Backlog counts finished operations as waiting
	DropsResult          bool // Finish keeps the status and loses the result
	AnyLease             bool // Claim accepts a lease that is not positive
}

type memAdminOp struct {
	op        ports.AdminOperation
	seq       int
	leaseTill time.Time
}

// MemAdminOps is an in-memory ports.AdminOperationStore: the honest reference the contract is written against, and what the use case's
// own tests run on.
type MemAdminOps struct {
	Defects MemAdminOpDefects

	mu    sync.Mutex
	clock ports.Clock
	n     int
	byID  map[string]*memAdminOp
	byKey map[string]string
}

var _ ports.AdminOperationStore = (*MemAdminOps)(nil)

// NewMemAdminOps builds a queue that reads time from clock.
func NewMemAdminOps(clock ports.Clock) *MemAdminOps {
	return &MemAdminOps{clock: clock, byID: map[string]*memAdminOp{}, byKey: map[string]string{}}
}

func (m *MemAdminOps) keyOf(realm, key string) string {
	if m.Defects.IgnoresRealmInKey {
		return key
	}
	return realm + "|" + key
}

func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return string(a) == string(b)
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return string(xa) == string(ya)
}

func cloneOp(o ports.AdminOperation) ports.AdminOperation {
	o.Params = append(json.RawMessage(nil), o.Params...)
	o.Result = append(json.RawMessage(nil), o.Result...)
	if o.StartedAt != nil {
		t := *o.StartedAt
		o.StartedAt = &t
	}
	if o.FinishedAt != nil {
		t := *o.FinishedAt
		o.FinishedAt = &t
	}
	return o
}

func (m *MemAdminOps) Submit(_ context.Context, op ports.AdminOperation) (ports.AdminOperation, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := m.keyOf(op.Realm, op.Key)
	if id, ok := m.byKey[k]; ok && !m.Defects.NewOperationOnReplay {
		have := m.byID[id].op
		if have.Kind != op.Kind || have.Actor != op.Actor || have.Reason != op.Reason || !sameJSON(have.Params, op.Params) {
			if m.Defects.AcceptsKeyReuse {
				return cloneOp(have), false, nil
			}
			return cloneOp(have), false, fmt.Errorf("%w: key %q was used for a different request", domain.ErrIdemConflict, op.Key)
		}
		return cloneOp(have), false, nil
	}
	m.n++
	op.ID = fmt.Sprintf("00000000-0000-4000-8000-%012d", m.n)
	op.Status, op.Attempts, op.CreatedAt = ports.AdminPending, 0, m.clock.Now()
	op.StartedAt, op.FinishedAt, op.Result, op.Error = nil, nil, nil, ""
	op.Params = append(json.RawMessage(nil), op.Params...)
	m.byID[op.ID] = &memAdminOp{op: op, seq: m.n}
	m.byKey[k] = op.ID
	return cloneOp(op), true, nil
}

func (m *MemAdminOps) Get(_ context.Context, realm, id string) (ports.AdminOperation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.byID[id]
	if !ok || (o.op.Realm != realm && !m.Defects.ReadsAcrossRealms) {
		return ports.AdminOperation{}, fmt.Errorf("%w: operation %q", domain.ErrNotFound, id)
	}
	return cloneOp(o.op), nil
}

func (m *MemAdminOps) eligible(o *memAdminOp, now time.Time) bool {
	switch o.op.Status {
	case ports.AdminPending:
		return true
	case ports.AdminRunning:
		if m.Defects.ClaimsLiveLease {
			return true
		}
		return !m.Defects.NeverLapses && !now.Before(o.leaseTill)
	case ports.AdminFailed:
		return m.Defects.RetriesFailed
	}
	return false
}

func (m *MemAdminOps) Claim(_ context.Context, lease time.Duration) (ports.AdminOperation, bool, error) {
	if lease <= 0 && !m.Defects.AnyLease {
		return ports.AdminOperation{}, false, fmt.Errorf("%w: a lease is a positive duration, got %v", domain.ErrInvalid, lease)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock.Now()
	var pick *memAdminOp
	for _, o := range m.byID {
		if !m.eligible(o, now) {
			continue
		}
		older := pick == nil || o.seq < pick.seq
		if m.Defects.ClaimsNewestFirst {
			older = pick == nil || o.seq > pick.seq
		}
		if older {
			pick = o
		}
	}
	if pick == nil {
		return ports.AdminOperation{}, false, nil
	}
	if !m.Defects.ForgetsAttempts {
		pick.op.Attempts++
	} else {
		pick.op.Attempts = 1
	}
	started := now
	pick.op.Status, pick.op.StartedAt, pick.leaseTill = ports.AdminRunning, &started, now.Add(lease)
	return cloneOp(pick.op), true, nil
}

func (m *MemAdminOps) Finish(_ context.Context, id string, attempt int, status ports.AdminOperationStatus, result json.RawMessage, errText string) (bool, error) {
	if status != ports.AdminSucceeded && status != ports.AdminFailed {
		return false, fmt.Errorf("%w: an outcome is succeeded or failed, got %q", domain.ErrInvalid, status)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.byID[id]
	if !ok {
		return false, fmt.Errorf("%w: operation %q", domain.ErrNotFound, id)
	}
	if o.op.Status != ports.AdminRunning && !m.Defects.FinishesTwice {
		return false, nil
	}
	if o.op.Attempts != attempt && !m.Defects.FinishesAnyAttempt {
		return false, nil
	}
	now := m.clock.Now()
	o.op.Status, o.op.FinishedAt, o.op.Error = status, &now, errText
	o.op.Result = append(json.RawMessage(nil), result...)
	if m.Defects.DropsResult {
		o.op.Result = nil
	}
	return true, nil
}

func (m *MemAdminOps) Backlog(context.Context) (ports.AdminBacklog, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock.Now()
	var waiting []time.Time
	for _, o := range m.byID {
		wait := o.op.Status == ports.AdminPending || (o.op.Status == ports.AdminRunning && !now.Before(o.leaseTill))
		if m.Defects.CountsDoneInBacklog && (o.op.Status == ports.AdminSucceeded || o.op.Status == ports.AdminFailed) {
			wait = true
		}
		if wait {
			waiting = append(waiting, o.op.CreatedAt)
		}
	}
	b := ports.AdminBacklog{Waiting: int64(len(waiting))}
	if len(waiting) > 0 {
		sort.Slice(waiting, func(i, j int) bool { return waiting[i].Before(waiting[j]) })
		b.Oldest = now.Sub(waiting[0])
	}
	return b, nil
}
