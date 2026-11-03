package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/entitlement/app"
	"github.com/truongpx396/intel-payment/entitlement/contracts"
	"github.com/truongpx396/intel-payment/entitlement/domain"
	"github.com/truongpx396/intel-payment/entitlement/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// R103 (entitlement invariant 9, FR-035): entitlements are read on nearly every request, so they are cached — and the cache is
// BOUNDED (a TTL, and an entry never outlives a grace window or an override's expiry) and INVALIDATED on change (a
// subscription, an override, a grace expiry, a configuration change), so a customer who just paid does not wait out a TTL.
// Everything here runs on a fake clock and a bus that delivers when told to.

const prefix = "billing"

// countingStore counts resolutions (every resolution reads Keys once) and can hold one open.
type countingStore struct {
	ports.EntitlementStore
	keys    atomic.Int32
	entered chan struct{} // a Keys call has started
	gate    chan struct{} // while non-nil and open, Keys waits for it to close

	lastEntered chan struct{} // an Overrides call — the last read of a load — has started
	lastGate    chan struct{} // while non-nil and open, Overrides waits for it to close
}

func (c *countingStore) Overrides(ctx context.Context, s md.Scope) ([]domain.Grant, error) {
	if c.lastEntered != nil {
		select {
		case c.lastEntered <- struct{}{}:
		default:
		}
	}
	if c.lastGate != nil {
		<-c.lastGate
	}
	return c.EntitlementStore.Overrides(ctx, s)
}

func (c *countingStore) Keys(ctx context.Context, r md.Realm) ([]domain.KeyDecl, error) {
	c.keys.Add(1)
	if c.entered != nil {
		select {
		case c.entered <- struct{}{}:
		default:
		}
	}
	if c.gate != nil {
		<-c.gate
	}
	if err := ctx.Err(); err != nil { // as a database driver does: a cancelled caller's read does not complete
		return nil, err
	}
	return c.EntitlementStore.Keys(ctx, r)
}

type rig struct {
	mem    *contracts.MemStore
	faults *contracts.Breakable
	store  *countingStore
	clock  *contracts.FakeClock
	bus    *fakeBus
	met    *recorder
	e      *app.Entitler
}

func newRig(t *testing.T, ttl time.Duration, tweak ...func(*app.Deps)) *rig {
	t.Helper()
	mem := contracts.NewMemStore()
	for _, d := range []domain.KeyDecl{{Key: "sso", Kind: domain.Flag}, {Key: "export", Kind: domain.Flag}, {Key: "seats", Kind: domain.Quota}} {
		mem.DeclareKey(t, "t1", d)
		mem.DeclareKey(t, "other", d)
	}
	mem.PutPlan(t, "t1", "free", 1, domain.Entitlement{Key: "sso", Value: domain.FlagValue(false)}, domain.Entitlement{Key: "seats", Value: domain.QuotaValue(3)})
	mem.PutPlan(t, "t1", "pro", 2, domain.Entitlement{Key: "sso", Value: domain.FlagValue(true)}, domain.Entitlement{Key: "seats", Value: domain.QuotaValue(25)})
	mem.Subscribe(t, scope, "free", domain.Active, nil)
	faults := contracts.NewBreakable(mem)
	r := &rig{mem: mem, faults: faults, store: &countingStore{EntitlementStore: faults}, clock: contracts.NewFakeClock(contracts.T0), bus: &fakeBus{}, met: newRecorder()}
	d := app.Deps{Store: r.store, Clock: r.clock, Metrics: r.met, CacheTTL: ttl}
	for _, f := range tweak {
		f(&d)
	}
	e, err := app.New(d)
	if err != nil {
		t.Fatal(err)
	}
	r.e = e
	return r
}

