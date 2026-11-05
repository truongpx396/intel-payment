package contracts

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/truongpx396/intel-payment/events"
)

// Clock is a clock that moves only when told to.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock starts at t.
func NewClock(t time.Time) *Clock { return &Clock{now: t} }

// Now implements the clock port.
func (c *Clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

// Defects are the deliberate faults a MemStore can be given. StoreContract is shown to reject each by name: a suite that passes a store
// that hands an endpoint another realm's events has not been shown to test anything.
type Defects struct {
	AppendEveryCall   bool // the source key is ignored: an event recorded twice is two events
	UnorderedIDs      bool // ids are issued without looking at the last: two in a millisecond may sort either way
	FeedIgnoresRealm  bool // the feed returns every realm's events
	FeedIgnoresTypes  bool // the types filter is ignored
	FeedInclusive     bool // the cursor is included in the page after it
	FeedBackwards     bool // newest first
	FanOutInactive    bool // an endpoint that is switched off is still owed deliveries
	FanOutAnyRealm    bool // an endpoint is owed another realm's events
	FanOutAnyType     bool // an endpoint is owed types it did not subscribe to
	NoLease           bool // a claim does not lease: the same delivery is handed to every claimant
	DeliveredStaysDue bool // a delivered delivery is claimable again
	DeadStaysDue      bool // a dead delivery is claimable again
	RedeliverAnything bool // a delivery that is not dead can be re-driven
	ExpireNothing     bool // pending deliveries older than the window stay pending
	PurgeEverything   bool // an event something is still owed for is purged
	PurgeNothing      bool // nothing is ever purged
	DropData          bool // the event's data is not kept
}

type memDelivery struct {
	id        string
	eventID   string
	endpoint  string
	status    string
	attempts  int
	next      time.Time
	createdAt time.Time
}

// MemStore is an in-memory events.Store: the honest reference the contract is written against.
type MemStore struct {
	Defects Defects

	mu         sync.Mutex
	clock      *Clock
	events     []events.Event // in the order appended
	sources    map[string]string
	endpoints  map[string]events.Endpoint
	deliveries []*memDelivery
	n          int
}

var _ events.Store = (*MemStore)(nil)

// NewMemStore builds a store that reads time from clock.
func NewMemStore(clock *Clock) *MemStore {
	return &MemStore{clock: clock, sources: map[string]string{}, endpoints: map[string]events.Endpoint{}}
}

// AddEndpoint implements Seeder.
func (m *MemStore) AddEndpoint(_ context.Context, ep events.Endpoint) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.endpoints[ep.ID] = ep
	return ep.ID, nil
}

// SetActive implements Seeder.
func (m *MemStore) SetActive(_ context.Context, id string, active bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ep := m.endpoints[id]
	ep.Active = active
	m.endpoints[id] = ep
	return nil
}

