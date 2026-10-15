package contracts

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// In-memory collaborators for the suites: honest enough that a suite exercises the real contract of each port
// (a lease is exclusive, an update is serialized, a grant is idempotent on its key and refuses the same key for
// a different request), and observable enough that a suite can ask what was touched, and in what order.

// CallLog records the order in which the collaborators were touched.
type CallLog struct {
	mu    sync.Mutex
	calls []string
}

// Add appends a call.
func (l *CallLog) Add(call string) {
	l.mu.Lock()
	l.calls = append(l.calls, call)
	l.mu.Unlock()
}

// Calls returns the calls so far.
func (l *CallLog) Calls() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

// Reset forgets the calls.
func (l *CallLog) Reset() {
	l.mu.Lock()
	l.calls = nil
	l.mu.Unlock()
}

// Touched reports the calls whose name starts with one of the prefixes ("inbox.", "meter.").
func (l *CallLog) Touched(prefixes ...string) []string {
	var out []string
	for _, c := range l.Calls() {
		for _, p := range prefixes {
			if strings.HasPrefix(c, p) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// Index returns the position of the first call equal to name, or -1.
func (l *CallLog) Index(name string) int {
	for i, c := range l.Calls() {
		if c == name {
			return i
		}
	}
	return -1
}

// RecordingMetrics is a Metrics that remembers every count and gauge.
type RecordingMetrics struct {
	mu     sync.Mutex
	counts map[string]int64
	gauges map[string]int64
}

func metricKey(name string, l mports.Labels) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(name)
	for _, k := range keys {
		fmt.Fprintf(&b, "|%s=%s", k, l[k])
	}
	return b.String()
}

// Count adds n to a counter.
func (m *RecordingMetrics) Count(name string, n int64, l mports.Labels) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.counts == nil {
		m.counts = map[string]int64{}
	}
	m.counts[metricKey(name, l)] += n
}

// Gauge sets a gauge.
func (m *RecordingMetrics) Gauge(name string, v int64, l mports.Labels) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gauges == nil {
		m.gauges = map[string]int64{}
	}
	m.gauges[metricKey(name, l)] = v
}

// Series is every counter and gauge series recorded, as name|label=value.
func (m *RecordingMetrics) Series() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.counts {
		out = append(out, k)
	}
	for k := range m.gauges {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Observe is not recorded: no billing metric is a latency.
func (m *RecordingMetrics) Observe(string, float64, mports.Labels) {}

// Counter reads a counter by name and labels.
func (m *RecordingMetrics) Counter(name string, l mports.Labels) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[metricKey(name, l)]
}

// GaugeValue reads a gauge, and whether it was ever set.
func (m *RecordingMetrics) GaugeValue(name string, l mports.Labels) (int64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.gauges[metricKey(name, l)]
	return v, ok
}

// ---------------------------------------------------------------- accounts --

// MemAccounts serves provider accounts.
type MemAccounts struct {
	mu   sync.Mutex
	log  *CallLog
	accs map[string]domain.ProviderAccount
}

// NewMemAccounts holds the accounts given.
func NewMemAccounts(log *CallLog, accs ...domain.ProviderAccount) *MemAccounts {
	m := &MemAccounts{log: log, accs: map[string]domain.ProviderAccount{}}
	for _, a := range accs {
		m.accs[a.ID] = a
	}
	return m
}

// Put adds or replaces an account.
func (m *MemAccounts) Put(a domain.ProviderAccount) {
	m.mu.Lock()
	m.accs[a.ID] = a
	m.mu.Unlock()
}

// Account implements ports.Accounts.
func (m *MemAccounts) Account(_ context.Context, id string) (domain.ProviderAccount, bool, error) {
	m.log.Add("accounts.Account")
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.accs[id]
	return a, ok, nil
}

