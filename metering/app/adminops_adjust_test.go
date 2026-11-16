package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/app"
	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// R112a: an adjustment is an investigated correction to the books, made by a person, as a signed Grant with the reserved reason
// admin_adjustment. It moves money, so what a request may carry is narrow — one scope, one pool, one non-zero integer within what an
// operation may move — and the key its grant is booked under is the operation's, never the caller's: a retry of the operation, or the
// operation done again by a worker that took it over, is the same grant, and cannot book twice.

const adjScope = `{"kind":"workspace","id":"w1"}`

func TestAnAdjustmentNamesAScopeAPoolAndASignedAmountAndIsStoredInOneCanonicalForm(t *testing.T) {
	t.Parallel()
	r := newAdminRig(t)
	for i, g := range []struct{ params, want string }{
		{`{"scope":` + adjScope + `,"amount":500}`, `{"scope":` + adjScope + `,"pool":"general","amount":500}`},
		{`{"amount":-30,"pool":"promo","scope":{"id":"w1","kind":"workspace"}}`, `{"scope":` + adjScope + `,"pool":"promo","amount":-30}`},
		{`{"scope":` + adjScope + `,"pool":"","amount":1}`, `{"scope":` + adjScope + `,"pool":"general","amount":1}`},
		{` { "scope" : ` + adjScope + ` , "amount" : 7 } `, `{"scope":` + adjScope + `,"pool":"general","amount":7}`},
		{fmt.Sprintf(`{"scope":%s,"amount":%d}`, adjScope, domain.MaxOperationAmount), fmt.Sprintf(`{"scope":%s,"pool":"general","amount":%d}`, adjScope, domain.MaxOperationAmount)},
		{fmt.Sprintf(`{"scope":%s,"amount":-%d}`, adjScope, domain.MaxOperationAmount), fmt.Sprintf(`{"scope":%s,"pool":"general","amount":-%d}`, adjScope, domain.MaxOperationAmount)},
	} {
		op := r.submit(t, req(ports.AdminAdjust, g.params, fmt.Sprintf("good-%d", i)))
		if op.Kind != ports.AdminAdjust || string(op.Params) != g.want {
			t.Errorf("%s was stored as %s %s, want %s", g.params, op.Kind, op.Params, g.want)
		}
	}
}

func TestAnAdjustmentThatCannotBeReadExactlyIsRefusedAndNeverQueued(t *testing.T) {
	t.Parallel()
	r := newAdminRig(t)
	bad := []string{
		``, `not json`, `[1]`, `null`, `{}`,
		`{"scope":` + adjScope + `}`, // no amount
		`{"amount":5}`,               // no scope
		`{"scope":{},"amount":5}`, `{"scope":null,"amount":5}`, `{"scope":{"kind":"workspace"},"amount":5}`,
		`{"scope":` + adjScope + `,"amount":0}`,    // changes nothing
		`{"scope":` + adjScope + `,"amount":null}`, // not an amount
		`{"scope":` + adjScope + `,"amount":1.5}`,  // credits are whole
		`{"scope":` + adjScope + `,"amount":1e3}`,  // and written as integers
		`{"scope":` + adjScope + `,"amount":"5"}`,  // and numbers
		`{"scope":` + adjScope + `,"amount":true}`,
		fmt.Sprintf(`{"scope":%s,"amount":%d}`, adjScope, int64(domain.MaxOperationAmount)+1),
		fmt.Sprintf(`{"scope":%s,"amount":-%d}`, adjScope, int64(domain.MaxOperationAmount)+1),
		fmt.Sprintf(`{"scope":%s,"amount":%d}`, adjScope, int64(math.MinInt64)),
		`{"scope":` + adjScope + `,"amount":9223372036854775808}`, // one past int64
		`{"scope":` + adjScope + `,"amount":5,"pool":"Not A Pool"}`, `{"scope":` + adjScope + `,"amount":5,"pool":"a b"}`,
		`{"scope":` + adjScope + `,"amount":5,"idem_key":"mine"}`, // the key is the operation's, never the caller's
		`{"scope":` + adjScope + `,"amount":5,"reason":"x"}`,      // the reason is reserved, and the operator's is the request's
		`{"scope":` + adjScope + `,"amount":5,"expires_at":"2027-01-01T00:00:00Z"}`,
		`{"scope":{"realm":"other","kind":"workspace","id":"w1"},"amount":5}`, // a realm is the request's, never the arguments'
		`{"scope":` + adjScope + `,"amount":5} {"scope":` + adjScope + `,"amount":6}`,
	}
	for i, p := range bad {
		if _, created, err := r.ops.Submit(t.Context(), req(ports.AdminAdjust, p, fmt.Sprintf("bad-%d", i))); !errors.Is(err, domain.ErrInvalid) || created {
			t.Errorf("adjust %q: created %v, err %v, want %v", p, created, err, domain.ErrInvalid)
		}
	}
	if b, _ := r.store.Backlog(t.Context()); b.Waiting != 0 {
		t.Fatalf("%d requests wait after every one was refused", b.Waiting)
	}
}

