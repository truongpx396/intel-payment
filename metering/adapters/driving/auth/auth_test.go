package auth_test

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"strings"
	"testing"

	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// docs/security.md: a credential carries a caller id, a realm set and a privilege set. Bearer tokens are
// stored as SHA-256 digests; a client certificate's subject maps to the same triple.

func TestPrivilegesParseAndPrintAsTheConfigurationSpellsThem(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]auth.Privilege{
		"record": auth.Record, "grant": auth.Grant, "admin": auth.Admin,
		"record+grant": auth.Record | auth.Grant, "grant+record": auth.Record | auth.Grant, "record+grant+admin": auth.Record | auth.Grant | auth.Admin,
	} {
		got, err := auth.ParsePrivileges(in)
		if err != nil || got != want {
			t.Fatalf("ParsePrivileges(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	// A misspelt privilege must not become a narrower — or a different — credential.
	for _, bad := range []string{"", "read", "Record", "record+", "+record", "record grant", "record,grant", "root"} {
		if _, err := auth.ParsePrivileges(bad); err == nil {
			t.Fatalf("ParsePrivileges(%q) was accepted", bad)
		}
	}
	if auth.Privilege(0).Has(auth.Record) || auth.Record.Has(0) {
		t.Fatal("the empty privilege is held by no one, and a check that needs nothing is a check nobody wrote")
	}
	if got := (auth.Record | auth.Admin).String(); got != "record+admin" {
		t.Fatalf("String() = %q", got)
	}
}

func tokenSpec(entries ...string) string { return strings.Join(entries, ",") }

func TestBearerTokensAuthenticateToTheirTriple(t *testing.T) {
	t.Parallel()
	a, err := auth.ParseTokens(tokenSpec(
		"host-a:"+auth.HashToken("tok-a")+":acme:record",
		"host-b:"+strings.ToUpper(auth.HashToken("tok-b"))+":acme+globex:record+grant",
		" ops:"+auth.HashToken("tok-ops")+":acme:admin ",
	))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for token, want := range map[string]auth.Principal{
		"tok-a":   {CallerID: "host-a", Realms: []domain.Realm{"acme"}, Privileges: auth.Record},
		"tok-b":   {CallerID: "host-b", Realms: []domain.Realm{"acme", "globex"}, Privileges: auth.Record | auth.Grant},
		"tok-ops": {CallerID: "ops", Realms: []domain.Realm{"acme"}, Privileges: auth.Admin},
	} {
		got, err := a.Authenticate(ctx, auth.Credential{Bearer: token})
		if err != nil || got.CallerID != want.CallerID || got.Privileges != want.Privileges || got.AnyRealm || len(got.Realms) != len(want.Realms) {
			t.Fatalf("%s: %+v, %v; want %+v", token, got, err, want)
		}
		for i, r := range want.Realms {
			if got.Realms[i] != r {
				t.Fatalf("%s: realms %v, want %v", token, got.Realms, want.Realms)
			}
		}
	}
	for _, token := range []string{"", "tok", "tok-a ", " tok-a", "TOK-A", "tok-a\n", auth.HashToken("tok-a")} {
		if _, err := a.Authenticate(ctx, auth.Credential{Bearer: token}); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Fatalf("%q must not authenticate: %v (the stored digest is not a token)", token, err)
		}
	}
	if _, err := a.Authenticate(ctx, auth.Credential{}); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("no credential: %v", err)
	}
}

func TestNoTokensConfiguredRefusesEveryone(t *testing.T) {
	t.Parallel()
	a, err := auth.ParseTokens("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(context.Background(), auth.Credential{Bearer: "anything"}); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("an empty set is not an open door: %v", err)
	}
}

// A token spec is read at start-up, and a mistake in it must stop the service, not widen or narrow a
// credential quietly.
func TestAMistakeInTheTokenSpecIsRefused(t *testing.T) {
	t.Parallel()
	d := auth.HashToken("tok")
	bad := map[string]string{
		"the token itself where its digest belongs": "host-a:tok:acme:record",
		"a short digest":             "host-a:abcd:acme:record",
		"a digest that is not hex":   "host-a:" + strings.Repeat("z", 64) + ":acme:record",
		"too few fields":             "host-a:" + d + ":acme",
		"too many fields":            "host-a:" + d + ":acme:record:extra",
		"no caller id":               ":" + d + ":acme:record",
		"no realm":                   "host-a:" + d + "::record",
		"a realm that is not a slug": "host-a:" + d + ":Acme Corp:record",
		"a wildcard realm":           "host-a:" + d + ":*:record",
		"no privilege":               "host-a:" + d + ":acme:",
		"an unknown privilege":       "host-a:" + d + ":acme:root",
		"a caller listed twice":      "host-a:" + d + ":acme:record,host-a:" + auth.HashToken("other") + ":acme:record",
		"one token for two callers":  "host-a:" + d + ":acme:record,host-b:" + d + ":acme:grant",
	}
	for name, spec := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := auth.ParseTokens(spec); err == nil {
				t.Fatalf("%q was accepted", spec)
			}
		})
	}
}

func cert(cn string) *x509.Certificate { return &x509.Certificate{Subject: pkix.Name{CommonName: cn}} }

func TestACertificateSubjectMapsToItsTriple(t *testing.T) {
	t.Parallel()
	a, err := auth.ParseCertSubjects("host-a:host-a.internal:acme:record,host-b:host-b.internal:acme+globex:record+grant")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	got, err := a.Authenticate(ctx, auth.Credential{PeerCertificates: []*x509.Certificate{cert("host-b.internal"), cert("host-a.internal")}})
	if err != nil || got.CallerID != "host-b" || got.Privileges != auth.Record|auth.Grant || len(got.Realms) != 2 {
		t.Fatalf("the leaf (first) certificate decides: %+v, %v", got, err)
	}
	for name, c := range map[string]auth.Credential{
		"no certificate":        {},
		"an unknown subject":    {PeerCertificates: []*x509.Certificate{cert("evil.example")}},
		"only a bearer":         {Bearer: "host-a.internal"},
		"a subject with a tail": {PeerCertificates: []*x509.Certificate{cert("host-a.internal.evil.example")}},
	} {
		if _, err := a.Authenticate(ctx, c); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for name, spec := range map[string]string{
		"too few fields": "host-a:host-a.internal:acme", "a subject twice": "a:s:acme:record,b:s:acme:grant", "a caller twice": "a:s1:acme:record,a:s2:acme:grant",
		"an unknown privilege": "a:s:acme:root", "no subject": "a::acme:record",
	} {
		if _, err := auth.ParseCertSubjects(spec); err == nil {
			t.Fatalf("%s: %q was accepted", name, spec)
		}
	}
}

// PAYMENT_SERVICE_AUTH=none is permitted only where the network is the credential (loopback, a Unix
// socket); the caller it yields can do everything, and the service refuses to start with it on a routable address.
func TestNoneIsEveryoneAndSaysSo(t *testing.T) {
	t.Parallel()
	p, err := auth.None().Authenticate(context.Background(), auth.Credential{})
	if err != nil || !p.AnyRealm || !p.Privileges.Has(auth.Record) || !p.Privileges.Has(auth.Grant) || !p.Privileges.Has(auth.Admin) {
		t.Fatalf("%+v %v", p, err)
	}
	if !p.Covers("anything") {
		t.Fatal("AnyRealm covers every realm")
	}
	if s, err := p.Resolve(domain.Scope{Kind: "workspace", ID: "w"}); err != nil || s.Realm != domain.DefaultRealm {
		t.Fatalf("an omitted realm is the default one: %+v %v", s, err)
	}
}