// ForRealm implements ports.Accounts.
func (m *MemAccounts) ForRealm(_ context.Context, realm domain.Realm) ([]domain.ProviderAccount, error) {
	m.log.Add("accounts.ForRealm")
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.ProviderAccount
	for _, a := range m.accs {
		if a.Serves(realm) {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ------------------------------------------------------------------- inbox --

// Statuses of an inbox row, as payment_events has them, plus the one a rejected parked delivery ends in.
const (
	RowReceived     = "received"
	RowProcessing   = "processing"
	RowProcessed    = "processed"
	RowIgnored      = "ignored"
	RowFailed       = "failed"
	RowDead         = "dead"
	RowUnverifiable = "unverifiable"
	RowRejected     = "rejected"
)

// InboxRow is one row of the in-memory webhook queue.
type InboxRow struct {
	ID            string
	Event         domain.Event
	Status        string
	Outcome       string
	Attempts      int
	NextAttemptAt time.Time
	LeaseUntil    time.Time
	Err           string
	ReceivedAt    time.Time
	ProcessedAt   time.Time
	// Only while Status is unverifiable.
	Provider     domain.Provider
	Account      string
	ParkedBody   []byte
	ParkedHeader http.Header
}

// MemInbox is the webhook queue in memory. The exported flags break it in the ways the ingress suite must be
// able to see: a store that lies about persisting, one that refuses a duplicate, one that loses a parked body.
type MemInbox struct {
	// MaxAttempts is the attempt after which a failed event is dead (default 5).
	MaxAttempts int
	// LiePersist makes Persist report success and store nothing.
	LiePersist bool
	// ErrOnDuplicate makes Persist fail on an event it already holds, instead of reporting success.
	ErrOnDuplicate bool
	// DropParkedBody makes Park keep the row and lose the body.
	DropParkedBody bool
	// NoExclusion makes Lease hand the same event to every caller: a lease that excludes nobody.
	NoExclusion bool
	// AckOnLease marks an event processed the moment it is leased: an acknowledgement before the work, which a
	// later failure cannot take back.
	AckOnLease bool

	mu          sync.Mutex
	gate        *leaseGate
	clock       mports.Clock
	log         *CallLog
	rows        []*InboxRow
	byKey       map[string]*InboxRow
	seq         int
	failPersist error
}

// NewMemInbox is an empty queue.
func NewMemInbox(clock mports.Clock, log *CallLog) *MemInbox {
	return &MemInbox{MaxAttempts: 5, clock: clock, log: log, byKey: map[string]*InboxRow{}}
}

func inboxKey(account, eventID string) string { return account + "|" + eventID }

// leaseGate holds the first n callers of Lease until all n have arrived, once.
type leaseGate struct {
	n       int32
	arrived atomic.Int32
	open    chan struct{}
}

// SyncLeases makes the next n calls to Lease rendezvous before any of them proceeds, so a test of concurrent
// processors does not depend on one finishing before another starts. Set it before the callers run.
func (m *MemInbox) SyncLeases(n int) {
	m.gate = &leaseGate{n: int32(n), open: make(chan struct{})} //nolint:gosec // n is a handful of goroutines in a test
}

// FailPersist makes the next Persists fail with err; nil clears it.
func (m *MemInbox) FailPersist(err error) {
	m.mu.Lock()
	m.failPersist = err
	m.mu.Unlock()
}

func (m *MemInbox) newID() string {
	m.seq++
	return fmt.Sprintf("row-%d", m.seq)
}

// Persist implements ports.Inbox.
func (m *MemInbox) Persist(_ context.Context, e domain.Event) (bool, error) {
	m.log.Add("inbox.Persist")
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failPersist != nil {
		return false, m.failPersist
	}
	if err := e.Validate(); err != nil {
		return false, err
	}
	if m.LiePersist {
		return true, nil
	}
	k := inboxKey(e.Account, e.ProviderEventID)
	if _, dup := m.byKey[k]; dup {
		if m.ErrOnDuplicate {
			return false, fmt.Errorf("%w: event %s", md.ErrAlreadyExists, e.ProviderEventID)
		}
		return false, nil
	}
	now := m.clock.Now()
	r := &InboxRow{ID: m.newID(), Event: e, Status: RowReceived, NextAttemptAt: now, ReceivedAt: now}
	m.rows = append(m.rows, r)
	m.byKey[k] = r
	return true, nil
}

// Park implements ports.Inbox.
func (m *MemInbox) Park(_ context.Context, provider domain.Provider, account, eventID string, raw []byte, headers http.Header) error {
	m.log.Add("inbox.Park")
	m.mu.Lock()
	defer m.mu.Unlock()
	k := inboxKey(account, eventID)
	if _, dup := m.byKey[k]; dup {
		return nil
	}
	now := m.clock.Now()
	r := &InboxRow{ID: m.newID(), Status: RowUnverifiable, NextAttemptAt: now, ReceivedAt: now, Provider: provider, Account: account,
		Event: domain.Event{Provider: provider, Account: account, ProviderEventID: eventID}}
	if !m.DropParkedBody {
		r.ParkedBody = append([]byte(nil), raw...)
		r.ParkedHeader = headers.Clone()
	}
	m.rows = append(m.rows, r)
	m.byKey[k] = r
	return nil
}

// customerKey names whose events must not be handled concurrently.
func customerKey(e domain.Event) string {
	if c := e.Object.ProviderCustomerID; c != "" {
		return e.Account + "|" + c
	}
	return e.Account + "|event|" + e.ProviderEventID
}

// Lease implements ports.Inbox: events due, in the provider's order, never two of one customer at once.
func (m *MemInbox) Lease(_ context.Context, n int, ttl time.Duration) ([]ports.LeasedEvent, error) {
	m.log.Add("inbox.Lease")
	out := m.leaseLocked(n, ttl)
	// With SyncLeases, nobody goes on until every caller has taken its lease: the lease alone decides who has
	// the event, not who got there first.
	if g := m.gate; g != nil {
		if g.arrived.Add(1) == g.n {
			close(g.open)
		}
		select {
		case <-g.open:
		case <-time.After(10 * time.Second):
			return nil, errors.New("memory inbox: the lease rendezvous never completed")
		}
	}
	return out, nil
}

func (m *MemInbox) leaseLocked(n int, ttl time.Duration) []ports.LeasedEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock.Now()
	busy := map[string]bool{}
	for _, r := range m.rows {
		if r.Status == RowProcessing && r.LeaseUntil.After(now) {
			busy[customerKey(r.Event)] = true
		}
	}
	var due []*InboxRow
	for _, r := range m.rows {
		switch {
		case r.Status == RowReceived || r.Status == RowFailed:
			if !r.NextAttemptAt.After(now) {
				due = append(due, r)
			}
		case r.Status == RowProcessing && !r.LeaseUntil.After(now):
			due = append(due, r)
		}
	}
	sort.SliceStable(due, func(i, j int) bool {
		if !due[i].Event.CreatedAt.Equal(due[j].Event.CreatedAt) {
			return due[i].Event.CreatedAt.Before(due[j].Event.CreatedAt)
		}
		return due[i].ReceivedAt.Before(due[j].ReceivedAt)
	})
	var out []ports.LeasedEvent
	for _, r := range due {
		if len(out) >= n {
			break
		}
		key := customerKey(r.Event)
		if busy[key] && !m.NoExclusion {
			continue
		}
		busy[key] = true
		r.Attempts++
		switch {
		case m.AckOnLease:
			r.Status, r.Outcome, r.ProcessedAt = RowProcessed, RowProcessed, now
		case !m.NoExclusion:
			r.Status, r.LeaseUntil = RowProcessing, now.Add(ttl)
		}
		out = append(out, ports.LeasedEvent{ID: r.ID, Event: r.Event, Attempts: r.Attempts})
	}
	return out
}

func (m *MemInbox) rowLocked(id string) *InboxRow {
	for _, r := range m.rows {
		if r.ID == id {
			return r
		}
	}
	return nil
}

// MarkProcessed implements ports.Inbox.
func (m *MemInbox) MarkProcessed(_ context.Context, id, outcome string) error {
	m.log.Add("inbox.MarkProcessed")
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rowLocked(id)
	if r == nil {
		return fmt.Errorf("%w: inbox row %s", md.ErrNotFound, id)
	}
	r.Status, r.Outcome, r.ProcessedAt = RowProcessed, outcome, m.clock.Now()
	if outcome == RowIgnored {
		r.Status = RowIgnored
	}
	r.LeaseUntil = time.Time{}
	return nil
}

// MarkFailed implements ports.Inbox.
func (m *MemInbox) MarkFailed(_ context.Context, id string, cause error, retryAt time.Time) (bool, error) {
	m.log.Add("inbox.MarkFailed")
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rowLocked(id)
	if r == nil {
		return false, fmt.Errorf("%w: inbox row %s", md.ErrNotFound, id)
	}
	if m.AckOnLease && r.Status == RowProcessed {
		return false, nil // acknowledged already: the failure has nowhere to be recorded
	}
	r.Err, r.LeaseUntil = cause.Error(), time.Time{}
	if r.Attempts >= m.MaxAttempts {
		r.Status = RowDead
		return true, nil
	}
	r.Status, r.NextAttemptAt = RowFailed, retryAt
	return false, nil
}

// LeaseParked implements ports.Inbox.
func (m *MemInbox) LeaseParked(_ context.Context, n int, ttl time.Duration) ([]ports.ParkedDelivery, error) {
	m.log.Add("inbox.LeaseParked")
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock.Now()
	var out []ports.ParkedDelivery
	for _, r := range m.rows {
		if len(out) >= n {
			break
		}
		if r.Status != RowUnverifiable || r.NextAttemptAt.After(now) || r.LeaseUntil.After(now) {
			continue
		}
		r.LeaseUntil = now.Add(ttl)
		r.Attempts++
		out = append(out, ports.ParkedDelivery{ID: r.ID, Provider: r.Provider, Account: r.Account,
			Body: append([]byte(nil), r.ParkedBody...), Header: r.ParkedHeader.Clone(), Attempts: r.Attempts})
	}
	return out, nil
}

// Promote implements ports.Inbox.
func (m *MemInbox) Promote(_ context.Context, id string, e domain.Event) (bool, error) {
	m.log.Add("inbox.Promote")
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rowLocked(id)
	if r == nil || r.Status != RowUnverifiable {
		return false, fmt.Errorf("%w: no parked delivery %s", md.ErrNotFound, id)
	}
	if err := e.Validate(); err != nil {
		return false, err
	}
	k := inboxKey(e.Account, e.ProviderEventID)
	if existing, dup := m.byKey[k]; dup && existing != r {
		// The provider redelivered it normally while it was parked: one event, and the parked copy goes.
		r.Status, r.ParkedBody, r.ParkedHeader, r.Outcome = RowRejected, nil, nil, "duplicate"
		return false, nil
	}
	delete(m.byKey, inboxKey(r.Account, r.Event.ProviderEventID))
	m.byKey[k] = r
	r.Event, r.Status, r.ParkedBody, r.ParkedHeader = e, RowReceived, nil, nil
	r.Attempts, r.LeaseUntil, r.NextAttemptAt = 0, time.Time{}, m.clock.Now()
	return true, nil
}

// RetryParked implements ports.Inbox.
func (m *MemInbox) RetryParked(_ context.Context, id string, retryAt time.Time) error {
	m.log.Add("inbox.RetryParked")
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.rowLocked(id); r != nil {
		r.NextAttemptAt, r.LeaseUntil = retryAt, time.Time{}
	}
	return nil
}

// Reject implements ports.Inbox.
func (m *MemInbox) Reject(_ context.Context, id, reason string) error {
	m.log.Add("inbox.Reject")
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.rowLocked(id); r != nil {
		r.Status, r.Err, r.ParkedBody, r.ParkedHeader = RowRejected, reason, nil, nil
	}
	return nil
}

// Oldest implements ports.Inbox.
func (m *MemInbox) Oldest(context.Context) (time.Time, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var oldest time.Time
	var ok bool
	for _, r := range m.rows {
		switch r.Status {
		case RowReceived, RowProcessing, RowFailed, RowUnverifiable:
			if !ok || r.ReceivedAt.Before(oldest) {
				oldest, ok = r.ReceivedAt, true
			}
		}
	}
	return oldest, ok, nil
}

// Purge implements ports.Inbox.
func (m *MemInbox) Purge(_ context.Context, olderThan time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept, n := m.rows[:0:0], 0
	for _, r := range m.rows {
		if (r.Status == RowProcessed || r.Status == RowIgnored) && r.ProcessedAt.Before(olderThan) {
			delete(m.byKey, inboxKey(r.Event.Account, r.Event.ProviderEventID))
			n++
			continue
		}
		kept = append(kept, r)
	}
	m.rows = kept
	return n, nil
}

// Rows is a copy of every row.
func (m *MemInbox) Rows() []InboxRow {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]InboxRow, 0, len(m.rows))
	for _, r := range m.rows {
		out = append(out, *r)
	}
	return out
}

