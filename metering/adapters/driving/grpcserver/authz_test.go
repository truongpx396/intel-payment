package grpcserver_test

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/truongpx396/intel-payment/api/meteringv1"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// "The realm is authenticated, never asserted" and "Grant and Transfer are privileged, separated at the
// RPC level, so a compromised spend producer can debit but never mint or move credits."

func principal(privs auth.Privilege, realms ...domain.Realm) *auth.Principal {
	return &auth.Principal{CallerID: "host-a", Realms: realms, Privileges: privs}
}

func TestNoCredentialIsUnauthenticatedOnEveryRPC(t *testing.T) {
	t.Parallel()
	for _, call := range allRPCs {
		t.Run(call.name, func(t *testing.T) {
			t.Parallel()
			stub := &stubMeter{}
			st := status.Convert(call.call(context.Background(), newRig(t, stub, nil).client))
			if st.Code() != codes.Unauthenticated || reasonOf(st) != "unauthenticated" {
				t.Fatalf("an absent principal must never default to allowed: %v / %q", st.Code(), reasonOf(st))
			}
			if stub.calls.Load() != 0 {
				t.Fatal("the engine was reached without a credential")
			}
		})
	}
}

func TestPrivilegesAreSeparatedAtTheRPC(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name    string
		p       *auth.Principal
		allowed []string
	}{
		{"record only", principal(auth.Record, "acme"), []string{"Admit", "Record", "GetBalance"}},
		{"grant only", principal(auth.Grant, "acme"), []string{"Grant", "Transfer"}},
		{"admin only", principal(auth.Admin, "acme"), nil},
		{"record and grant", principal(auth.Record|auth.Grant, "acme"), []string{"Admit", "Record", "GetBalance", "Grant", "Transfer"}},
	}
	for _, row := range rows {
		for _, call := range allRPCs {
			allowed := false
			for _, a := range row.allowed {
				allowed = allowed || a == call.name
			}
			t.Run(row.name+"/"+call.name, func(t *testing.T) {
				t.Parallel()
				stub := &stubMeter{}
				err := call.call(context.Background(), newRig(t, stub, row.p).client)
				if allowed {
					if err != nil || stub.calls.Load() != 1 {
						t.Fatalf("%s must be allowed: %v (engine calls %d)", call.name, err, stub.calls.Load())
					}
					return
				}
				st := status.Convert(err)
				if st.Code() != codes.PermissionDenied || reasonOf(st) != "forbidden" {
					t.Fatalf("%s must be refused at the RPC: %v / %q", call.name, st.Code(), reasonOf(st))
				}
				if stub.calls.Load() != 0 {
					t.Fatal("a refused privilege reached the engine")
				}
			})
		}
	}
}

func TestARealmOutsideTheCredentialIsRefusedNotTrusted(t *testing.T) {
	t.Parallel()
	foreign := func(s *meteringv1.Scope) { s.Realm = "globex" }
	calls := map[string]func(context.Context, *rig) error{
		"Admit": func(ctx context.Context, r *rig) error {
			req := okAdmit()
			foreign(req.Scope)
			_, err := r.client.Admit(ctx, req)
			return err
		},
		"Record": func(ctx context.Context, r *rig) error {
			req := okRecord()
			foreign(req.Event.Scope)
			_, err := r.client.Record(ctx, req)
			return err
		},
		"Grant": func(ctx context.Context, r *rig) error {
			req := okGrant()
			foreign(req.Scope)
			_, err := r.client.Grant(ctx, req)
			return err
		},
		"GetBalance": func(ctx context.Context, r *rig) error {
			req := okBalance()
			foreign(req.Scope)
			_, err := r.client.GetBalance(ctx, req)
			return err
		},
		"Transfer from": func(ctx context.Context, r *rig) error {
			req := okTransfer()
			foreign(req.From)
			_, err := r.client.Transfer(ctx, req)
			return err
		},
		"Transfer to": func(ctx context.Context, r *rig) error {
			req := okTransfer()
			foreign(req.To)
			_, err := r.client.Transfer(ctx, req)
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stub := &stubMeter{}
			st := status.Convert(call(context.Background(), newRig(t, stub, principal(auth.Record|auth.Grant, "acme"))))
			if st.Code() != codes.PermissionDenied || reasonOf(st) != "realm_mismatch" {
				t.Fatalf("a mismatch is PERMISSION_DENIED realm_mismatch: %v / %q", st.Code(), reasonOf(st))
			}
			if stub.calls.Load() != 0 {
				t.Fatal("a foreign realm reached the engine")
			}
		})
	}
}

func TestAPrivilegeIsCheckedBeforeARealmIsDisclosed(t *testing.T) {
	t.Parallel()
	req := okGrant()
	req.Scope.Realm = "globex"
	_, err := newRig(t, &stubMeter{}, principal(auth.Record, "acme")).client.Grant(context.Background(), req)
	if st := status.Convert(err); reasonOf(st) != "forbidden" {
		t.Fatalf("an under-privileged caller learns nothing about realms: %q", reasonOf(st))
	}
}