// An operation may move no more than the deployment says one operation may, and the request is refused for it where it is read — not after
// it has waited for a worker and failed at the hot tier.
func TestAnAdjustmentIsLimitedByWhatTheDeploymentLetsOneOperationMove(t *testing.T) {
	t.Parallel()
	clock := contracts.NewFakeClock(time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC))
	ops, err := app.NewAdminOperations(app.AdminOperationsDeps{Store: contracts.NewMemAdminOps(clock), Shards: rigShards, MaxAmount: 1_000})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		amount int64
		ok     bool
	}{{1_000, true}, {-1_000, true}, {1_001, false}, {-1_001, false}} {
		q := req(ports.AdminAdjust, fmt.Sprintf(`{"scope":%s,"amount":%d}`, adjScope, c.amount), fmt.Sprintf("k%d", c.amount))
		if _, _, err := ops.Submit(t.Context(), q); (err == nil) != c.ok || (err != nil && !errors.Is(err, domain.ErrInvalid)) {
			t.Errorf("an adjustment of %d: %v, want ok=%v", c.amount, err, c.ok)
		}
	}
}

// The same request under the same key is one adjustment however its JSON is written; the same key for another amount is a conflict.
func TestARetriedAdjustmentIsTheSameOperationAndAnotherAmountUnderTheKeyIsAConflict(t *testing.T) {
	t.Parallel()
	r := newAdminRig(t)
	first := r.submit(t, req(ports.AdminAdjust, `{"scope":`+adjScope+`,"amount":500}`, "k-1"))
	again, created, err := r.ops.Submit(t.Context(), req(ports.AdminAdjust, `{"amount":500,"pool":"general","scope":{"id":"w1","kind":"workspace"}}`, "k-1"))
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("a retry made %+v (created %v, err %v) after %s", again, created, err, first.ID)
	}
	for name, p := range map[string]string{
		"another amount": `{"scope":` + adjScope + `,"amount":501}`,
		"the opposite":   `{"scope":` + adjScope + `,"amount":-500}`,
		"another pool":   `{"scope":` + adjScope + `,"amount":500,"pool":"promo"}`,
		"another scope":  `{"scope":{"kind":"workspace","id":"w2"},"amount":500}`,
	} {
		if _, _, err := r.ops.Submit(t.Context(), req(ports.AdminAdjust, p, "k-1")); !errors.Is(err, domain.ErrIdemConflict) {
			t.Errorf("the key reused for %s: %v, want %v", name, err, domain.ErrIdemConflict)
		}
	}
}

// ------------------------------------------------------------------ the executor --

// fakeGranter is a hot tier that tells a replay from a conflict the way the real one does: by the key and the fingerprint.
type fakeGranter struct {
	mu       sync.Mutex
	got      []domain.Grant
	byKey    map[string]string
	err      error
	writeoff domain.Credits
	balance  domain.Credits
}

func newFakeGranter() *fakeGranter { return &fakeGranter{byKey: map[string]string{}, balance: 1_000} }

func (f *fakeGranter) Grant(_ context.Context, g domain.Grant) (domain.Receipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, g)
	if f.err != nil {
		return domain.Receipt{}, f.err
	}
	if fp, seen := f.byKey[g.IdemKey]; seen {
		if fp != g.Fingerprint() {
			return domain.Receipt{}, domain.ErrIdemConflict
		}
		return domain.Receipt{IdemKey: g.IdemKey, Applied: false, Seq: 8}, nil
	}
	f.byKey[g.IdemKey] = g.Fingerprint()
	f.balance += g.Amount + f.writeoff
	return domain.Receipt{IdemKey: g.IdemKey, Applied: true, Delta: g.Amount + f.writeoff, Balance: f.balance, Seq: 8, Writeoff: f.writeoff}, nil
}