// Count is the number of verified events held: everything but a parked or rejected delivery.
func (m *MemInbox) Count() int {
	n := 0
	for _, r := range m.Rows() {
		if r.Status != RowUnverifiable && r.Status != RowRejected {
			n++
		}
	}
	return n
}

// Parked is the deliveries waiting for verification.
func (m *MemInbox) Parked() []InboxRow {
	var out []InboxRow
	for _, r := range m.Rows() {
		if r.Status == RowUnverifiable {
			out = append(out, r)
		}
	}
	return out
}

// Row finds an event row by its provider event id.
func (m *MemInbox) Row(eventID string) (InboxRow, bool) {
	for _, r := range m.Rows() {
		if r.Event.ProviderEventID == eventID {
			return r, true
		}
	}
	return InboxRow{}, false
}

// Add stores a verified event as received, as the ingress would have; a test's way of saying "this was
// persisted before the crash".
func (m *MemInbox) Add(e domain.Event) {
	if _, err := m.Persist(context.Background(), e); err != nil {
		panic(err)
	}
	m.log.Reset()
}

// ---------------------------------------------------------------- payments --

// MemPayments holds payments, with an exclusive update per payment.
type MemPayments struct {
	// failCommit is how many more Updates run their function and then fail to store the result, as a database
	// that goes away between the work and the commit does.
	failCommit int

	log   *CallLog
	mu    sync.Mutex
	rows  map[string]domain.Payment
	locks map[string]*sync.Mutex
}

