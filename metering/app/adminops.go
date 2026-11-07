package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// The limits on what an operator writes into a record that is kept for ever. A key is a handle, an actor a person, a reason a sentence or
// a ticket; none of them is a place to put a document.
const (
	MaxAdminActor  = 200
	MaxAdminReason = 2000
	MaxAdminKey    = 200
	MaxAdminError  = 2000
)

// DefaultAdminLease is how long a worker's claim on an operator request holds before another worker may take it over: a worker that is killed
// midway leaves its operation to be done again, and every operation is safe to do again.
const DefaultAdminLease = 5 * time.Minute

// AdminKind says how a request of one kind reads its arguments. The engine defines the kinds that act on the engine; the composition root
// defines the ones that act on something the engine does not know of (a payment provider's account), because metering imports no billing.
type AdminKind struct {
	Kind ports.AdminOperationKind
	// Parse turns the caller's arguments into the one form that is stored, or refuses them with domain.ErrInvalid. It is given the request's
	// realm, for the kinds whose arguments name something that lives in one.
	Parse func(realm string, params json.RawMessage) (json.RawMessage, error)
}

// The arguments the engine's own kinds take, in the form they are stored in and the worker reads them from.
type (
	// ReconcileParams: which shard, and whether the sweep is full or incremental (the default).
	ReconcileParams struct {
		Shard int    `json:"shard"`
		Kind  string `json:"kind"`
	}
	// RecoverParams: which shard.
	RecoverParams struct {
		Shard int `json:"shard"`
	}
	// ScopeParams names a scope of the request's realm.
	ScopeParams struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	}
	// RehydrateParams: the scope whose hot account is rebuilt from the books.
	RehydrateParams struct {
		Scope ScopeParams `json:"scope"`
	}
	// ReplayParams: the dead letter to publish again.
	ReplayParams struct {
		ID string `json:"id"`
	}
)

// AdminOperationsDeps are what taking a request needs.
type AdminOperationsDeps struct {
	Store   ports.AdminOperationStore
	Metrics ports.Metrics // default: no-op
	// Shards is the deployment's shard count: a request that names one beyond it is refused where it is read, not after it has waited.
	Shards int
	// Kinds are the kinds the composition root adds to the engine's own.
	Kinds []AdminKind
}

// AdminOperations takes what an operator asks of the engine. It does not do it: paymentd has no writer, and the operations that matter act on
// the writer. It checks the request, stores it with the person and the reason, and returns; the worker that holds the writer does the rest
// (AdminWorker), and the record says how it went.
type AdminOperations struct {
	d     AdminOperationsDeps
	kinds map[ports.AdminOperationKind]AdminKind
}

var _ ports.AdminOperations = (*AdminOperations)(nil)

// NewAdminOperations validates the dependencies.
func NewAdminOperations(d AdminOperationsDeps) (*AdminOperations, error) {
	var errs []error
	if d.Store == nil {
		errs = append(errs, errors.New("an AdminOperationStore is required"))
	}
	if d.Shards <= 0 {
		errs = append(errs, errors.New("the deployment's shard count is required: without it a shard cannot be told from a typo"))
	}
	kinds := map[ports.AdminOperationKind]AdminKind{
		ports.AdminReconcile: {Kind: ports.AdminReconcile, Parse: func(_ string, p json.RawMessage) (json.RawMessage, error) { return parseReconcile(p, d.Shards) }},
		ports.AdminRecover:   {Kind: ports.AdminRecover, Parse: func(_ string, p json.RawMessage) (json.RawMessage, error) { return parseRecover(p, d.Shards) }},
		ports.AdminRehydrate: {Kind: ports.AdminRehydrate, Parse: parseRehydrate},
		ports.AdminReplay:    {Kind: ports.AdminReplay, Parse: func(_ string, p json.RawMessage) (json.RawMessage, error) { return parseReplay(p) }},
	}
	for _, k := range d.Kinds {
		if k.Kind == "" || k.Parse == nil {
			errs = append(errs, fmt.Errorf("the kind %q needs a name and a parser", k.Kind))
			continue
		}
		if _, dup := kinds[k.Kind]; dup {
			errs = append(errs, fmt.Errorf("the kind %q is already defined", k.Kind))
		}
		kinds[k.Kind] = k
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("metering admin operations: %w", err)
	}
	if d.Metrics == nil {
		d.Metrics = nopMetrics{}
	}
	return &AdminOperations{d: d, kinds: kinds}, nil
}

