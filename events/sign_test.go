package events_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/events"
)

// R115 (outbound-events.md § Contract-test obligations): "A delivery carries a signature that verifies with the endpoint's secret and
// fails with any other."

var signedAt = time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)

func TestASignatureVerifiesWithTheEndpointsSecretAndFailsWithAnyOther(t *testing.T) {
	t.Parallel()
	body := []byte(`{"id":"0192f3c4-7b1e-7a2c-9d4e-5f6a7b8c9d0e","type":"credits.low"}`)
	secret := []byte("whsec_endpoint_one")
	h := events.Sign([][]byte{secret}, signedAt, body)

	if err := events.Verify(h, body, secret, signedAt, 5*time.Minute); err != nil {
		t.Fatalf("a delivery must verify with the endpoint's own secret: %v", err)
	}
	for _, other := range []string{"whsec_endpoint_two", "whsec_endpoint_one ", "WHSEC_ENDPOINT_ONE", "", "w"} {
		if err := events.Verify(h, body, []byte(other), signedAt, 5*time.Minute); !errors.Is(err, events.ErrBadSignature) {
			t.Errorf("verified with the wrong secret %q: %v", other, err)
		}
	}
}

func TestTheSignatureIsOverTheTimestampAndTheRawBody(t *testing.T) {
	t.Parallel()
	body := []byte(`{"a":1}`)
	secret := []byte("s")
	h := events.Sign([][]byte{secret}, signedAt, body)
	// The documented shape: t=<unix seconds>,v1=<hex HMAC-SHA256(secret, "<t>.<raw body>")>.
	if !strings.HasPrefix(h, "t=1793872800,v1=") || strings.Count(h, ",") != 1 || len(h) != len("t=1793872800,v1=")+64 {
		t.Fatalf("header %q", h)
	}
	for name, tampered := range map[string][]byte{"one byte changed": []byte(`{"a":2}`), "whitespace added": []byte(`{"a": 1}`), "truncated": []byte(`{"a":1`), "empty": nil} {
		if err := events.Verify(h, tampered, secret, signedAt, time.Minute); !errors.Is(err, events.ErrBadSignature) {
			t.Errorf("%s verified: %v", name, err)
		}
	}
	// Moving the timestamp in the header is not enough: it is part of what is signed.
	forged := strings.Replace(h, "t=1793872800", "t=1793872801", 1)
	if err := events.Verify(forged, body, secret, signedAt, time.Minute); !errors.Is(err, events.ErrBadSignature) {
		t.Errorf("a re-dated signature verified: %v", err)
	}
}

func TestADeliveryOutsideTheToleranceIsRefusedInBothDirections(t *testing.T) {
	t.Parallel()
	body, secret := []byte("x"), []byte("s")
	h := events.Sign([][]byte{secret}, signedAt, body)
	for _, tc := range []struct {
		skew time.Duration
		ok   bool
	}{{0, true}, {5 * time.Minute, true}, {-5 * time.Minute, true}, {5*time.Minute + time.Second, false}, {-5*time.Minute - time.Second, false}, {24 * time.Hour, false}} {
		err := events.Verify(h, body, secret, signedAt.Add(tc.skew), 5*time.Minute)
		if tc.ok != (err == nil) || (!tc.ok && !errors.Is(err, events.ErrStale)) {
			t.Errorf("skew %v: %v", tc.skew, err)
		}
	}
}

// During a secret rotation the header carries one v1 per active secret, so a host that has rolled only one side still verifies.
func TestARotationCarriesOneSignaturePerSecretAndEitherVerifies(t *testing.T) {
	t.Parallel()
	body := []byte(`{"id":"e1"}`)
	oldS, newS := []byte("old-secret"), []byte("new-secret")
	h := events.Sign([][]byte{newS, oldS}, signedAt, body)
	if strings.Count(h, "v1=") != 2 {
		t.Fatalf("a rotation signs with both secrets: %q", h)
	}
	for _, s := range [][]byte{oldS, newS} {
		if err := events.Verify(h, body, s, signedAt, time.Minute); err != nil {
			t.Errorf("secret %q does not verify a rotation header: %v", s, err)
		}
	}
	if err := events.Verify(h, body, []byte("third"), signedAt, time.Minute); !errors.Is(err, events.ErrBadSignature) {
		t.Errorf("a secret that is neither verified: %v", err)
	}
}

func TestAMalformedSignatureHeaderIsRefusedNotMisread(t *testing.T) {
	t.Parallel()
	for _, h := range []string{"", "v1=abcd", "t=1793872800", "t=notanumber,v1=ab", "garbage", "t=1793872800,v2=ab", "t=,v1=", "t=1793872800,v1"} {
		if err := events.Verify(h, []byte("x"), []byte("s"), signedAt, time.Hour); err == nil {
			t.Errorf("%q verified", h)
		}
	}
}

// Any change to any byte of the body, and any change to the secret, is a failure — whatever the body.
func FuzzASignatureBindsTheBodyAndTheSecret(f *testing.F) {
	f.Add([]byte(`{"id":"x"}`), []byte("secret"), uint(0))
	f.Add([]byte{}, []byte("s"), uint(3))
	f.Add([]byte{0, 0xff, '\n'}, []byte{0}, uint(1))
	f.Fuzz(func(t *testing.T, body, secret []byte, flip uint) {
		h := events.Sign([][]byte{secret}, signedAt, body)
		if err := events.Verify(h, body, secret, signedAt, time.Minute); err != nil {
			t.Fatalf("its own signature does not verify: %v", err)
		}
		if len(body) > 0 {
			changed := append([]byte(nil), body...)
			changed[int(flip%uint(len(changed)))] ^= 1
			if err := events.Verify(h, changed, secret, signedAt, time.Minute); err == nil {
				t.Fatal("a body with one flipped bit verified")
			}
		}
		if err := events.Verify(h, body, append(append([]byte(nil), secret...), 'x'), signedAt, time.Minute); err == nil {
			t.Fatal("another secret verified")
		}
	})
}