// NewMemPayments is an empty store.
func NewMemPayments(log *CallLog) *MemPayments {
	return &MemPayments{log: log, rows: map[string]domain.Payment{}, locks: map[string]*sync.Mutex{}}
}

func paymentKey(account, id string) string { return account + "|" + id }

// FailCommit makes the next n Updates run their function, with whatever side effects it has, and then fail.
func (m *MemPayments) FailCommit(n int) {
	m.mu.Lock()
	m.failCommit = n
	m.mu.Unlock()
}

// Get implements ports.Payments.
func (m *MemPayments) Get(_ context.Context, account, id string) (domain.Payment, bool, error) {
	m.log.Add("payments.Get")
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.rows[paymentKey(account, id)]
	return p, ok, nil
}

// Upsert implements ports.Payments.
func (m *MemPayments) Upsert(_ context.Context, p domain.Payment) error {
	m.log.Add("payments.Upsert")
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[paymentKey(p.Account, p.ProviderPaymentID)] = p
	return nil
}

// Update implements ports.Payments: fn runs with the payment's lock held, so two refund reports for one payment
// cannot both read the same cumulative figures.
func (m *MemPayments) Update(_ context.Context, account, id string, fn func(domain.Payment, bool) (domain.Payment, error)) (domain.Payment, error) {
	m.log.Add("payments.Update")
	k := paymentKey(account, id)
	m.mu.Lock()
	lock := m.locks[k]
	if lock == nil {
		lock = &sync.Mutex{}
		m.locks[k] = lock
	}
	m.mu.Unlock()

	lock.Lock()
	defer lock.Unlock()
	m.mu.Lock()
	cur, found := m.rows[k]
	m.mu.Unlock()
	next, err := fn(cur, found)
	if err != nil {
		return cur, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failCommit > 0 {
		m.failCommit--
		return cur, errors.New("memory payments: the commit failed after the work was done")
	}
	m.rows[k] = next
	return next, nil
}

// List implements ports.Payments (newest first; the cursor is an index).
func (m *MemPayments) List(_ context.Context, s domain.Scope, cursor string, limit int) ([]domain.Payment, string, error) {
	m.log.Add("payments.List")
	m.mu.Lock()
	defer m.mu.Unlock()
	var all []domain.Payment
	for _, p := range m.rows {
		if p.Scope.Normalized() == s.Normalized() {
			all = append(all, p)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.After(all[j].CreatedAt)
		}
		return all[i].ProviderPaymentID < all[j].ProviderPaymentID
	})
	start := 0
	if cursor != "" {
		if _, err := fmt.Sscanf(cursor, "%d", &start); err != nil || start < 0 {
			return nil, "", fmt.Errorf("%w: bad cursor", md.ErrInvalid)
		}
	}
	if start > len(all) {
		start = len(all)
	}
	end := min(start+max(limit, 1), len(all))
	next := ""
	if end < len(all) {
		next = fmt.Sprintf("%d", end)
	}
	return all[start:end], next, nil
}

