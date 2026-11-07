package contracts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// AdminOpFactory builds a fresh, empty store that reads time from the clock it is given.
type AdminOpFactory func(tb testing.TB, clock *FakeClock) ports.AdminOperationStore

// AdminOpCase is one named obligation of the queue of operator requests.
type AdminOpCase struct {
	Name string
	Run  func(tb testing.TB, f AdminOpFactory)
}

// The names of the cases, so a test that plants a defect can say which case must catch it.
const (
	CaseAORecorded        = "a request is recorded whole: kind, params, actor, reason, key and who sent it, and waits as pending"
	CaseAOOnce            = "the same request under the same key is one operation, and says it was not created again"
	CaseAOOnceTogether    = "the same request from sixteen places at once is one operation and one creation"
	CaseAOKeyReuse        = "a key reused for a different request is refused as a conflict and changes nothing"
	CaseAORealmIsolation  = "a key is a realm's own, and an operation cannot be read from another realm"
	CaseAOOldestFirst     = "claims go to the oldest request first, and each operation is claimed once"
	CaseAOClaimTogether   = "sixteen workers claiming at once: every operation goes to exactly one of them"
	CaseAOLeaseLapses     = "a claim whose worker died lapses: the next claim takes the operation again and counts the attempt"
	CaseAOFinishOnce      = "an outcome is recorded once, by the attempt that holds the claim: a stale attempt and a second outcome are refused"
	CaseAOFailedStays     = "a failure is recorded with its text and is never claimed again"
	CaseAOUnknown         = "an operation that does not exist is not found"
	CaseAOBacklog         = "the backlog counts what is waiting and how long the oldest has waited, and nothing that is in hand or done"
	CaseAOJSONRoundTrip   = "params and a result come back as the same JSON whatever the order of their keys"
	CaseAOLeaseIsPositive = "a claim needs a positive lease"
)

// AdminOpCases are the obligations of an AdminOperationStore (R112): a durable queue of operator requests whose every entry has a person
// and a reason, is made once however often it is asked for, and is worked by exactly one worker at a time.
func AdminOpCases() []AdminOpCase {
	return []AdminOpCase{
		{CaseAORecorded, aoRecorded},
		{CaseAOOnce, aoOnce},
		{CaseAOOnceTogether, aoOnceTogether},
		{CaseAOKeyReuse, aoKeyReuse},
		{CaseAORealmIsolation, aoRealmIsolation},
		{CaseAOOldestFirst, aoOldestFirst},
		{CaseAOClaimTogether, aoClaimTogether},
		{CaseAOLeaseLapses, aoLeaseLapses},
		{CaseAOFinishOnce, aoFinishOnce},
		{CaseAOFailedStays, aoFailedStays},
		{CaseAOUnknown, aoUnknown},
		{CaseAOBacklog, aoBacklog},
		{CaseAOJSONRoundTrip, aoJSONRoundTrip},
		{CaseAOLeaseIsPositive, aoLeaseIsPositive},
	}
}

// AdminOpContract runs every case, each against a store of its own, in parallel.
func AdminOpContract(t *testing.T, f AdminOpFactory) {
	t.Helper()
	for _, c := range AdminOpCases() {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			c.Run(t, f)
		})
	}
}

const aoLease = time.Minute

func aoWorld(tb testing.TB, f AdminOpFactory) (ports.AdminOperationStore, *FakeClock) {
	tb.Helper()
	clock := NewFakeClock(time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC))
	return f(tb, clock), clock
}

func aoReq(realm, key string) ports.AdminOperation {
	return ports.AdminOperation{
		Realm: realm, Kind: ports.AdminReconcile, Params: json.RawMessage(`{"shard":3,"kind":"full"}`),
		Actor: "alice@example.com", Reason: "INC-4412: the full sweep after the failover", Key: key, Via: "ops-console",
	}
}

func aoSubmit(tb testing.TB, s ports.AdminOperationStore, op ports.AdminOperation) (ports.AdminOperation, bool) {
	tb.Helper()
	got, created, err := s.Submit(context.Background(), op)
	if err != nil {
		tb.Fatalf("Submit(%s/%s): %v", op.Realm, op.Key, err)
	}
	return got, created
}

func aoGet(tb testing.TB, s ports.AdminOperationStore, realm, id string) ports.AdminOperation {
	tb.Helper()
	got, err := s.Get(context.Background(), realm, id)
	if err != nil {
		tb.Fatalf("Get(%s/%s): %v", realm, id, err)
	}
	return got
}