func (m *MemStore) Append(_ context.Context, e events.Event, source string) (events.Event, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, ok := m.sources[source]; ok && !m.Defects.AppendEveryCall {
		for _, x := range m.events {
			if x.ID == id {
				return x, false, nil
			}
		}
	}
	last := ""
	if n := len(m.events); n > 0 && !m.Defects.UnorderedIDs {
		last = m.events[n-1].ID
	}
	id, err := events.NextID(last, m.clock.Now(), rand.Reader)
	if err != nil {
		return events.Event{}, false, err
	}
	e.ID, e.CreatedAt = id, m.clock.Now()
	if m.Defects.DropData {
		e.Data = nil
	}
	m.events = append(m.events, e)
	m.sources[source] = id
	for _, ep := range m.endpoints {
		ok := ep.Subscribes(e.Realm, e.Type)
		if m.Defects.FanOutInactive && ep.Realm == e.Realm && contains(ep.Types, e.Type) {
			ok = true
		}
		if m.Defects.FanOutAnyRealm && ep.Active && contains(ep.Types, e.Type) {
			ok = true
		}
		if m.Defects.FanOutAnyType && ep.Active && ep.Realm == e.Realm {
			ok = true
		}
		if ok {
			m.n++
			m.deliveries = append(m.deliveries, &memDelivery{
				id: fmt.Sprintf("00000000-0000-4000-8000-%012d", m.n), eventID: id, endpoint: ep.ID, status: events.Pending,
				next: m.clock.Now(), createdAt: m.clock.Now(),
			})
		}
	}
	return e, true, nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func (m *MemStore) Feed(_ context.Context, q events.FeedQuery) ([]events.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []events.Event
	for _, e := range m.events {
		switch {
		case !m.Defects.FeedIgnoresRealm && !contains(q.Realms, e.Realm):
		case !m.Defects.FeedIgnoresTypes && len(q.Types) > 0 && !contains(q.Types, e.Type):
		case q.After != "" && (e.ID < q.After || e.ID == q.After && !m.Defects.FeedInclusive):
		default:
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if m.Defects.FeedBackwards {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (m *MemStore) eventByID(id string) events.Event {
	for _, e := range m.events {
		if e.ID == id {
			return e
		}
	}
	return events.Event{}
}

func (m *MemStore) Claim(_ context.Context, now time.Time, lease time.Duration, limit int) ([]events.Due, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []events.Due
	for _, d := range m.deliveries {
		due := d.status == events.Pending || m.Defects.DeliveredStaysDue && d.status == events.Delivered || m.Defects.DeadStaysDue && d.status == events.Dead
		if !due || d.next.After(now) || limit > 0 && len(out) >= limit {
			continue
		}
		ep := m.endpoints[d.endpoint]
		if !ep.Active && !m.Defects.FanOutInactive {
			continue
		}
		if !m.Defects.NoLease {
			d.next = now.Add(lease)
		}
		out = append(out, events.Due{DeliveryID: d.id, Event: m.eventByID(d.eventID), Endpoint: ep, Attempts: d.attempts, CreatedAt: d.createdAt})
	}
	return out, nil
}

func (m *MemStore) find(id string) (*memDelivery, error) {
	for _, d := range m.deliveries {
		if d.id == id {
			return d, nil
		}
	}
	return nil, fmt.Errorf("%w: delivery %s", events.ErrNotFound, id)
}

func (m *MemStore) Delivered(_ context.Context, id string, _ int, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, err := m.find(id)
	if err != nil {
		return err
	}
	d.status = events.Delivered
	return nil
}

func (m *MemStore) Failed(_ context.Context, id string, f events.Failure) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, err := m.find(id)
	if err != nil {
		return err
	}
	d.attempts++
	if f.Dead {
		d.status = events.Dead
		return nil
	}
	d.next = f.Next
	return nil
}

func (m *MemStore) ExpireStale(_ context.Context, now time.Time) ([]events.Ended, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Defects.ExpireNothing {
		return nil, nil
	}
	n := map[[2]string]int64{}
	for _, d := range m.deliveries {
		if d.status == events.Pending && !d.createdAt.Add(events.Window).After(now) {
			d.status = events.Dead
			n[[2]string{m.eventByID(d.eventID).Realm, d.endpoint}]++
		}
	}
	var out []events.Ended
	for k, v := range n {
		out = append(out, events.Ended{Realm: k[0], Endpoint: k[1], N: v})
	}
	return out, nil
}

func (m *MemStore) Redeliver(_ context.Context, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, err := m.find(id)
	if err != nil {
		return err
	}
	if d.status != events.Dead && !m.Defects.RedeliverAnything {
		return fmt.Errorf("%w: delivery %s is %s, and only a dead one is re-driven", events.ErrInvalid, id, d.status)
	}
	d.status, d.next = events.Pending, at
	return nil
}

func (m *MemStore) DeadCounts(context.Context) (map[string]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int64{}
	for _, e := range m.events {
		out[e.Realm] += 0
	}
	for _, d := range m.deliveries {
		if d.status == events.Dead {
			out[m.eventByID(d.eventID).Realm]++
		}
	}
	return out, nil
}

func (m *MemStore) Purge(_ context.Context, before time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Defects.PurgeNothing {
		return 0, nil
	}
	owed := map[string]bool{}
	for _, d := range m.deliveries {
		if d.status == events.Pending && !m.Defects.PurgeEverything {
			owed[d.eventID] = true
		}
	}
	var keep []events.Event
	gone := map[string]bool{}
	for _, e := range m.events {
		if e.CreatedAt.Before(before) && !owed[e.ID] {
			gone[e.ID] = true
			continue
		}
		keep = append(keep, e)
	}
	m.events = keep
	var ds []*memDelivery
	for _, d := range m.deliveries {
		if !gone[d.eventID] {
			ds = append(ds, d)
		}
	}
	m.deliveries = ds
	return len(gone), nil
}

// raw is for the contract's own comparison of data.
func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return string(a) == string(b)
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return string(xa) == string(ya)
}