// Row finds a payment by its provider id in any account; the zero Payment if there is none.
func (m *MemPayments) Row(id string) domain.Payment {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.rows {
		if p.ProviderPaymentID == id {
			return p
		}
	}
	return domain.Payment{}
}

// Count is the number of payments held.
func (m *MemPayments) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows)
}

// ------------------------------------------------------------ subscriptions --

// MemSubs holds subscriptions.
type MemSubs struct {
	// NoOrder makes ApplyIfNewer overwrite whatever is held, whatever its age: delivery order deciding state.
	NoOrder bool
	// NoLapseMark makes MarkGraceLapsed record nothing, so an expired grace is due on every sweep.
	NoLapseMark bool

	log  *CallLog
	mu   sync.Mutex
	rows map[string]domain.Sub
}

// NewMemSubs is an empty store.
func NewMemSubs(log *CallLog) *MemSubs { return &MemSubs{log: log, rows: map[string]domain.Sub{}} }

func subKey(account, id string) string { return account + "|" + id }

// Get implements ports.Subs: the scope's live subscription, else the newest it ever had.
func (m *MemSubs) Get(_ context.Context, s domain.Scope) (domain.Sub, bool, error) {
	m.log.Add("subs.Get")
	m.mu.Lock()
	defer m.mu.Unlock()
	var best domain.Sub
	var found bool
	for _, sub := range m.rows {
		if sub.Scope.Normalized() != s.Normalized() {
			continue
		}
		switch {
		case !found, sub.Live() && !best.Live(), sub.Live() == best.Live() && sub.LastEventAt.After(best.LastEventAt):
			best, found = sub, true
		}
	}
	return best, found, nil
}

