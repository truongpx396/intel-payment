package grpcclient_test

import (
	"context"
	"testing"

	"github.com/truongpx396/intel-payment/metering/adapters/driving/grpcclient"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// The credential rides on the connection (per-RPC credentials), so business code that holds a ports.Meter
// never handles a token. A bearer token is a password: it is not sent in the clear unless the caller says
// the transport is one it trusts (loopback, a sidecar's Unix socket).

func admit(cl *grpcclient.Client) error {
	_, err := cl.Admit(context.Background(), ws, domain.AdmitRequest{Resource: "llm.chat", MaxCost: 1})
	return err
}

func TestTheBearerTokenRidesOnEveryCall(t *testing.T) {
	t.Parallel()
	f := &fakeServer{}
	cl := dial(t, f, grpcclient.WithBearer("tok-a", grpcclient.OverInsecureTransport()))
	if err := admit(cl); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Record(context.Background(), event()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := f.md.Get("authorization"); len(got) != 1 || got[0] != "Bearer tok-a" {
		t.Fatalf("authorization = %v", got)
	}
}

func TestATokenIsNotSentInTheClearByDefault(t *testing.T) {
	t.Parallel()
	f := &fakeServer{}
	// The test connection is not TLS. gRPC refuses the combination when the connection is made; had it let
	// it through, the call would fail before a byte left. Either way the token never reaches the server.
	cl, err := tryDial(t, f, grpcclient.WithBearer("tok-a"))
	if err == nil {
		err = admit(cl)
	}
	if err == nil {
		t.Fatal("a token went out over a transport the caller never said it trusts")
	}
	if f.calls.Load() != 0 {
		t.Fatal("the call reached the server")
	}
}

func TestAnEmptyTokenFailsTheCallInsteadOfSendingNone(t *testing.T) {
	t.Parallel()
	f := &fakeServer{}
	cl := dial(t, f, grpcclient.WithBearer("", grpcclient.OverInsecureTransport()))
	if err := admit(cl); err == nil || f.calls.Load() != 0 {
		t.Fatalf("a client configured with no token must say so, not send an anonymous call: %v (calls %d)", err, f.calls.Load())
	}
}