func aoClaim(tb testing.TB, s ports.AdminOperationStore) (ports.AdminOperation, bool) {
	tb.Helper()
	got, ok, err := s.Claim(context.Background(), aoLease)
	if err != nil {
		tb.Fatalf("Claim: %v", err)
	}
	return got, ok
}

func aoSameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

func aoRecorded(tb testing.TB, f AdminOpFactory) {
	s, clock := aoWorld(tb, f)
	want := aoReq("default", "key-1")
	got, created := aoSubmit(tb, s, want)
	if !created || got.ID == "" || got.Status != ports.AdminPending || got.Attempts != 0 || !got.CreatedAt.Equal(clock.Now()) || got.StartedAt != nil || got.FinishedAt != nil {
		tb.Fatalf("a new request came back as %+v (created %v)", got, created)
	}
	if got.Realm != want.Realm || got.Kind != want.Kind || got.Actor != want.Actor || got.Reason != want.Reason || got.Key != want.Key || got.Via != want.Via || !aoSameJSON(got.Params, want.Params) {
		tb.Fatalf("the request was not recorded whole: sent %+v, got %+v", want, got)
	}
	if read := aoGet(tb, s, "default", got.ID); !reflect.DeepEqual(read, got) {
		tb.Fatalf("what Submit returned and what Get reads differ:\n%+v\n%+v", got, read)
	}
}

func aoOnce(tb testing.TB, f AdminOpFactory) {
	s, clock := aoWorld(tb, f)
	first, created := aoSubmit(tb, s, aoReq("default", "key-1"))
	clock.Advance(time.Minute)
	second, again := aoSubmit(tb, s, aoReq("default", "key-1"))
	if !created || again || second.ID != first.ID || !second.CreatedAt.Equal(first.CreatedAt) {
		tb.Fatalf("a retry made %+v (created %v) after %+v (created %v)", second, again, first, created)
	}
	if b, err := s.Backlog(context.Background()); err != nil || b.Waiting != 1 {
		tb.Fatalf("backlog = %+v, %v: a retry must not queue the work twice", b, err)
	}
}

func aoOnceTogether(tb testing.TB, f AdminOpFactory) {
	s, _ := aoWorld(tb, f)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ids := map[string]bool{}
	created := 0
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, c, err := s.Submit(context.Background(), aoReq("default", "key-1"))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				tb.Errorf("Submit: %v", err)
				return
			}
			ids[got.ID] = true
			if c {
				created++
			}
		}()
	}
	wg.Wait()
	if len(ids) != 1 || created != 1 {
		tb.Fatalf("sixteen identical requests made %d operations and %d creations, want 1 and 1", len(ids), created)
	}
}

func aoKeyReuse(tb testing.TB, f AdminOpFactory) {
	s, _ := aoWorld(tb, f)
	first, _ := aoSubmit(tb, s, aoReq("default", "key-1"))
	variants := map[string]func(*ports.AdminOperation){
		"params": func(o *ports.AdminOperation) { o.Params = json.RawMessage(`{"shard":4,"kind":"full"}`) },
		"kind":   func(o *ports.AdminOperation) { o.Kind = ports.AdminRecover },
		"actor":  func(o *ports.AdminOperation) { o.Actor = "mallory@example.com" },
		"reason": func(o *ports.AdminOperation) { o.Reason = "a different story" },
	}
	for name, mutate := range variants {
		op := aoReq("default", "key-1")
		mutate(&op)
		if _, created, err := s.Submit(context.Background(), op); !errors.Is(err, domain.ErrIdemConflict) || created {
			tb.Fatalf("a key reused with a different %s: created %v, err %v, want %v", name, created, err, domain.ErrIdemConflict)
		}
	}
	if read := aoGet(tb, s, "default", first.ID); !reflect.DeepEqual(read, first) {
		tb.Fatalf("a refused request changed the operation: %+v became %+v", first, read)
	}
}

func aoRealmIsolation(tb testing.TB, f AdminOpFactory) {
	s, _ := aoWorld(tb, f)
	a, _ := aoSubmit(tb, s, aoReq("default", "key-1"))
	b, created := aoSubmit(tb, s, aoReq("other", "key-1"))
	if !created || b.ID == a.ID {
		tb.Fatalf("the same key in another realm was the same operation: %+v and %+v", a, b)
	}
	if _, err := s.Get(context.Background(), "other", a.ID); !errors.Is(err, domain.ErrNotFound) {
		tb.Fatalf("a realm read another realm's operation: %v", err)
	}
	if got := aoGet(tb, s, "other", b.ID); got.Realm != "other" {
		tb.Fatalf("an operation lost its realm: %+v", got)
	}
}

