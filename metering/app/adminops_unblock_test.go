package app_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// R112a: an unblock names one scope of the request's own realm, and nothing else. What it clears lets an account spend again, so a request
// that cannot be read exactly is refused where it is read — "shrad" for "shard" is the mistake the strict reading exists to catch, and an
// unblock that read a scope loosely would clear the wrong one.

func TestAnUnblockNamesOneScopeAndIsStoredInOneCanonicalForm(t *testing.T) {
	t.Parallel()
	r := newAdminRig(t)
	for i, g := range []struct{ params, want string }{
		{`{"scope":{"kind":"workspace","id":"w1"}}`, `{"scope":{"kind":"workspace","id":"w1"}}`},
		{`{"scope":{"id":"w1","kind":"workspace"}}`, `{"scope":{"kind":"workspace","id":"w1"}}`},
		{` { "scope" : { "kind" : "workspace" , "id" : "w1" } } `, `{"scope":{"kind":"workspace","id":"w1"}}`},
	} {
		op := r.submit(t, req(ports.AdminUnblock, g.params, fmt.Sprintf("good-%d", i)))
		if op.Kind != ports.AdminUnblock || string(op.Params) != g.want {
			t.Errorf("%s was stored as %s %s, want %s", g.params, op.Kind, op.Params, g.want)
		}
	}
}

func TestAnUnblockThatCannotBeReadExactlyIsRefusedAndNeverQueued(t *testing.T) {
	t.Parallel()
	r := newAdminRig(t)
	bad := []string{
		``, `not json`, `[1]`, `null`, `{}`, `{"scope":null}`, `{"scope":{}}`,
		`{"scope":{"kind":"workspace"}}`, `{"scope":{"id":"w1"}}`, `{"scope":{"kind":"","id":"w1"}}`, `{"scope":{"kind":"workspace","id":""}}`,
		`{"scope":{"kind":"Work Space","id":"w1"}}`, `{"scope":{"kind":"workspace","id":"w1","extra":1}}`, `{"scope":"workspace:w1"}`,
		`{"scope":{"kind":"workspace","id":"w1"},"pool":"general"}`, `{"scop":{"kind":"workspace","id":"w1"}}`,
		`{"scope":{"kind":"workspace","id":"w1"}} {"scope":{"kind":"workspace","id":"w2"}}`,
		`{"scope":{"realm":"another","kind":"workspace","id":"w1"}}`, // a realm is the request's, never the arguments'
	}
	for i, p := range bad {
		if _, created, err := r.ops.Submit(t.Context(), req(ports.AdminUnblock, p, fmt.Sprintf("bad-%d", i))); !errors.Is(err, domain.ErrInvalid) || created {
			t.Errorf("unblock %q: created %v, err %v, want %v", p, created, err, domain.ErrInvalid)
		}
	}
	if b, _ := r.store.Backlog(t.Context()); b.Waiting != 0 {
		t.Fatalf("%d requests wait after every one was refused", b.Waiting)
	}
}

// A retry is the same operation, so the operator who did not hear the answer does not clear twice; the same key for a different scope is not
// a retry.
func TestAnUnblockUnderTheSameKeyForAnotherScopeIsAConflict(t *testing.T) {
	t.Parallel()
	r := newAdminRig(t)
	first := r.submit(t, req(ports.AdminUnblock, `{"scope":{"kind":"workspace","id":"w1"}}`, "k-1"))
	again, created, err := r.ops.Submit(t.Context(), req(ports.AdminUnblock, `{"scope":{"id":"w1","kind":"workspace"}}`, "k-1"))
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("a retry made %+v (created %v, err %v) after %s", again, created, err, first.ID)
	}
	if _, _, err := r.ops.Submit(t.Context(), req(ports.AdminUnblock, `{"scope":{"kind":"workspace","id":"w2"}}`, "k-1")); !errors.Is(err, domain.ErrIdemConflict) {
		t.Fatalf("the key reused for another scope: %v, want %v", err, domain.ErrIdemConflict)
	}
}

// An unblock names a scope of the request's own realm: the scope's realm is the operation's, and an operator of one realm cannot clear
// another's.
func TestAnUnblockIsInTheRealmOfTheRequestAndNoOther(t *testing.T) {
	t.Parallel()
	r := newAdminRig(t)
	q := req(ports.AdminUnblock, `{"scope":{"kind":"workspace","id":"w1"}}`, "k-1")
	q.Realm = "tenant-b"
	op := r.submit(t, q)
	if op.Realm != "tenant-b" {
		t.Fatalf("the operation is in realm %q, want the request's", op.Realm)
	}
	if _, err := r.ops.Get(t.Context(), "default", op.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another realm read the operation: %v", err)
	}
}