// ApplyIfNewer implements ports.Subs through the domain's total order.
func (m *MemSubs) ApplyIfNewer(_ context.Context, sub domain.Sub, at time.Time) (bool, error) {
	m.log.Add("subs.ApplyIfNewer")
	m.mu.Lock()
	defer m.mu.Unlock()
	k := subKey(sub.Account, sub.ProviderSubscriptionID)
	next, applied := m.rows[k].Apply(sub, at)
	if m.NoOrder {
		sub.LastEventAt = at
		next, applied = sub, true
	}
	if applied {
		m.rows[k] = next
	}
	return applied, nil
}

// DueForGraceExpiry implements ports.Subs.
func (m *MemSubs) DueForGraceExpiry(_ context.Context, now time.Time) ([]domain.Sub, error) {
	m.log.Add("subs.DueForGraceExpiry")
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.Sub
	for _, s := range m.rows {
		if s.GraceDue(now) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProviderSubscriptionID < out[j].ProviderSubscriptionID })
	return out, nil
}

// MarkGraceLapsed implements ports.Subs.
func (m *MemSubs) MarkGraceLapsed(_ context.Context, sub domain.Sub, at time.Time) (bool, error) {
	m.log.Add("subs.MarkGraceLapsed")
	m.mu.Lock()
	defer m.mu.Unlock()
	k := subKey(sub.Account, sub.ProviderSubscriptionID)
	cur, ok := m.rows[k]
	if m.NoLapseMark {
		return true, nil
	}
	if !ok || cur.GraceLapsedAt != nil || cur.GraceUntil == nil || sub.GraceUntil == nil || !cur.GraceUntil.Equal(*sub.GraceUntil) {
		return false, nil
	}
	cur.GraceLapsedAt = &at
	m.rows[k] = cur
	return true, nil
}

// Row finds a subscription by its provider id in any account; the zero Sub if there is none.
func (m *MemSubs) Row(id string) domain.Sub {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.rows {
		if s.ProviderSubscriptionID == id {
			return s
		}
	}
	return domain.Sub{}
}

// Put stores a subscription as it is, bypassing the order: a test's way of saying "this is where it stood".
func (m *MemSubs) Put(s domain.Sub) {
	m.mu.Lock()
	m.rows[subKey(s.Account, s.ProviderSubscriptionID)] = s
	m.mu.Unlock()
}

// --------------------------------------------------------------- customers --

// MemCustomers maps provider customers to scopes.
type MemCustomers struct {
	log  *CallLog
	mu   sync.Mutex
	rows []domain.Customer
}

// NewMemCustomers is an empty store.
func NewMemCustomers(log *CallLog) *MemCustomers { return &MemCustomers{log: log} }

// Get implements ports.Customers.
func (m *MemCustomers) Get(_ context.Context, s domain.Scope, account string) (domain.Customer, bool, error) {
	m.log.Add("customers.Get")
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.rows {
		if c.Scope.Normalized() == s.Normalized() && c.Account == account {
			return c, true, nil
		}
	}
	return domain.Customer{}, false, nil
}

// Upsert implements ports.Customers.
func (m *MemCustomers) Upsert(_ context.Context, c domain.Customer) error {
	m.log.Add("customers.Upsert")
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, have := range m.rows {
		if have.Scope.Normalized() == c.Scope.Normalized() && have.Account == c.Account {
			m.rows[i] = c
			return nil
		}
	}
	m.rows = append(m.rows, c)
	return nil
}

// ResolveScope implements ports.Customers.
func (m *MemCustomers) ResolveScope(_ context.Context, account, providerCustomerID string) (domain.Scope, bool, error) {
	m.log.Add("customers.ResolveScope")
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.rows {
		if c.Account == account && c.ProviderCustomerID == providerCustomerID {
			return c.Scope, true, nil
		}
	}
	return domain.Scope{}, false, nil
}

// ----------------------------------------------------------------- catalog --

// MemCatalog is the catalogue in memory: plans per realm, with the provider price each account sells them at.
type MemCatalog struct {
	// FallbackPlan, when set, is what PlanForPrice answers for a price it does not know: a default plan, the
	// thing the catalogue must never do.
	FallbackPlan *domain.Plan

	log    *CallLog
	mu     sync.Mutex
	plans  []domain.Plan
	prices map[string]string // account|realm|code|currency → provider price id
}

// NewMemCatalog is an empty catalogue.
func NewMemCatalog(log *CallLog) *MemCatalog {
	return &MemCatalog{log: log, prices: map[string]string{}}
}