func (f *fakeGranter) grants() []domain.Grant {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.Grant(nil), f.got...)
}

func adjustOp(id, params string) ports.AdminOperation {
	return ports.AdminOperation{
		ID: id, Realm: "acme", Kind: ports.AdminAdjust, Params: json.RawMessage(params), Actor: "alice@example.com",
		Reason: "INC-4412: the chargeback was booked twice", Key: "ticket-4412-a",
	}
}

const (
	opA       = "5b0e6f2a-1c3d-4e5f-8a9b-0c1d2e3f4a5b"
	opB       = "6c1f7a3b-2d4e-4f60-9bac-1d2e3f4a5b6c"
	adjParams = `{"scope":` + adjScope + `,"pool":"promo","amount":-30}`
)

func TestAnAdjustmentIsASignedGrantWithTheReservedReasonAndAKeyOfTheOperation(t *testing.T) {
	t.Parallel()
	g := newFakeGranter()
	if _, err := app.AdjustmentExecutor(g)(t.Context(), adjustOp(opA, adjParams)); err != nil {
		t.Fatal(err)
	}
	got := g.grants()
	if len(got) != 1 {
		t.Fatalf("%d grants were made", len(got))
	}
	want := domain.Grant{
		Scope: domain.Scope{Realm: "acme", Kind: "workspace", ID: "w1"}, Pool: "promo", Amount: -30,
		IdemKey: "admin-adjustment:" + opA, Reason: ports.OperationAdjustment,
	}
	x := got[0]
	if x.Scope != want.Scope || x.Pool != want.Pool || x.Amount != want.Amount || x.IdemKey != want.IdemKey || x.Reason != "admin_adjustment" ||
		x.ExpiresAt != nil || x.SourceKey != "" {
		t.Fatalf("the grant was %+v, want %+v", x, want)
	}
	if x.Ref["operation"] != opA || x.Ref["actor"] != "alice@example.com" {
		t.Fatalf("the grant's ref is %v: the ledger row must lead back to the operation and the person", x.Ref)
	}
}

// The scope's realm is the operation's: an adjustment is never booked in a realm the request was not made in.
func TestAnAdjustmentIsBookedInTheRealmOfTheOperationAndNoOther(t *testing.T) {
	t.Parallel()
	g := newFakeGranter()
	op := adjustOp(opA, adjParams)
	op.Realm = "tenant-b"
	if _, err := app.AdjustmentExecutor(g)(t.Context(), op); err != nil {
		t.Fatal(err)
	}
	if got := g.grants(); len(got) != 1 || got[0].Scope.Realm != "tenant-b" {
		t.Fatalf("the grant was %+v, want realm tenant-b", got)
	}
}

// A worker that dies after the grant and before it says so leaves the operation to the next worker, which runs it again: the same key, the
// same fingerprint, a replay — and not a second adjustment.
func TestAnOperationDoneAgainBooksNothingMore(t *testing.T) {
	t.Parallel()
	g := newFakeGranter()
	ex := app.AdjustmentExecutor(g)
	op := adjustOp(opA, adjParams)
	first, err := ex(t.Context(), op)
	if err != nil {
		t.Fatal(err)
	}
	// The next worker finds it as the first left it, claimed once already: the attempts say so, and the key must not.
	taken := op
	taken.Attempts = 2
	at := time.Date(2026, 11, 16, 9, 10, 0, 0, time.UTC)
	taken.StartedAt = &at
	second, err := ex(t.Context(), taken)
	if err != nil {
		t.Fatalf("running the same operation again: %v", err)
	}
	got := g.grants()
	if len(got) != 2 || got[0].IdemKey != got[1].IdemKey || got[0].Fingerprint() != got[1].Fingerprint() {
		t.Fatalf("the two runs made grants %+v: they must be the same grant", got)
	}
	if len(g.byKey) != 1 || g.balance != 970 {
		t.Fatalf("%d adjustments were booked and the balance is %d, want 1 and 970", len(g.byKey), g.balance)
	}
	var a, b struct{ Applied bool }
	_ = json.Unmarshal(first, &a)
	_ = json.Unmarshal(second, &b)
	if !a.Applied || b.Applied {
		t.Fatalf("the first run says applied=%v and the second applied=%v: want true, then false (a replay)", a.Applied, b.Applied)
	}
}