// Submit checks the request and records it. The person is trimmed of the spaces around them, and the reason is kept as written: it is
// what someone will read when they ask why a shard was recovered.
func (a *AdminOperations) Submit(ctx context.Context, rq ports.AdminRequest) (ports.AdminOperation, bool, error) {
	actor, reason, key := strings.TrimSpace(rq.Actor), strings.TrimSpace(rq.Reason), strings.TrimSpace(rq.Key)
	if actor == "" {
		return ports.AdminOperation{}, false, fmt.Errorf("%w: actor is required: an operation is the act of a person", domain.ErrInvalid)
	}
	if len(actor) > MaxAdminActor {
		return ports.AdminOperation{}, false, fmt.Errorf("%w: actor is at most %d bytes", domain.ErrInvalid, MaxAdminActor)
	}
	if reason == "" {
		return ports.AdminOperation{}, false, fmt.Errorf("%w: reason is required: an operation is kept with why it was done", domain.ErrInvalid)
	}
	if len(reason) > MaxAdminReason {
		return ports.AdminOperation{}, false, fmt.Errorf("%w: reason is at most %d bytes", domain.ErrInvalid, MaxAdminReason)
	}
	if key == "" {
		return ports.AdminOperation{}, false, fmt.Errorf("%w: an idempotency key is required: it is this operation's identity, and a retry carries the same one", domain.ErrInvalid)
	}
	if len(key) > MaxAdminKey || strings.ContainsFunc(key, unicode.IsControl) {
		return ports.AdminOperation{}, false, fmt.Errorf("%w: an idempotency key is at most %d bytes and has no control characters", domain.ErrInvalid, MaxAdminKey)
	}
	if rq.Realm == "" {
		return ports.AdminOperation{}, false, fmt.Errorf("%w: realm is required", domain.ErrInvalid)
	}
	if err := domain.Realm(rq.Realm).Validate(); err != nil {
		return ports.AdminOperation{}, false, err
	}
	k, ok := a.kinds[rq.Kind]
	if !ok {
		return ports.AdminOperation{}, false, fmt.Errorf("%w: %q is not an operation", domain.ErrInvalid, rq.Kind)
	}
	params, err := k.Parse(rq.Realm, rq.Params)
	if err != nil {
		return ports.AdminOperation{}, false, err
	}
	return a.d.Store.Submit(ctx, ports.AdminOperation{
		Realm: rq.Realm, Kind: rq.Kind, Params: params, Actor: actor, Reason: reason, Key: key, Via: rq.Via,
	})
}

// Get reads an operation of the realm.
func (a *AdminOperations) Get(ctx context.Context, realm, id string) (ports.AdminOperation, error) {
	return a.d.Store.Get(ctx, realm, id)
}

// decodeStrict reads exactly one JSON object into v. A field it does not know is a mistake the operator should hear about now ("shrad" for
// "shard" would otherwise be a request for shard zero), and so is a second value after the first.
func decodeStrict(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: the arguments are not what this operation takes: %w", domain.ErrInvalid, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: the arguments are one JSON object", domain.ErrInvalid)
	}
	return nil
}

func checkShard(shard *int, shards int) (int, error) {
	if shard == nil {
		return 0, fmt.Errorf("%w: shard is required: {\"shard\": N}", domain.ErrInvalid)
	}
	if *shard < 0 || *shard >= shards {
		return 0, fmt.Errorf("%w: shard %d is not one of this deployment's %d", domain.ErrInvalid, *shard, shards)
	}
	return *shard, nil
}

func parseReconcile(raw json.RawMessage, shards int) (json.RawMessage, error) {
	var in struct {
		Shard *int   `json:"shard"`
		Kind  string `json:"kind"`
	}
	if err := decodeStrict(raw, &in); err != nil {
		return nil, err
	}
	shard, err := checkShard(in.Shard, shards)
	if err != nil {
		return nil, err
	}
	kind := domain.ReconcileKind(in.Kind)
	switch kind {
	case "":
		kind = domain.ReconcileIncremental
	case domain.ReconcileIncremental, domain.ReconcileFull:
	default:
		return nil, fmt.Errorf("%w: a reconcile is %q or %q, not %q", domain.ErrInvalid, domain.ReconcileIncremental, domain.ReconcileFull, in.Kind)
	}
	return json.Marshal(ReconcileParams{Shard: shard, Kind: string(kind)})
}

func parseRecover(raw json.RawMessage, shards int) (json.RawMessage, error) {
	var in struct {
		Shard *int `json:"shard"`
	}
	if err := decodeStrict(raw, &in); err != nil {
		return nil, err
	}
	shard, err := checkShard(in.Shard, shards)
	if err != nil {
		return nil, err
	}
	return json.Marshal(RecoverParams{Shard: shard})
}

func parseRehydrate(realm string, raw json.RawMessage) (json.RawMessage, error) {
	var in struct {
		Scope *ScopeParams `json:"scope"`
	}
	if err := decodeStrict(raw, &in); err != nil {
		return nil, err
	}
	if in.Scope == nil {
		return nil, fmt.Errorf("%w: scope is required: {\"scope\": {\"kind\": ..., \"id\": ...}}", domain.ErrInvalid)
	}
	if err := (domain.Scope{Realm: domain.Realm(realm), Kind: in.Scope.Kind, ID: in.Scope.ID}).Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(RehydrateParams{Scope: *in.Scope})
}

// A dead letter's id is a lower-case UUID: anything else is not an id this deployment issued.
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func parseReplay(raw json.RawMessage) (json.RawMessage, error) {
	var in ReplayParams
	if err := decodeStrict(raw, &in); err != nil {
		return nil, err
	}
	if !uuidPattern.MatchString(in.ID) {
		return nil, fmt.Errorf("%w: id is a dead letter's id, a lower-case UUID", domain.ErrInvalid)
	}
	return json.Marshal(in)
}