// Add puts a plan on sale, and maps its provider price for one account and currency.
func (m *MemCatalog) Add(p domain.Plan, account, currency, providerPriceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	found := false
	for i, have := range m.plans {
		if have.Realm.Or() == p.Realm.Or() && have.Code == p.Code {
			m.plans[i], found = p, true
		}
	}
	if !found {
		m.plans = append(m.plans, p)
	}
	m.prices[account+"|"+string(p.Realm.Or())+"|"+p.Code+"|"+currency] = providerPriceID
}

// List implements ports.Catalog.
func (m *MemCatalog) List(_ context.Context, realm domain.Realm, currency string) ([]domain.Plan, error) {
	m.log.Add("catalog.List")
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.Plan
	for _, p := range m.plans {
		if _, ok := p.Prices[currency]; ok && p.Active && p.Realm.Or() == realm.Or() {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].SortOrder < out[j].SortOrder })
	return out, nil
}

// Resolve implements ports.Catalog.
func (m *MemCatalog) Resolve(_ context.Context, realm domain.Realm, code, currency, account string) (domain.Plan, string, error) {
	m.log.Add("catalog.Resolve")
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.plans {
		if p.Realm.Or() == realm.Or() && p.Code == code && p.Active {
			if id, ok := m.prices[account+"|"+string(p.Realm.Or())+"|"+code+"|"+currency]; ok {
				return p, id, nil
			}
		}
	}
	return domain.Plan{}, "", fmt.Errorf("%w: %s in %s for account %s", domain.ErrUnknownPrice, code, currency, account)
}

// PlanForPrice implements ports.Catalog.
func (m *MemCatalog) PlanForPrice(_ context.Context, account, providerPriceID string) (domain.Plan, bool, error) {
	m.log.Add("catalog.PlanForPrice")
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, id := range m.prices {
		if id != providerPriceID || !strings.HasPrefix(k, account+"|") {
			continue
		}
		parts := strings.Split(k, "|")
		for _, p := range m.plans {
			if string(p.Realm.Or()) == parts[1] && p.Code == parts[2] {
				return p, true, nil
			}
		}
	}
	if m.FallbackPlan != nil {
		return *m.FallbackPlan, true, nil
	}
	return domain.Plan{}, false, nil
}

// ------------------------------------------------------------------- meter --

// FakeMeter is the Meter billing mints credits through. It is idempotent the way the engine is: the same key and
// the same grant is a replay (Applied=false), the same key for a different grant is ErrIdemConflict, and keys are
// per realm. It records every call, so a suite can say "granted once", "granted nothing" and "net of the dispute".
type FakeMeter struct {
	// NoIdempotency makes the meter apply every grant it is given, whatever the key: the flaw the durable guard
	// exists to prevent.
	NoIdempotency bool

	log *CallLog
	mu  sync.Mutex

	calls   int
	failN   int
	applied []md.Grant
	byKey   map[string]string // realm|key → fingerprint
	balance map[string]md.Credits
}

// NewFakeMeter is a meter that has granted nothing.
func NewFakeMeter(log *CallLog) *FakeMeter {
	return &FakeMeter{log: log, byKey: map[string]string{}, balance: map[string]md.Credits{}}
}

var errMeterDown = errors.New("meter: the hot tier is unavailable")

// FailNextGrant makes the next n Grant calls fail, as an outage does.
func (m *FakeMeter) FailNextGrant(n int) {
	m.mu.Lock()
	m.failN = n
	m.mu.Unlock()
}

// Grant implements ports.Meter.
func (m *FakeMeter) Grant(_ context.Context, g md.Grant) (md.Receipt, error) {
	m.log.Add("meter.Grant")
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.failN > 0 {
		m.failN--
		return md.Receipt{}, errMeterDown
	}
	if err := g.Validate(); err != nil {
		return md.Receipt{}, err
	}
	k := string(g.Scope.Realm.Or()) + "|" + g.IdemKey
	fp := g.Fingerprint()
	if have, ok := m.byKey[k]; ok && !m.NoIdempotency {
		if have != fp {
			return md.Receipt{}, fmt.Errorf("%w: %s", md.ErrIdemConflict, g.IdemKey)
		}
		return md.Receipt{IdemKey: g.IdemKey, Applied: false}, nil
	}
	m.byKey[k] = fp
	m.applied = append(m.applied, g)
	bk := g.Scope.Tag() + "|" + string(g.Pool.Or())
	m.balance[bk] += g.Amount
	return md.Receipt{IdemKey: g.IdemKey, Applied: true, Delta: g.Amount, Balance: m.balance[bk]}, nil
}