// Two operations are two adjustments, even when they ask for the same thing: the key is the operation's, and an operator who means it twice
// asks twice.
func TestTwoOperationsAreTwoAdjustmentsEvenWhenTheyAskForTheSame(t *testing.T) {
	t.Parallel()
	g := newFakeGranter()
	ex := app.AdjustmentExecutor(g)
	for _, id := range []string{opA, opB} {
		if _, err := ex(t.Context(), adjustOp(id, adjParams)); err != nil {
			t.Fatal(err)
		}
	}
	if len(g.byKey) != 2 || g.balance != 940 {
		t.Fatalf("%d adjustments were booked and the balance is %d, want 2 and 940", len(g.byKey), g.balance)
	}
}

func TestAnAdjustmentReportsWhatTheHotTierSaid(t *testing.T) {
	t.Parallel()
	g := newFakeGranter()
	g.writeoff = 20 // a clamp floored 20 of a clawback that was larger than the balance
	out, err := app.AdjustmentExecutor(g)(t.Context(), adjustOp(opA, adjParams))
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Scope    app.ScopeParams `json:"scope"`
		Pool     string          `json:"pool"`
		Amount   int64           `json:"amount"`
		Applied  bool            `json:"applied"`
		Delta    int64           `json:"delta"`
		Writeoff int64           `json:"writeoff"`
		Balance  int64           `json:"balance"`
		IdemKey  string          `json:"idem_key"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatal(err)
	}
	if r.Scope != (app.ScopeParams{Kind: "workspace", ID: "w1"}) || r.Pool != "promo" || r.Amount != -30 || !r.Applied || r.Delta != -10 || r.Writeoff != 20 || r.Balance != 990 ||
		r.IdemKey != "admin-adjustment:"+opA {
		t.Fatalf("the result is %s: want the request, what the pool took (-10 after a write-off of 20), the balance and the key", out)
	}
}

// What the hot tier refused changed nothing, and the operation says so; what it did not confirm may have been applied, and the operation
// says THAT, with the key to look for — because a failed operation is never run again, and an operator who asks again under a new key
// would adjust twice.
func TestAnAdjustmentTheHotTierRefusedFailsPlainlyAndOneItDidNotConfirmSaysItMayHaveBeenApplied(t *testing.T) {
	t.Parallel()
	refused := newFakeGranter()
	refused.err = domain.ErrShardFrozen
	_, err := app.AdjustmentExecutor(refused)(t.Context(), adjustOp(opA, adjParams))
	if !errors.Is(err, domain.ErrShardFrozen) || strings.Contains(err.Error(), "may have been applied") {
		t.Fatalf("a refused adjustment came to %v: it failed, and nothing was applied", err)
	}

	unsure := newFakeGranter()
	unsure.err = domain.ErrHotStoreUnavailable
	_, err = app.AdjustmentExecutor(unsure)(t.Context(), adjustOp(opA, adjParams))
	if !errors.Is(err, domain.ErrHotStoreUnavailable) || !strings.Contains(err.Error(), "may have been applied") || !strings.Contains(err.Error(), "admin-adjustment:"+opA) {
		t.Fatalf("an unconfirmed adjustment came to %v: it must say it may have been applied, and which key to look for", err)
	}
}

func TestAnAdjustmentWhoseArgumentsCannotBeReadFailsTheOperationAndGrantsNothing(t *testing.T) {
	t.Parallel()
	for _, p := range []string{``, `nope`, `{}`, `{"scope":` + adjScope + `}`, `{"scope":` + adjScope + `,"amount":0}`} {
		g := newFakeGranter()
		if _, err := app.AdjustmentExecutor(g)(t.Context(), adjustOp(opA, p)); err == nil {
			t.Errorf("arguments %q were taken", p)
		}
		if n := len(g.grants()); n != 0 {
			t.Errorf("arguments %q made %d grants", p, n)
		}
	}
}
