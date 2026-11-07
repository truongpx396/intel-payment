package resthandler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// R112: the operator's door. Every call names a person and a reason, carries the key that makes a retry the same request, needs the admin
// privilege, and is answered 202: it is recorded and done by the worker that holds the sole durable writer, and the answer says where to
// look for how it went.

// stubOps is a programmable ports.AdminOperations.
type stubOps struct {
	mu              sync.Mutex
	got             []ports.AdminRequest
	err             error
	created         bool
	op              ports.AdminOperation
	getRealm, getID string
}

var _ ports.AdminOperations = (*stubOps)(nil)

func (s *stubOps) Submit(_ context.Context, rq ports.AdminRequest) (ports.AdminOperation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, rq)
	if s.err != nil {
		return ports.AdminOperation{}, false, s.err
	}
	op := s.op
	if op.ID == "" {
		op = ports.AdminOperation{ID: "5b0e6f2a-1c3d-4e5f-8a9b-0c1d2e3f4a5b", Status: ports.AdminPending, CreatedAt: time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)}
	}
	op.Realm, op.Kind, op.Params, op.Actor, op.Reason, op.Key = rq.Realm, rq.Kind, rq.Params, rq.Actor, rq.Reason, rq.Key
	return op, s.created, nil
}

func (s *stubOps) Get(_ context.Context, realm, id string) (ports.AdminOperation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getRealm, s.getID = realm, id
	if s.err != nil {
		return ports.AdminOperation{}, s.err
	}
	return s.op, nil
}

func (s *stubOps) requests() []ports.AdminRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ports.AdminRequest(nil), s.got...)
}

const opWho = `"actor":"alice@example.com","reason":"INC-4412: the failover took the shard"`

type opRoute struct {
	name, path, body string
	kind             ports.AdminOperationKind
	params           string
}

var opRoutes = []opRoute{
	{"reconcile", "/v1/admin/reconcile?realm=acme", `{"shard":3,"kind":"full",` + opWho + `}`, ports.AdminReconcile, `{"shard":3,"kind":"full"}`},
	{"reconcile with the default kind", "/v1/admin/reconcile?realm=acme", `{"shard":0,` + opWho + `}`, ports.AdminReconcile, `{"shard":0}`},
	{"recover", "/v1/admin/shards/5/recover?realm=acme", `{` + opWho + `}`, ports.AdminRecover, `{"shard":5}`},
	{"rehydrate", "/v1/admin/rehydrate?realm=acme", `{"scope":{"kind":"workspace","id":"w1"},` + opWho + `}`, ports.AdminRehydrate, `{"scope":{"kind":"workspace","id":"w1"}}`},
	{"replay", "/v1/admin/dead-letters/5b0e6f2a-1c3d-4e5f-8a9b-0c1d2e3f4a5b/replay?realm=acme", `{` + opWho + `}`, ports.AdminReplay, `{"id":"5b0e6f2a-1c3d-4e5f-8a9b-0c1d2e3f4a5b"}`},
	{"subdrift", "/v1/admin/subdrift?realm=acme", `{"account":"stripe-live",` + opWho + `}`, ports.AdminSubdrift, `{"account":"stripe-live"}`},
}