func TestAnOmittedRealmIsTheCredentialsOnlyRealm(t *testing.T) {
	t.Parallel()
	t.Run("one realm: it is resolved, and the engine sees it", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{}
		req := okAdmit()
		req.Scope.Realm = ""
		if _, err := newRig(t, stub, principal(auth.Record, "acme")).client.Admit(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if stub.scope.Realm != "acme" {
			t.Fatalf("the engine saw realm %q", stub.scope.Realm)
		}
	})
	t.Run("two realms: ambiguous, so refused rather than guessed", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{}
		req := okAdmit()
		req.Scope.Realm = ""
		st := status.Convert(func() error {
			_, err := newRig(t, stub, principal(auth.Record, "acme", "globex")).client.Admit(context.Background(), req)
			return err
		}())
		if st.Code() != codes.InvalidArgument || stub.calls.Load() != 0 {
			t.Fatalf("%v, engine calls %d", st.Code(), stub.calls.Load())
		}
	})
	t.Run("a realm in the credential's set is served", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{}
		req := okAdmit()
		req.Scope.Realm = "globex"
		if _, err := newRig(t, stub, principal(auth.Record, "acme", "globex")).client.Admit(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if stub.scope.Realm != "globex" {
			t.Fatalf("the engine saw realm %q", stub.scope.Realm)
		}
	})
}

// A missing idem_key or occurred_at is INVALID_ARGUMENT, never a generated default: a server-generated
// key makes every retry a new charge. The refusal is the server's own — it does not lean on the engine.
func TestAMissingKeyOrTimeIsInvalidArgumentAndNeverReachesTheEngine(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name  string
		call  func(context.Context, meteringv1.MeteringClient) error
		field string
	}{
		{"Record without idem_key", func(ctx context.Context, c meteringv1.MeteringClient) error {
			req := okRecord()
			req.Event.IdemKey = ""
			_, err := c.Record(ctx, req)
			return err
		}, "idem_key"},
		{"Record without occurred_at", func(ctx context.Context, c meteringv1.MeteringClient) error {
			req := okRecord()
			req.Event.OccurredAt = nil
			_, err := c.Record(ctx, req)
			return err
		}, "occurred_at"},
		{"Record with an out-of-range occurred_at", func(ctx context.Context, c meteringv1.MeteringClient) error {
			req := okRecord()
			req.Event.OccurredAt.Seconds = 1 << 40
			_, err := c.Record(ctx, req)
			return err
		}, "occurred_at"},
		{"Record without an event", func(ctx context.Context, c meteringv1.MeteringClient) error {
			_, err := c.Record(ctx, &meteringv1.RecordRequest{})
			return err
		}, "event"},
		{"Grant without idem_key", func(ctx context.Context, c meteringv1.MeteringClient) error {
			req := okGrant()
			req.IdemKey = ""
			_, err := c.Grant(ctx, req)
			return err
		}, "idem_key"},
		{"Transfer without idem_key", func(ctx context.Context, c meteringv1.MeteringClient) error {
			req := okTransfer()
			req.IdemKey = ""
			_, err := c.Transfer(ctx, req)
			return err
		}, "idem_key"},
		{"Grant with an out-of-range expires_at", func(ctx context.Context, c meteringv1.MeteringClient) error {
			req := okGrant()
			req.ExpiresAt = tspb(2027, 1, 1, 0, 0, 0)
			req.ExpiresAt.Seconds = 1 << 40
			_, err := c.Grant(ctx, req)
			return err
		}, "expires_at"},
		{"Admit without a scope", func(ctx context.Context, c meteringv1.MeteringClient) error {
			_, err := c.Admit(ctx, &meteringv1.AdmitRequest{Resource: "llm.chat"})
			return err
		}, "scope"},
		{"Transfer without a destination", func(ctx context.Context, c meteringv1.MeteringClient) error {
			req := okTransfer()
			req.To = nil
			_, err := c.Transfer(ctx, req)
			return err
		}, "to"},
		{"Admit with an unknown window", func(ctx context.Context, c meteringv1.MeteringClient) error {
			req := okAdmit()
			req.Limits = []*meteringv1.Limit{{Name: "x", Unit: "credit", Window: meteringv1.Window(99), DenyCode: "limit_reached"}}
			_, err := c.Admit(ctx, req)
			return err
		}, "window"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			stub := &stubMeter{}
			st := status.Convert(row.call(context.Background(), newRig(t, stub, &acme).client))
			if st.Code() != codes.InvalidArgument || reasonOf(st) != "invalid_request" {
				t.Fatalf("got %v / %q (%s)", st.Code(), reasonOf(st), st.Message())
			}
			if !strings.Contains(st.Message(), row.field) {
				t.Fatalf("the message should name %q: %q", row.field, st.Message())
			}
			if stub.calls.Load() != 0 {
				t.Fatal("the engine was reached with a request the contract says is invalid")
			}
		})
	}
}

func TestAuthenticationPrecedesValidation(t *testing.T) {
	t.Parallel()
	req := okRecord()
	req.Event.IdemKey = ""
	_, err := newRig(t, &stubMeter{}, nil).client.Record(context.Background(), req)
	if st := status.Convert(err); st.Code() != codes.Unauthenticated {
		t.Fatalf("a stranger is not told what is wrong with the request: %v", st.Code())
	}
}