func aoOldestFirst(tb testing.TB, f AdminOpFactory) {
	s, clock := aoWorld(tb, f)
	var order []string
	for _, k := range []string{"k1", "k2", "k3"} {
		got, _ := aoSubmit(tb, s, aoReq("default", k))
		order = append(order, got.ID)
		clock.Advance(time.Second)
	}
	for i, want := range order {
		got, ok := aoClaim(tb, s)
		if !ok || got.ID != want || got.Status != ports.AdminRunning || got.Attempts != 1 || got.StartedAt == nil {
			tb.Fatalf("claim %d: %+v (ok %v), want %s running on its first attempt", i+1, got, ok, want)
		}
	}
	if got, ok := aoClaim(tb, s); ok {
		tb.Fatalf("a fourth claim found %+v: every operation is claimed once", got)
	}
}

func aoClaimTogether(tb testing.TB, f AdminOpFactory) {
	s, clock := aoWorld(tb, f)
	const n = 8
	for i := range n {
		aoSubmit(tb, s, aoReq("default", fmt.Sprintf("k%d", i)))
		clock.Advance(time.Millisecond)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	claimed := map[string]int{}
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 2 * n { // bounded: a queue that keeps handing out the same operation must be caught, not waited for
				got, ok, err := s.Claim(context.Background(), aoLease)
				if err != nil {
					tb.Errorf("Claim: %v", err)
					return
				}
				if !ok {
					return
				}
				mu.Lock()
				claimed[got.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(claimed) != n {
		tb.Fatalf("%d of %d operations were claimed", len(claimed), n)
	}
	for id, c := range claimed {
		if c != 1 {
			tb.Fatalf("operation %s was claimed %d times at once", id, c)
		}
	}
}

func aoLeaseLapses(tb testing.TB, f AdminOpFactory) {
	s, clock := aoWorld(tb, f)
	op, _ := aoSubmit(tb, s, aoReq("default", "key-1"))
	first, ok := aoClaim(tb, s)
	if !ok || first.ID != op.ID || first.Attempts != 1 {
		tb.Fatalf("first claim: %+v (ok %v)", first, ok)
	}
	clock.Advance(aoLease - time.Second)
	if got, ok := aoClaim(tb, s); ok {
		tb.Fatalf("an operation whose lease is live was claimed again: %+v", got)
	}
	clock.Advance(2 * time.Second)
	again, ok := aoClaim(tb, s)
	if !ok || again.ID != op.ID || again.Attempts != 2 || again.Status != ports.AdminRunning {
		tb.Fatalf("after the lease lapsed: %+v (ok %v), want the same operation on its second attempt", again, ok)
	}
}

func aoFinishOnce(tb testing.TB, f AdminOpFactory) {
	s, clock := aoWorld(tb, f)
	op, _ := aoSubmit(tb, s, aoReq("default", "key-1"))
	aoClaim(tb, s)
	clock.Advance(aoLease + time.Second)
	second, _ := aoClaim(tb, s)
	result := json.RawMessage(`{"outcome":"ran"}`)
	if ok, err := s.Finish(context.Background(), op.ID, 1, ports.AdminSucceeded, result, ""); err != nil || ok {
		tb.Fatalf("the attempt that lost its lease recorded an outcome: %v, %v", ok, err)
	}
	if read := aoGet(tb, s, "default", op.ID); read.Status != ports.AdminRunning || read.FinishedAt != nil {
		tb.Fatalf("a stale attempt changed the operation: %+v", read)
	}
	clock.Advance(time.Second)
	if ok, err := s.Finish(context.Background(), op.ID, second.Attempts, ports.AdminSucceeded, result, ""); err != nil || !ok {
		tb.Fatalf("the attempt that holds the claim could not record its outcome: %v, %v", ok, err)
	}
	if ok, err := s.Finish(context.Background(), op.ID, second.Attempts, ports.AdminFailed, nil, "too late"); err != nil || ok {
		tb.Fatalf("a second outcome was recorded: %v, %v", ok, err)
	}
	read := aoGet(tb, s, "default", op.ID)
	if read.Status != ports.AdminSucceeded || read.FinishedAt == nil || !read.FinishedAt.Equal(clock.Now()) || !aoSameJSON(read.Result, result) || read.Error != "" {
		tb.Fatalf("the outcome was not kept as recorded: %+v", read)
	}
}

func aoFailedStays(tb testing.TB, f AdminOpFactory) {
	s, clock := aoWorld(tb, f)
	op, _ := aoSubmit(tb, s, aoReq("default", "key-1"))
	got, _ := aoClaim(tb, s)
	if ok, err := s.Finish(context.Background(), op.ID, got.Attempts, ports.AdminFailed, nil, "the shard is not ours"); err != nil || !ok {
		tb.Fatalf("Finish: %v, %v", ok, err)
	}
	clock.Advance(24 * time.Hour)
	if again, ok := aoClaim(tb, s); ok {
		tb.Fatalf("a failed operation was claimed again: %+v: a failure is the operator's to look at, and a new request is a new key", again)
	}
	if read := aoGet(tb, s, "default", op.ID); read.Status != ports.AdminFailed || read.Error != "the shard is not ours" || read.FinishedAt == nil {
		tb.Fatalf("a failure was not kept: %+v", read)
	}
}

func aoUnknown(tb testing.TB, f AdminOpFactory) {
	s, _ := aoWorld(tb, f)
	for _, id := range []string{"00000000-0000-4000-8000-000000000000", "not-a-uuid", ""} {
		if _, err := s.Get(context.Background(), "default", id); !errors.Is(err, domain.ErrNotFound) {
			tb.Fatalf("Get(%q): %v, want %v", id, err, domain.ErrNotFound)
		}
	}
}

func aoBacklog(tb testing.TB, f AdminOpFactory) {
	s, clock := aoWorld(tb, f)
	if b, err := s.Backlog(context.Background()); err != nil || b.Waiting != 0 || b.Oldest != 0 {
		tb.Fatalf("an empty queue has a backlog of %+v, %v", b, err)
	}
	aoSubmit(tb, s, aoReq("default", "k1"))
	clock.Advance(90 * time.Second)
	aoSubmit(tb, s, aoReq("default", "k2"))
	if b, err := s.Backlog(context.Background()); err != nil || b.Waiting != 2 || b.Oldest != 90*time.Second {
		tb.Fatalf("backlog = %+v, %v, want two waiting and the oldest 90s", b, err)
	}
	first, _ := aoClaim(tb, s)
	if b, _ := s.Backlog(context.Background()); b.Waiting != 1 || b.Oldest != 0 {
		tb.Fatalf("backlog = %+v with one in hand and one that has only just arrived, want one waiting and the oldest 0", b)
	}
	if _, err := s.Finish(context.Background(), first.ID, first.Attempts, ports.AdminSucceeded, nil, ""); err != nil {
		tb.Fatal(err)
	}
	clock.Advance(aoLease + time.Second)
	if b, _ := s.Backlog(context.Background()); b.Waiting != 1 {
		tb.Fatalf("backlog = %+v, want the one still pending: a finished operation is not waiting", b)
	}
	aoClaim(tb, s)
	clock.Advance(aoLease + time.Second)
	if b, _ := s.Backlog(context.Background()); b.Waiting != 1 {
		tb.Fatalf("backlog = %+v, want an operation whose worker died to wait again", b)
	}
}

func aoJSONRoundTrip(tb testing.TB, f AdminOpFactory) {
	s, _ := aoWorld(tb, f)
	op := aoReq("default", "key-1")
	op.Params = json.RawMessage(`{"z":[3,2,1],"a":{"y":"é","x":null},"n":12345678901234567}`)
	got, _ := aoSubmit(tb, s, op)
	if !aoSameJSON(got.Params, op.Params) {
		tb.Fatalf("params came back as %s", got.Params)
	}
	claimed, _ := aoClaim(tb, s)
	result := json.RawMessage(`{"shards":[1,2],"note":"done"}`)
	if ok, err := s.Finish(context.Background(), claimed.ID, claimed.Attempts, ports.AdminSucceeded, result, ""); err != nil || !ok {
		tb.Fatalf("Finish: %v, %v", ok, err)
	}
	if read := aoGet(tb, s, "default", got.ID); !aoSameJSON(read.Result, result) || !aoSameJSON(read.Params, op.Params) {
		tb.Fatalf("params %s and result %s came back as %s and %s", op.Params, result, read.Params, read.Result)
	}
}

func aoLeaseIsPositive(tb testing.TB, f AdminOpFactory) {
	s, _ := aoWorld(tb, f)
	aoSubmit(tb, s, aoReq("default", "key-1"))
	for _, lease := range []time.Duration{0, -time.Second} {
		if _, ok, err := s.Claim(context.Background(), lease); err == nil || ok || !errors.Is(err, domain.ErrInvalid) {
			tb.Fatalf("Claim(%v): ok %v, err %v, want %v: a claim that lapses at once is a claim two workers share", lease, ok, err, domain.ErrInvalid)
		}
	}
}