func TestEveryOperationIsAcceptedRecordedAndPointedTo(t *testing.T) {
	t.Parallel()
	for _, rt := range opRoutes {
		t.Run(rt.name, func(t *testing.T) {
			t.Parallel()
			ops := &stubOps{created: true}
			a := newAPI(t, &stubMeter{}, apiOpts{ops: ops})
			rec := a.call("admin", "POST", rt.path, rt.body, "Idempotency-Key", "ticket-4412-a")
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			got := ops.requests()
			if len(got) != 1 {
				t.Fatalf("%d requests were recorded", len(got))
			}
			rq := got[0]
			if rq.Realm != "acme" || rq.Kind != rt.kind || rq.Actor != "alice@example.com" || rq.Reason != "INC-4412: the failover took the shard" ||
				rq.Key != "ticket-4412-a" || rq.Via != "ops-tool" || !jsonEqual(rq.Params, rt.params) {
				t.Fatalf("recorded %+v, want kind %s params %s", rq, rt.kind, rt.params)
			}
			var body struct {
				Created   bool `json:"created"`
				Operation struct {
					ID     string          `json:"id"`
					Kind   string          `json:"kind"`
					Status string          `json:"status"`
					Actor  string          `json:"actor"`
					Reason string          `json:"reason"`
					Params json.RawMessage `json:"params"`
				} `json:"operation"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if !body.Created || body.Operation.ID == "" || body.Operation.Status != "pending" || body.Operation.Kind != string(rt.kind) || body.Operation.Actor != "alice@example.com" || !jsonEqual(body.Operation.Params, rt.params) {
				t.Fatalf("answered %s", rec.Body)
			}
			if loc := rec.Header().Get("Location"); loc != "/v1/admin/operations/"+body.Operation.ID {
				t.Fatalf("Location = %q: the answer must say where to look", loc)
			}
		})
	}
}

func jsonEqual(raw json.RawMessage, want string) bool {
	var a, b any
	if json.Unmarshal(raw, &a) != nil || json.Unmarshal([]byte(want), &b) != nil {
		return false
	}
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestARetryIsTheSameOperationAndSaysSo(t *testing.T) {
	t.Parallel()
	ops := &stubOps{created: false}
	a := newAPI(t, &stubMeter{}, apiOpts{ops: ops})
	rec := a.call("admin", "POST", opRoutes[0].path, opRoutes[0].body, "Idempotency-Key", "ticket-4412-a")
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"created":false`) {
		t.Fatalf("%d: %s", rec.Code, rec.Body)
	}
}

func TestAKeyReusedForADifferentRequestIsAConflictAndNeverASilentNoOp(t *testing.T) {
	t.Parallel()
	ops := &stubOps{err: fmt.Errorf("%w: key %q was used for a different request", domain.ErrIdemConflict, "ticket-4412-a")}
	a := newAPI(t, &stubMeter{}, apiOpts{ops: ops})
	rec := a.call("admin", "POST", opRoutes[0].path, opRoutes[0].body, "Idempotency-Key", "ticket-4412-a")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "idempotency_conflict") {
		t.Fatalf("%d: %s", rec.Code, rec.Body)
	}
}

func TestEveryOperationNeedsAKeyAThePersonAndAReasonBeforeAnythingIsRecorded(t *testing.T) {
	t.Parallel()
	for _, rt := range opRoutes {
		t.Run(rt.name, func(t *testing.T) {
			t.Parallel()
			ops := &stubOps{created: true}
			a := newAPI(t, &stubMeter{}, apiOpts{ops: ops})
			noActor := strings.Replace(rt.body, `"actor":"alice@example.com"`, `"actor":" "`, 1)
			noReason := strings.Replace(rt.body, `"reason":"INC-4412: the failover took the shard"`, `"reason":""`, 1)
			cases := []struct {
				what, body string
				hdr        []string
				code       int
			}{
				{"no Idempotency-Key", rt.body, nil, 400},
				{"a blank Idempotency-Key", rt.body, []string{"Idempotency-Key", "  "}, 400},
				{"no person", noActor, []string{"Idempotency-Key", "k"}, 400},
				{"no reason", noReason, []string{"Idempotency-Key", "k"}, 400},
				{"a body that is not JSON", `nope`, []string{"Idempotency-Key", "k"}, 400},
				{"a field nobody knows", strings.Replace(rt.body, `{`, `{"shrad":1,`, 1), []string{"Idempotency-Key", "k"}, 400},
			}
			for _, c := range cases {
				rec := a.call("admin", "POST", rt.path, c.body, c.hdr...)
				if rec.Code != c.code || !strings.Contains(rec.Body.String(), "invalid_request") {
					t.Errorf("%s: %d %s, want %d invalid_request", c.what, rec.Code, rec.Body, c.code)
				}
			}
			if n := len(ops.requests()); n != 0 {
				t.Fatalf("%d requests were recorded from calls that were refused", n)
			}
		})
	}
}

func TestOperationsAreRefusedToAnyCredentialWithoutTheAdminPrivilegeAndToNoCredential(t *testing.T) {
	t.Parallel()
	for _, rt := range opRoutes {
		t.Run(rt.name, func(t *testing.T) {
			t.Parallel()
			ops := &stubOps{created: true}
			a := newAPI(t, &stubMeter{}, apiOpts{ops: ops})
			for _, token := range []string{"all", "record", "grant"} { // a credential that can spend and mint cannot operate
				if rec := a.call(token, "POST", rt.path, rt.body, "Idempotency-Key", "k"); rec.Code != http.StatusForbidden {
					t.Errorf("the %q credential: %d %s, want 403", token, rec.Code, rec.Body)
				}
			}
			if rec := a.call("", "POST", rt.path, rt.body, "Idempotency-Key", "k"); rec.Code != http.StatusUnauthorized {
				t.Errorf("no credential: %d, want 401", rec.Code)
			}
			if n := len(ops.requests()); n != 0 {
				t.Fatalf("%d requests were recorded for a caller who may not ask", n)
			}
		})
	}
}

func TestACredentialThatSpansRealmsMustSayWhichAndOneThatDoesNotCoverTheRealmIsRefused(t *testing.T) {
	t.Parallel()
	ops := &stubOps{created: true}
	a := newAPI(t, &stubMeter{}, apiOpts{ops: ops})
	path := "/v1/admin/reconcile"
	if rec := a.call("admin", "POST", path, opRoutes[0].body, "Idempotency-Key", "k"); rec.Code != http.StatusBadRequest {
		t.Fatalf("a credential for two realms and no realm named: %d %s", rec.Code, rec.Body)
	}
	if rec := a.call("admin", "POST", path+"?realm=initech", opRoutes[0].body, "Idempotency-Key", "k"); rec.Code != http.StatusForbidden && rec.Code != http.StatusBadRequest {
		t.Fatalf("a realm the credential does not cover: %d %s", rec.Code, rec.Body)
	}
	if rec := a.call("admin", "POST", path+"?realm=globex", opRoutes[0].body, "Idempotency-Key", "k"); rec.Code != http.StatusAccepted {
		t.Fatalf("a realm it covers: %d %s", rec.Code, rec.Body)
	}
	if got := ops.requests(); len(got) != 1 || got[0].Realm != "globex" {
		t.Fatalf("recorded %+v, want one request in globex", got)
	}
}

func TestAShardThatIsNotANumberIsRefusedAtTheDoor(t *testing.T) {
	t.Parallel()
	ops := &stubOps{created: true}
	a := newAPI(t, &stubMeter{}, apiOpts{ops: ops})
	for _, shard := range []string{"x", "-1", "1.5", "99999999999999999999", "%20"} {
		rec := a.call("admin", "POST", "/v1/admin/shards/"+shard+"/recover?realm=acme", `{`+opWho+`}`, "Idempotency-Key", "k")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("shard %q: %d %s, want 400", shard, rec.Code, rec.Body)
		}
	}
	if n := len(ops.requests()); n != 0 {
		t.Fatalf("%d requests were recorded for a shard that is not a number", n)
	}
}

func TestAnOperationIsReadBackWithHowItWent(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 11, 6, 9, 0, 5, 0, time.UTC)
	done := started.Add(2 * time.Second)
	ops := &stubOps{op: ports.AdminOperation{
		ID: "5b0e6f2a-1c3d-4e5f-8a9b-0c1d2e3f4a5b", Realm: "acme", Kind: ports.AdminReconcile, Params: json.RawMessage(`{"shard":3,"kind":"full"}`),
		Actor: "alice@example.com", Reason: "INC-4412", Key: "ticket-4412-a", Via: "ops-tool", Status: ports.AdminSucceeded,
		Result: json.RawMessage(`{"outcome":"ran"}`), Attempts: 1, CreatedAt: started.Add(-time.Second), StartedAt: &started, FinishedAt: &done,
	}}
	a := newAPI(t, &stubMeter{}, apiOpts{ops: ops})
	rec := a.call("admin", "GET", "/v1/admin/operations/5b0e6f2a-1c3d-4e5f-8a9b-0c1d2e3f4a5b?realm=acme", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d: %s", rec.Code, rec.Body)
	}
	var op struct {
		ID, Kind, Status, Actor, Reason, Error string
		IdempotencyKey                         string          `json:"idempotency_key"`
		Result                                 json.RawMessage `json:"result"`
		Attempts                               int
		CreatedAt                              string `json:"created_at"`
		StartedAt                              string `json:"started_at"`
		FinishedAt                             string `json:"finished_at"`
		Via                                    string `json:"via"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &op); err != nil {
		t.Fatal(err)
	}
	if op.Status != "succeeded" || !jsonEqual(op.Result, `{"outcome":"ran"}`) || op.Attempts != 1 || op.IdempotencyKey != "ticket-4412-a" ||
		op.FinishedAt != "2026-11-06T09:00:07Z" || op.StartedAt != "2026-11-06T09:00:05Z" || op.CreatedAt != "2026-11-06T09:00:04Z" || op.Error != "" {
		t.Fatalf("answered %s", rec.Body)
	}
	if op.Via != "" {
		t.Fatalf("the credential that carried the request was shown to the person reading it: %q", op.Via)
	}
	if ops.getRealm != "acme" || ops.getID != "5b0e6f2a-1c3d-4e5f-8a9b-0c1d2e3f4a5b" {
		t.Fatalf("read realm %q id %q", ops.getRealm, ops.getID)
	}
}

func TestAnOperationThatIsNotThereOrIsAnotherRealmsIs404AndAFailedOneShowsWhy(t *testing.T) {
	t.Parallel()
	ops := &stubOps{err: fmt.Errorf("%w: operation", domain.ErrNotFound)}
	a := newAPI(t, &stubMeter{}, apiOpts{ops: ops})
	if rec := a.call("admin", "GET", "/v1/admin/operations/5b0e6f2a-1c3d-4e5f-8a9b-0c1d2e3f4a5b?realm=acme", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("%d: %s", rec.Code, rec.Body)
	}
	ops.err = nil
	ops.op = ports.AdminOperation{ID: "x", Realm: "acme", Kind: ports.AdminRecover, Status: ports.AdminFailed, Error: "the shard is frozen by another recovery"}
	rec := a.call("admin", "GET", "/v1/admin/operations/x?realm=acme", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"error":"the shard is frozen by another recovery"`) || strings.Contains(rec.Body.String(), `"result"`) {
		t.Fatalf("%d: %s", rec.Code, rec.Body)
	}
}

func TestReadingAnOperationNeedsTheAdminPrivilegeToo(t *testing.T) {
	t.Parallel()
	a := newAPI(t, &stubMeter{}, apiOpts{ops: &stubOps{}})
	for _, token := range []string{"all", "record", "grant"} {
		if rec := a.call(token, "GET", "/v1/admin/operations/x?realm=acme", ""); rec.Code != http.StatusForbidden {
			t.Errorf("the %q credential: %d, want 403", token, rec.Code)
		}
	}
}

// A deployment that has not wired the operations serves none of their routes: a route that answers 202 and does nothing is worse than a 404.
func TestWithoutTheOperationsNoOperationRouteExists(t *testing.T) {
	t.Parallel()
	a := newAPI(t, &stubMeter{}, apiOpts{})
	for _, rt := range opRoutes {
		rec := a.call("admin", "POST", rt.path, rt.body, "Idempotency-Key", "k")
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: %d, want it not to exist", rt.name, rec.Code)
		}
	}
}