// Admit is not billing's to call.
func (m *FakeMeter) Admit(context.Context, md.Scope, md.AdmitRequest) (md.Admission, error) {
	return md.Admission{}, domain.ErrNotImplemented
}

// Record is not billing's to call.
func (m *FakeMeter) Record(context.Context, md.Event) (md.Receipt, error) {
	return md.Receipt{}, domain.ErrNotImplemented
}

// Transfer is not billing's to call.
func (m *FakeMeter) Transfer(context.Context, md.Transfer) (md.TransferReceipt, error) {
	return md.TransferReceipt{}, domain.ErrNotImplemented
}

// GrantCalls is how many times Grant was called, applied or not, failed or not.
func (m *FakeMeter) GrantCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// Applied is every grant that took effect, in order.
func (m *FakeMeter) Applied() []md.Grant {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]md.Grant(nil), m.applied...)
}

// Grants is how many grants with this idempotency key took effect.
func (m *FakeMeter) Grants(key string) int {
	n := 0
	for _, g := range m.Applied() {
		if g.IdemKey == key {
			n++
		}
	}
	return n
}

// LastGrant is the most recent grant that took effect.
func (m *FakeMeter) LastGrant() md.Grant {
	a := m.Applied()
	if len(a) == 0 {
		return md.Grant{}
	}
	return a[len(a)-1]
}

// NetGranted is the credits one payment has put into the books: its purchase grant, less what refunds and
// chargebacks took back, plus what a won dispute returned. A grant belongs to a payment when its key, its source
// key or its payment reference names it.
func (m *FakeMeter) NetGranted(providerPaymentID string) md.Credits {
	var net md.Credits
	for _, g := range m.Applied() {
		if g.IdemKey == providerPaymentID || g.SourceKey == providerPaymentID || g.Ref["payment"] == providerPaymentID {
			net += g.Amount
		}
	}
	return net
}

// Balance is the credits granted to a pool of a scope, net.
func (m *FakeMeter) Balance(s md.Scope, p md.Pool) md.Credits {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.balance[s.Tag()+"|"+string(p.Or())]
}

// -------------------------------------------------------------------- lots --

// FakeLots is the lot reader: where a grant's credits stand under CreditExpiry=lots_fefo. With nothing lapsed,
// it answers like a deployment with expiry off — no lots at all.
type FakeLots struct {
	// Blind makes the reader report no lots whatever has lapsed: a processor that cannot see expiry.
	Blind bool

	log     *CallLog
	mu      sync.Mutex
	expired map[string]md.Credits
	granted map[string]md.Credits
}

// NewFakeLots reports no lots.
func NewFakeLots(log *CallLog) *FakeLots {
	return &FakeLots{log: log, expired: map[string]md.Credits{}, granted: map[string]md.Credits{}}
}

func lotKey(s md.Scope, p md.Pool, sourceKey string) string {
	return s.Tag() + "|" + string(p.Or()) + "|" + sourceKey
}

// Lapse says that `expired` of the credits the grant under sourceKey opened have lapsed unspent.
func (l *FakeLots) Lapse(s md.Scope, p md.Pool, sourceKey string, granted, expired md.Credits) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.expired[lotKey(s, p, sourceKey)] = expired
	l.granted[lotKey(s, p, sourceKey)] = granted
}

// LotsOf implements ports.LotReader.
func (l *FakeLots) LotsOf(_ context.Context, s md.Scope, p md.Pool, sourceKey string) ([]mports.Lot, error) {
	l.log.Add("lots.LotsOf")
	l.mu.Lock()
	defer l.mu.Unlock()
	k := lotKey(s, p, sourceKey)
	exp, ok := l.expired[k]
	if !ok || l.Blind {
		return nil, nil
	}
	now := time.Now()
	return []mports.Lot{{ID: "lot-" + sourceKey, Scope: s, Pool: p.Or(), Granted: l.granted[k], Remaining: l.granted[k] - exp,
		Expired: exp, ExpiredAt: &now, SourceKey: sourceKey}}, nil
}

var (
	_ ports.Accounts   = (*MemAccounts)(nil)
	_ ports.Inbox      = (*MemInbox)(nil)
	_ ports.Payments   = (*MemPayments)(nil)
	_ ports.Subs       = (*MemSubs)(nil)
	_ ports.Customers  = (*MemCustomers)(nil)
	_ ports.Catalog    = (*MemCatalog)(nil)
	_ mports.Meter     = (*FakeMeter)(nil)
	_ mports.LotReader = (*FakeLots)(nil)
	_ mports.Metrics   = (*RecordingMetrics)(nil)
)