// AdminExecutor does one kind of operation and says what it came to as JSON. It is run again if its worker dies, so it must be safe to run
// again: every operation the engine defines is, and an executor that is not is a bug in the executor.
type AdminExecutor func(ctx context.Context, op ports.AdminOperation) (json.RawMessage, error)

// AdminWorkerDeps are what working the operator's requests needs.
type AdminWorkerDeps struct {
	Store     ports.AdminOperationStore
	Executors map[ports.AdminOperationKind]AdminExecutor
	Metrics   ports.Metrics // default: no-op
	Log       *slog.Logger  // default: slog.Default()
	// Lease is how long a claim holds; default DefaultAdminLease. It must outlast the longest operation, or a second worker starts it again
	// while the first is still at it.
	Lease time.Duration
}

// AdminWorker works the queue of operator requests: it claims one, runs the executor of its kind, and records what came of it.
type AdminWorker struct{ d AdminWorkerDeps }

// NewAdminWorker validates the dependencies.
func NewAdminWorker(d AdminWorkerDeps) (*AdminWorker, error) {
	var errs []error
	if d.Store == nil {
		errs = append(errs, errors.New("an AdminOperationStore is required"))
	}
	if d.Lease < 0 {
		errs = append(errs, errors.New("the lease cannot be negative"))
	}
	for kind, ex := range d.Executors {
		if ex == nil {
			errs = append(errs, fmt.Errorf("the executor for %q is nil", kind))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("metering admin worker: %w", err)
	}
	if d.Lease == 0 {
		d.Lease = DefaultAdminLease
	}
	if d.Metrics == nil {
		d.Metrics = nopMetrics{}
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	return &AdminWorker{d: d}, nil
}

// RunOne claims the oldest request that is waiting and works it. It reports whether there was one. What an operation came to is its own
// record, not an error of the worker's: a failure is written down as failed and the worker goes on. A worker that is told to stop while it
// is at work leaves the operation to the next one — unless the work was done, in which case that is recorded, because it is true.
func (w *AdminWorker) RunOne(ctx context.Context) (bool, error) {
	op, ok, err := w.d.Store.Claim(ctx, w.d.Lease)
	if err != nil {
		return false, fmt.Errorf("claim an operator request: %w", err)
	}
	if !ok {
		return false, nil
	}
	result, runErr := w.run(ctx, op)
	if runErr != nil && ctx.Err() != nil {
		w.d.Log.Warn("an operator request was interrupted and will be done by the next worker", "operation", op.ID, "kind", string(op.Kind), "attempt", op.Attempts)
		return true, ctx.Err()
	}
	status, errText, outcome := ports.AdminSucceeded, "", "succeeded"
	if runErr != nil {
		status, errText, outcome = ports.AdminFailed, truncate(runErr.Error(), MaxAdminError), "failed"
		result = nil
	} else if len(result) == 0 {
		result = json.RawMessage(`{}`)
	}
	// The outcome is written even if the worker was told to stop a moment ago: the work is done, and not saying so would have the next worker do it again.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	recorded, err := w.d.Store.Finish(fctx, op.ID, op.Attempts, status, result, errText)
	if err != nil {
		return true, fmt.Errorf("record the outcome of operation %s: %w", op.ID, err)
	}
	if !recorded {
		w.d.Log.Warn("an operator request's outcome was not recorded: another worker holds it now", "operation", op.ID, "kind", string(op.Kind), "attempt", op.Attempts)
		return true, nil
	}
	w.d.Metrics.Count(ports.MetricAdminOperations, 1, ports.Labels{"kind": string(op.Kind), "outcome": outcome})
	w.d.Log.Info("an operator request was worked", "operation", op.ID, "kind", string(op.Kind), "outcome", outcome, "actor", op.Actor, "realm", op.Realm)
	return true, nil
}

// run is the executor of the operation's kind. A kind this worker has no executor for fails at once and says which, so an operation does not
// wait for ever for a worker that cannot do it; a panic fails the operation and not the worker.
func (w *AdminWorker) run(ctx context.Context, op ports.AdminOperation) (result json.RawMessage, err error) {
	ex, ok := w.d.Executors[op.Kind]
	if !ok {
		return nil, fmt.Errorf("this worker has no executor for %s operations", op.Kind)
	}
	defer func() {
		if r := recover(); r != nil {
			result, err = nil, fmt.Errorf("panic: %v", r)
		}
	}()
	return ex(ctx, op)
}

// ExportGauges publishes what waits and for how long, read from the store: the numbers a page for a worker that has stopped is written
// against. An empty queue is a zero, not a gap.
func (w *AdminWorker) ExportGauges(ctx context.Context) error {
	b, err := w.d.Store.Backlog(ctx)
	if err != nil {
		return err
	}
	w.d.Metrics.Gauge(ports.MetricAdminWaiting, b.Waiting, nil)
	w.d.Metrics.Gauge(ports.MetricAdminOldestWait, int64(b.Oldest/time.Second), nil)
	return nil
}

// truncate cuts s to at most n bytes without cutting a character in two.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
