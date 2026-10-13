package grpcserver_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/truongpx396/intel-payment/api/meteringv1"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/grpcserver"
)

// The gRPC half of FR-040: the interceptor lifts a Credential off the call — the bearer in the
// authorization metadata, the verified client certificates of the handshake — and a handler is reached only
// with the Principal it yields.

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func tokens(t *testing.T) auth.Authenticator {
	t.Helper()
	a, err := auth.ParseTokens("host-a:" + auth.HashToken("tok-a") + ":acme:record,minter:" + auth.HashToken("tok-m") + ":acme:grant,both:" + auth.HashToken("tok-b") + ":acme:record+grant")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// authRig is a real server whose only authentication is the interceptor under test.
func authRig(t *testing.T, stub *stubMeter, a auth.Authenticator) meteringv1.MeteringClient {
	t.Helper()
	srv, err := grpcserver.New(stub, stub, grpcserver.WithLogger(quiet))
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer(grpc.UnaryInterceptor(grpcserver.AuthInterceptor(a, quiet)))
	srv.Register(g)
	lis := bufconn.Listen(1 << 20)
	go func() { _ = g.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(); g.Stop(); _ = lis.Close() })
	return meteringv1.NewMeteringClient(conn)
}

func bearer(token string) context.Context {
	return metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
}

func TestABearerInTheMetadataAuthenticatesTheCall(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		ctx  context.Context
		call rpc
		code codes.Code
		why  string
	}{
		"no credential":                  {context.Background(), rpcAdmit, codes.Unauthenticated, "unauthenticated"},
		"a wrong token":                  {bearer("nope"), rpcAdmit, codes.Unauthenticated, "unauthenticated"},
		"another scheme":                 {metadata.AppendToOutgoingContext(context.Background(), "authorization", "Basic dG9rLWE="), rpcAdmit, codes.Unauthenticated, "unauthenticated"},
		"the scheme is case-insensitive": {metadata.AppendToOutgoingContext(context.Background(), "authorization", "bearer tok-a"), rpcAdmit, codes.OK, ""},
		"record may admit":               {bearer("tok-a"), rpcAdmit, codes.OK, ""},
		"record may record":              {bearer("tok-a"), rpcRecord, codes.OK, ""},
		"record may not grant":           {bearer("tok-a"), rpcGrant, codes.PermissionDenied, "forbidden"},
		"record may not transfer":        {bearer("tok-a"), rpcTransfer, codes.PermissionDenied, "forbidden"},
		"grant may mint":                 {bearer("tok-m"), rpcGrant, codes.OK, ""},
		"grant may not spend":            {bearer("tok-m"), rpcRecord, codes.PermissionDenied, "forbidden"},
		"grant may not read a balance":   {bearer("tok-m"), rpcBalance, codes.PermissionDenied, "forbidden"},
		"both may do both":               {bearer("tok-b"), rpcTransfer, codes.OK, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stub := &stubMeter{}
			client := authRig(t, stub, tokens(t))
			err := tc.call.call(tc.ctx, client)
			st := status.Convert(err)
			if st.Code() != tc.code || reasonOf(st) != tc.why {
				t.Fatalf("got %v / %q (%v), want %v / %q", st.Code(), reasonOf(st), err, tc.code, tc.why)
			}
			if tc.code != codes.OK && stub.calls.Load() != 0 {
				t.Fatal("a refused call reached the engine")
			}
		})
	}
}

func TestACallIsHandedThePrincipalTheCredentialYields(t *testing.T) {
	t.Parallel()
	stub := &stubMeter{}
	client := authRig(t, stub, tokens(t))
	req := okAdmit()
	req.Scope.Realm = "globex" // the token's realm is acme
	_, err := client.Admit(bearer("tok-a"), req)
	if st := status.Convert(err); reasonOf(st) != "realm_mismatch" {
		t.Fatalf("realm binding follows from the credential, not the request: %v / %q", st.Code(), reasonOf(st))
	}
	req.Scope.Realm = ""
	if _, err := client.Admit(bearer("tok-a"), req); err != nil || stub.scope.Realm != "acme" {
		t.Fatalf("an omitted realm is the credential's: %v, realm %q", err, stub.scope.Realm)
	}
}

func TestAClientCertificateAuthenticatesTheCall(t *testing.T) {
	t.Parallel()
	certs, err := auth.ParseCertSubjects("host-cert:host.internal:acme:record")
	if err != nil {
		t.Fatal(err)
	}
	ic := grpcserver.AuthInterceptor(certs, quiet)
	info := &grpc.UnaryServerInfo{FullMethod: "/metering.v1.Metering/Admit"}
	withCert := func(cn string) context.Context {
		c := &x509.Certificate{Subject: pkix.Name{CommonName: cn}}
		return peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{c}}}})
	}
	var who string
	next := func(ctx context.Context, _ any) (any, error) {
		p, ok := auth.FromContext(ctx)
		if !ok {
			return nil, errors.New("no principal reached the handler")
		}
		who = p.CallerID
		return nil, nil //nolint:nilnil // the handler's result is not under test
	}
	if _, err := ic(withCert("host.internal"), nil, info, next); err != nil || who != "host-cert" {
		t.Fatalf("%v, caller %q", err, who)
	}
	for name, ctx := range map[string]context.Context{
		"an unknown subject": withCert("evil.example"),
		"no TLS at all":      context.Background(),
		"a peer with no TLS": peer.NewContext(context.Background(), &peer.Peer{}),
	} {
		who = ""
		_, err := ic(ctx, nil, info, next)
		if status.Code(err) != codes.Unauthenticated || who != "" {
			t.Fatalf("%s: %v (handler reached: %v)", name, err, who != "")
		}
	}
}

// A credential that cannot be CHECKED (a store is down) is the service failing, never a stranger's access.
func TestAnAuthenticatorThatCannotCheckIsInternalNotOpen(t *testing.T) {
	t.Parallel()
	down := auth.AuthenticatorFunc(func(context.Context, auth.Credential) (auth.Principal, error) {
		return auth.Principal{}, errors.New("credential store: connection refused")
	})
	stub := &stubMeter{}
	err := rpcAdmit.call(bearer("tok-a"), authRig(t, stub, down))
	if status.Code(err) != codes.Internal || stub.calls.Load() != 0 {
		t.Fatalf("%v (engine calls %d)", err, stub.calls.Load())
	}
}