func (r *rig) watch(t *testing.T) {
	t.Helper()
	w, err := r.e.Watch(context.Background(), r.bus, prefix, "entitlement-cache-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
}

func (r *rig) allowed(t *testing.T, s md.Scope, k domain.Key) bool {
	t.Helper()
	ok, err := r.e.Allowed(context.Background(), s, k)
	if err != nil {
		t.Fatalf("Allowed(%s): %v", k, err)
	}
	return ok
}

func (r *rig) announce(t *testing.T, s md.Scope, reason string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"realm": string(s.Realm), "scope": map[string]string{"kind": s.Kind, "id": s.ID}, "reason": reason})
	if err := r.bus.Publish(context.Background(), prefix+".entitlement."+s.SubjectToken(), body); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) announceConfig(t *testing.T, realm, table string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"table": table, "realm": realm})
	if err := r.bus.Publish(context.Background(), prefix+".config."+realm, body); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) reads() int { return int(r.store.keys.Load()) }

func TestResolutionsAreCachedPerScopeUntilTheTTL(t *testing.T) {
	t.Parallel()
	r := newRig(t, 30*time.Second)
	for range 5 {
		r.allowed(t, scope, "sso")
	}
	if r.reads() != 1 {
		t.Fatalf("five checks of one scope read the store %d times; want 1", r.reads())
	}
	other := md.Scope{Realm: "t1", Kind: "org", ID: "o2"}
	r.allowed(t, other, "sso")
	if r.reads() != 2 {
		t.Fatalf("a different scope was answered from another's entry: %d reads", r.reads())
	}
	r.clock.Advance(30*time.Second - time.Nanosecond)
	r.allowed(t, scope, "sso")
	if r.reads() != 2 {
		t.Fatalf("a nanosecond inside the TTL the entry was dropped: %d reads", r.reads())
	}
	r.clock.Advance(time.Nanosecond)
	r.allowed(t, scope, "sso")
	if r.reads() != 3 {
		t.Fatalf("at the TTL the entry must be gone: %d reads", r.reads())
	}
	// Every method reads through the same entry.
	if _, _, err := r.e.Quota(context.Background(), scope, "seats"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.e.Resolve(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	if _, err := r.e.Consume(context.Background(), scope, "seats", 1, 1); err != nil {
		t.Fatal(err)
	}
	if r.reads() != 3 {
		t.Fatalf("Quota, Resolve and Consume did not share the entry: %d reads", r.reads())
	}
}

func TestWithoutATTLNothingIsCached(t *testing.T) {
	t.Parallel()
	r := newRig(t, 0)
	r.allowed(t, scope, "sso")
	r.allowed(t, scope, "sso")
	if r.reads() != 2 {
		t.Fatalf("a zero TTL means no cache: %d reads", r.reads())
	}
	if r.e.CacheLen() != 0 {
		t.Fatal("an entry was kept")
	}
}

// A lost announcement is the case the TTL exists for. The bound is the TTL — no longer — and the test shows both halves: the
// answer is stale inside it (that is what a TTL costs) and fresh after it.
func TestALostInvalidationIsStillBoundedByTheTTL(t *testing.T) {
	t.Parallel()
	r := newRig(t, 30*time.Second)
	r.watch(t)
	if r.allowed(t, scope, "sso") {
		t.Fatal("free does not include sso")
	}
	r.mem.Subscribe(t, scope, "pro", domain.Active, nil) // the upgrade — and its announcement never arrives
	if r.allowed(t, scope, "sso") {
		t.Fatal("the entry was refreshed with no announcement and no TTL passing: it is not a cache")
	}
	r.clock.Advance(30 * time.Second)
	if !r.allowed(t, scope, "sso") {
		t.Fatal("a lost announcement kept the old answer beyond the TTL")
	}
}

func TestAnUpgradeIsVisibleAtOnceWhenItsAnnouncementArrives(t *testing.T) {
	t.Parallel()
	r := newRig(t, time.Hour)
	r.watch(t)
	if r.allowed(t, scope, "sso") {
		t.Fatal("free does not include sso")
	}
	r.mem.Subscribe(t, scope, "pro", domain.Active, nil)
	r.announce(t, scope, "subscription")
	if !r.allowed(t, scope, "sso") {
		t.Fatal("a customer who just paid must not wait out a cache TTL (invariant 9)")
	}
	if got := r.met.get("entitlement_cache_invalidations_total|source=scope"); got != 1 {
		t.Errorf("one scope announcement counted %d times", got)
	}
}

func TestAnOverrideWriteAnnouncesItselfAndIsVisibleAtOnce(t *testing.T) {
	t.Parallel()
	r := newRig(t, time.Hour)
	r.watch(t)
	o, err := app.NewOverrides(app.OverrideDeps{Store: r.mem, Bus: r.bus, SubjectPrefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	if r.allowed(t, scope, "export") {
		t.Fatal("setup: export is not granted")
	}
	g := domain.Grant{Scope: scope, Key: "export", Value: domain.FlagValue(true), Source: domain.FromOverride, Reason: "comp for the outage"}
	if err := o.Put(context.Background(), g, "alice"); err != nil {
		t.Fatal(err)
	}
	if !r.allowed(t, scope, "export") {
		t.Fatal("an override written was not visible at once")
	}
	var found bool
	for _, m := range r.bus.messages() {
		if m.Subject != prefix+".entitlement."+scope.SubjectToken() {
			continue
		}
		found = true
		var n struct {
			Realm  string `json:"realm"`
			Scope  struct{ Kind, ID string }
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(m.Payload, &n); err != nil || n.Realm != "t1" || n.Scope.Kind != "org" || n.Scope.ID != "o1" || n.Reason != "override" {
			t.Errorf("the announcement = %s (%v); want {realm t1, scope org/o1, reason override}, the shape the payment layer's has", m.Payload, err)
		}
	}
	if !found {
		t.Fatalf("the override was not announced on %s.entitlement.<tag>: %+v", prefix, r.bus.messages())
	}
}

// The write is the truth and the announcement is the hurry: if the announcement cannot be made, the caller is told — the write
// is repeatable — and the override is in the store all the same.
func TestAnOverrideWhoseAnnouncementFailsIsWrittenAndReported(t *testing.T) {
	t.Parallel()
	r := newRig(t, time.Hour)
	failing := &failingBus{fakeBus: r.bus}
	o, _ := app.NewOverrides(app.OverrideDeps{Store: r.mem, Bus: failing, SubjectPrefix: prefix})
	g := domain.Grant{Scope: scope, Key: "export", Value: domain.FlagValue(true), Source: domain.FromOverride, Reason: "comp"}
	err := o.Put(context.Background(), g, "alice")
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("want ErrUnavailable for the announcement that failed, got %v", err)
	}
	if os, _ := r.mem.Overrides(context.Background(), scope); len(os) != 1 {
		t.Fatalf("the override is not in the store: %+v", os)
	}
	// Repeating it is harmless, and now it announces.
	failing.ok.Store(true)
	if err := o.Put(context.Background(), g, "alice"); err != nil {
		t.Fatal(err)
	}
	if len(r.bus.messages()) != 1 {
		t.Fatalf("the repeat did not announce: %+v", r.bus.messages())
	}
}

// failingBus refuses every publication until told otherwise.
type failingBus struct {
	*fakeBus
	ok atomic.Bool
}

func (f *failingBus) Publish(ctx context.Context, subject string, payload []byte) error {
	if !f.ok.Load() {
		return errors.New("bus: down")
	}
	return f.fakeBus.Publish(ctx, subject, payload)
}

// Two ways a grace window ends. The payment layer announces it (and the cache evicts at that instant), and — because an
// announcement can be lost — the entry also knows when the grace it was built from ends, and never outlives it. The same goes for
// an override that was given an end.
func TestAGraceWindowAndAnOverridesEndAreBoundariesTheCacheNeverCrosses(t *testing.T) {
	t.Parallel()
	for name, announce := range map[string]bool{"announced": true, "announcement lost": false} {
		r := newRig(t, 24*time.Hour) // far longer than anything below
		r.watch(t)
		grace := contracts.T0.Add(time.Hour)
		r.mem.Subscribe(t, scope, "pro", domain.PastDue, &grace)
		if !r.allowed(t, scope, "sso") {
			t.Fatalf("%s: inside grace the plan holds", name)
		}
		r.clock.Advance(time.Hour - time.Nanosecond)
		if !r.allowed(t, scope, "sso") {
			t.Fatalf("%s: a nanosecond before the window closes the plan holds", name)
		}
		r.clock.Advance(time.Nanosecond)
		if announce {
			r.announce(t, scope, "grace_expired")
		}
		if r.allowed(t, scope, "sso") {
			t.Fatalf("%s: the grace window closed and the cached entitlement did not lapse", name)
		}
	}

	r := newRig(t, 24*time.Hour)
	end := contracts.T0.Add(2 * time.Hour)
	g := domain.Grant{Scope: scope, Key: "export", Value: domain.FlagValue(true), Source: domain.FromOverride, Reason: "trial", ExpiresAt: &end}
	if err := r.mem.PutOverride(context.Background(), g, "alice"); err != nil {
		t.Fatal(err)
	}
	if !r.allowed(t, scope, "export") {
		t.Fatal("an unexpired override applies")
	}
	r.clock.Advance(2*time.Hour - time.Nanosecond)
	if !r.allowed(t, scope, "export") {
		t.Fatal("a nanosecond before it ends an override applies")
	}
	r.clock.Advance(time.Nanosecond) // nobody announces an override's end
	if r.allowed(t, scope, "export") {
		t.Fatal("a cached override outlived its expiry instant")
	}
}

func TestAConfigurationAnnouncementEvictsEveryScopeOfItsRealmAndOnlyThat(t *testing.T) {
	t.Parallel()
	r := newRig(t, time.Hour)
	r.watch(t)
	a := md.Scope{Realm: "t1", Kind: "org", ID: "a"}
	b := md.Scope{Realm: "t1", Kind: "org", ID: "b"}
	c := md.Scope{Realm: "other", Kind: "org", ID: "a"}
	for _, s := range []md.Scope{a, b, c} {
		r.allowed(t, s, "sso")
	}
	if r.reads() != 3 {
		t.Fatalf("setup: %d reads", r.reads())
	}
	r.announceConfig(t, "t1", "plan_entitlements")
	r.allowed(t, a, "sso")
	r.allowed(t, b, "sso")
	if r.reads() != 5 {
		t.Fatalf("a configuration change of t1 must evict both of its scopes: %d reads", r.reads())
	}
	r.allowed(t, c, "sso")
	if r.reads() != 5 {
		t.Fatalf("t1's configuration change evicted another realm's scope: %d reads", r.reads())
	}
	if got := r.met.get("entitlement_cache_invalidations_total|source=realm"); got != 1 {
		t.Errorf("one realm announcement counted %d times", got)
	}
	// A change to something that belongs to no realm can affect any of them.
	r.announceConfig(t, "(global)", "provider_accounts")
	r.allowed(t, c, "sso")
	if r.reads() != 6 {
		t.Fatalf("a global configuration change must evict everything: %d reads", r.reads())
	}
}

// Invalidation is on billing.entitlement.<tag> and billing.config.<realm> and on NOTHING else (FR-035): a payment, a low
// balance, a warning, another deployment's subject, or a message that is not shaped like either does not evict. A bus that hands
// the handler more than it subscribed to is not a reason to throw the cache away.
func TestNothingElseInvalidates(t *testing.T) {
	t.Parallel()
	r := newRig(t, time.Hour)
	r.watch(t)
	r.allowed(t, scope, "sso")
	token := scope.SubjectToken()
	for _, subject := range []string{
		prefix + ".payment." + token, prefix + ".balance.low." + token, prefix + ".warn." + token, prefix + ".blocked." + token,
		prefix + ".webhook.received", prefix + ".reconcile.tick", prefix + ".dlq." + prefix + ".entitlement." + token,
		"other.entitlement." + token, "other.config.t1", // another deployment's namespace
		prefix + ".entitlement", prefix + ".config", // no token
		prefix + ".entitlement." + token + ".more", prefix + ".config.t1.more", // more tokens than a subject has
		prefix + ".entitlement.%zz", prefix + ".entitlement.no-realm-in-it", // not a scope's token
		prefix + ".config.NOT A REALM",
		prefix + "x.entitlement." + token, // a prefix that merely starts the same
	} {
		if err := r.bus.Inject(context.Background(), subject, []byte(`{"reason":"subscription"}`)); err != nil {
			t.Fatalf("%s: a message the handler does not own must be acknowledged, not redelivered for ever: %v", subject, err)
		}
		r.allowed(t, scope, "sso")
		if r.reads() != 1 {
			t.Fatalf("%q evicted the cache (%d reads)", subject, r.reads())
		}
	}
	// A subject with a raw dot in the token has more tokens than a subject has: it is not the encoded name of the scope whose id
	// has a dot in it, and must not be read as one.
	dotted := md.Scope{Realm: "t1", Kind: "org", ID: "o1.more"}
	r.allowed(t, dotted, "sso")
	base := r.reads()
	if err := r.bus.Inject(context.Background(), prefix+".entitlement.t1/org:o1.more", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	r.allowed(t, dotted, "sso")
	if r.reads() != base {
		t.Fatalf("a subject with a raw dot in its token evicted the scope it spells (%d reads, was %d)", r.reads(), base)
	}
	// And a message that is theirs, with a payload that is not what it should be, still evicts: the subject is the address.
	if err := r.bus.Inject(context.Background(), prefix+".entitlement."+token, []byte(`not json`)); err != nil {
		t.Fatal(err)
	}
	r.allowed(t, scope, "sso")
	if r.reads() != base+1 {
		t.Fatalf("an unreadable body on the right subject must evict (fail towards fresh): %d reads, was %d", r.reads(), base)
	}
}

// The subject carries the scope in a token that percent-encodes what a subject reserves. The handler must decode it to exactly
// that scope — a scope id with a dot or a space in it must evict itself, and no neighbour.
func TestTheSubjectTokenNamesExactlyOneScope(t *testing.T) {
	t.Parallel()
	ids := []string{"a", "a.b", "a b", "a*b", "a>b", "a%b", "a%2Eb", "a/b:c", "đồng-tiền", "..", "%"}
	for _, id := range ids {
		r := newRig(t, time.Hour)
		r.watch(t)
		target := md.Scope{Realm: "t1", Kind: "org", ID: id}
		if target.Validate() != nil {
			continue
		}
		others := []md.Scope{{Realm: "t1", Kind: "org", ID: "zzz"}, {Realm: "t1", Kind: "user", ID: id}, {Realm: "other", Kind: "org", ID: id}}
		for _, s := range append(others, target) {
			r.allowed(t, s, "sso")
		}
		base := r.reads()
		r.announce(t, target, "subscription")
		r.allowed(t, target, "sso")
		if r.reads() != base+1 {
			t.Errorf("id %q: its own announcement did not evict it (%d reads, was %d)", id, r.reads(), base)
		}
		for _, s := range others {
			r.allowed(t, s, "sso")
		}
		if r.reads() != base+1 {
			t.Errorf("id %q: its announcement evicted a neighbour %+v (%d reads)", id, tags(others), r.reads())
		}
	}
}

func tags(ss []md.Scope) []string {
	var out []string
	for _, s := range ss {
		out = append(out, s.Tag())
	}
	return out
}

// A load that started before a change and finishes after its announcement has read the old world. Its caller may be given that
// (the request raced the change), but it must not be KEPT.
func TestALoadThatRacesAnInvalidationIsNotCached(t *testing.T) {
	t.Parallel()
	r := newRig(t, time.Hour)
	r.watch(t)
	// Hold the LAST read of the load open: by then it has read the subscription as it was, so when the world changes under it,
	// what it holds is the old picture.
	r.store.lastEntered, r.store.lastGate = make(chan struct{}, 1), make(chan struct{})
	done := make(chan bool, 1)
	go func() {
		ok, _ := r.e.Allowed(context.Background(), scope, "sso")
		done <- ok
	}()
	select {
	case <-r.store.lastEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("the load never reached its last read")
	}
	r.mem.Subscribe(t, scope, "pro", domain.Active, nil) // the world changes while the load is in flight …
	r.announce(t, scope, "subscription")                 // … and says so
	close(r.store.lastGate)
	select {
	case ok := <-done:
		if ok {
			t.Log("the racing request read the new world; the test is only as strong as the interleaving, which the gate fixes")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the racing load never returned")
	}
	if !r.allowed(t, scope, "sso") {
		t.Fatal("the answer the racing load read before the change was kept: a customer who just upgraded is still on free")
	}
}

// A realm's configuration change races a load the same way.
func TestALoadThatRacesARealmAnnouncementIsNotCached(t *testing.T) {
	t.Parallel()
	r := newRig(t, time.Hour)
	r.watch(t)
	r.store.lastEntered, r.store.lastGate = make(chan struct{}, 1), make(chan struct{})
	done := make(chan struct{})
	go func() { _, _ = r.e.Allowed(context.Background(), scope, "sso"); close(done) }()
	<-r.store.lastEntered
	r.mem.Subscribe(t, scope, "pro", domain.Active, nil)
	r.announceConfig(t, "t1", "plan_entitlements")
	close(r.store.lastGate)
	<-done
	if !r.allowed(t, scope, "sso") {
		t.Fatal("a load that raced a configuration announcement was kept")
	}
}

// Concurrent misses for one scope are one read of the store, not one each: the moment an announcement evicts a hot scope is
// the moment every request for it arrives.
func TestConcurrentMissesForOneScopeShareOneLoad(t *testing.T) {
	t.Parallel()
	r := newRig(t, time.Hour)
	r.store.entered, r.store.gate = make(chan struct{}, 1), make(chan struct{})
	const n = 32
	var started, finished sync.WaitGroup
	started.Add(n)
	finished.Add(n)
	for range n {
		go func() {
			defer finished.Done()
			started.Done()
			if _, err := r.e.Allowed(context.Background(), scope, "sso"); err != nil {
				t.Error(err)
			}
		}()
	}
	started.Wait()
	<-r.store.entered
	time.Sleep(50 * time.Millisecond) // let the rest reach the cache and wait on the load in flight
	close(r.store.gate)
	finished.Wait()
	if r.reads() != 1 {
		t.Fatalf("%d concurrent checks of one scope read the store %d times; want 1", n, r.reads())
	}
}

// A caller that gives up does not take the others' load down with it.
func TestACallerThatCancelsDoesNotFailTheOthersLoad(t *testing.T) {
	t.Parallel()
	r := newRig(t, time.Hour)
	r.store.entered, r.store.gate = make(chan struct{}, 1), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := r.e.Allowed(ctx, scope, "sso"); first <- err }()
	<-r.store.entered
	second := make(chan error, 1)
	go func() { _, err := r.e.Allowed(context.Background(), scope, "sso"); second <- err }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("the caller that cancelled got %v; want context.Canceled", err)
	}
	close(r.store.gate)
	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("another caller's cancellation failed this one: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the surviving caller never got its answer")
	}
}

func TestTheCacheIsBounded(t *testing.T) {
	t.Parallel()
	r := newRig(t, time.Hour, func(d *app.Deps) { d.CacheMaxEntries = 3 })
	for i := range 20 {
		r.allowed(t, md.Scope{Realm: "t1", Kind: "org", ID: string(rune('a' + i))}, "sso")
		if n := r.e.CacheLen(); n > 3 {
			t.Fatalf("after %d scopes the cache holds %d entries; its bound is 3", i+1, n)
		}
	}
	if r.e.CacheLen() == 0 {
		t.Fatal("a bounded cache that holds nothing is not a cache")
	}
}

// What cannot be answered is never remembered. A warm entry is served through an outage until its TTL — that is what bounded
// staleness means — and after it the outage is an error and not a permit.
func TestErrorsAreNeverCachedAndAnOutageEndsAWarmEntryAtItsTTL(t *testing.T) {
	t.Parallel()
	boom := errors.New("connection refused")
	r := newRig(t, 30*time.Second)
	r.faults.BreakWith(boom)
	if _, err := r.e.Allowed(context.Background(), scope, "sso"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
	r.faults.Heal()
	if r.allowed(t, scope, "sso") {
		t.Fatal("free does not include sso")
	}
	r.faults.BreakWith(boom)
	if r.allowed(t, scope, "sso") {
		t.Fatal("the warm entry said free does not include sso")
	}
	r.clock.Advance(30 * time.Second)
	if ok, err := r.e.Allowed(context.Background(), scope, "sso"); ok || !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("past the TTL an outage must be an error and not a permit: %v, %v", ok, err)
	}
}

func TestTheCacheReportsHitsAndMisses(t *testing.T) {
	t.Parallel()
	r := newRig(t, time.Hour)
	for range 4 {
		r.allowed(t, scope, "sso")
	}
	if miss, hit := r.met.get("entitlement_cache_total|result=miss"), r.met.get("entitlement_cache_total|result=hit"); miss != 1 || hit != 3 {
		t.Errorf("1 miss and 3 hits expected; got %d and %d", miss, hit)
	}
}

// Watch joins the two subjects under the group it is given — one per replica, since every replica must see every announcement —
// and a Watch that cannot join both joins neither.
func TestWatchSubscribesToExactlyTheTwoSubjects(t *testing.T) {
	t.Parallel()
	r := newRig(t, time.Hour)
	w, err := r.e.Watch(context.Background(), r.bus, "acme", "entitlement-cache-pod-7")
	if err != nil {
		t.Fatal(err)
	}
	live := r.bus.live()
	if len(live) != 2 {
		t.Fatalf("%d subscriptions; want 2", len(live))
	}
	got := map[string]string{}
	for _, s := range live {
		got[s.pattern] = s.group
	}
	if got["acme.entitlement.>"] != "entitlement-cache-pod-7" || got["acme.config.>"] != "entitlement-cache-pod-7" || len(got) != 2 {
		t.Errorf("subscribed to %v; want acme.entitlement.> and acme.config.> under the replica's group", got)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if len(r.bus.live()) != 0 {
		t.Error("Close left a subscription open")
	}
	if err := w.Close(); err != nil {
		t.Errorf("Close twice: %v", err)
	}

	// Cannot join the second: the first is closed.
	r2 := newRig(t, time.Hour)
	r2.bus.failSub = 2
	if _, err := r2.e.Watch(context.Background(), r2.bus, prefix, "g"); err == nil {
		t.Fatal("a Watch that could not join both reported success")
	}
	if len(r2.bus.live()) != 0 {
		t.Error("the half-joined Watch left its first subscription open")
	}
	// Things a Watch cannot be built from.
	for name, f := range map[string]func() error{
		"no bus":    func() error { _, err := r.e.Watch(context.Background(), nil, prefix, "g"); return err },
		"no prefix": func() error { _, err := r.e.Watch(context.Background(), r.bus, "", "g"); return err },
		"no group":  func() error { _, err := r.e.Watch(context.Background(), r.bus, prefix, ""); return err },
	} {
		if err := f(); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
	// An Entitler with no cache has nothing to evict, and says so rather than subscribing to nothing.
	if _, err := newRig(t, 0).e.Watch(context.Background(), r.bus, prefix, "g"); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("Watch of an Entitler with no cache: want ErrInvalid, got %v", err)
	}
}

// Evictions race with checks all day. Under -race, many checks of many scopes while announcements land must not corrupt the
// cache, return an error, or deadlock.
func TestConcurrentChecksAndAnnouncementsAreSafe(t *testing.T) {
	t.Parallel()
	r := newRig(t, time.Hour, func(d *app.Deps) { d.CacheMaxEntries = 8 })
	r.watch(t)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				s := md.Scope{Realm: "t1", Kind: "org", ID: string(rune('a' + (i+g)%12))}
				if _, err := r.e.Allowed(context.Background(), s, "sso"); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	for i := range 200 {
		switch i % 3 {
		case 0:
			r.announce(t, md.Scope{Realm: "t1", Kind: "org", ID: string(rune('a' + i%12))}, "subscription")
		case 1:
			r.announceConfig(t, "t1", "plan_entitlements")
		default:
			r.clock.Advance(time.Minute)
		}
	}
	close(stop)
	wg.Wait()
}
