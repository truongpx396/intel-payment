package events

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Header names of a delivery (outbound-events.md § Webhook delivery).
const (
	HeaderEventID   = "Intel-Payment-Event-Id"
	HeaderSignature = "Intel-Payment-Signature"
)

// Sign returns the signature header for body at t: `t=<unix seconds>,v1=<hex HMAC-SHA256(secret, "<t>.<raw body>")>`, with one v1 per
// secret. A secret in rotation is a second v1, so a host that has only rolled one side still verifies.
func Sign(secrets [][]byte, t time.Time, body []byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	parts := []string{"t=" + ts}
	for _, s := range secrets {
		parts = append(parts, "v1="+mac(s, ts, body))
	}
	return strings.Join(parts, ",")
}

func mac(secret []byte, ts string, body []byte) string {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(ts))
	h.Write([]byte("."))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// Verification failures. The reason is for the host's log; none of them says which secret was close.
var (
	ErrBadSignature = errors.New("events: signature does not match")
	ErrStale        = errors.New("events: signature timestamp is outside the tolerance")
	ErrMalformed    = errors.New("events: malformed signature header")
)

// Verify is what a host does with a delivery, and what this service's tests do to prove the signature is one a host can check: it
// recomputes the MAC over the raw body, compares in constant time, and refuses a timestamp more than tolerance from now. It
// accepts the header if ANY of its v1 values matches the secret, as a host rolling a secret must.
func Verify(header string, body, secret []byte, now time.Time, tolerance time.Duration) error {
	var ts string
	var sigs []string
	for _, p := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		switch {
		case !ok:
			return ErrMalformed
		case k == "t":
			ts = v
		case k == "v1":
			sigs = append(sigs, v)
		}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || len(sigs) == 0 {
		return ErrMalformed
	}
	if d := now.Sub(time.Unix(sec, 0)); d > tolerance || d < -tolerance {
		return fmt.Errorf("%w: %s", ErrStale, d.Round(time.Second))
	}
	want := mac(secret, ts, body)
	ok := false
	for _, s := range sigs {
		if hmac.Equal([]byte(s), []byte(want)) {
			ok = true // keep going: the time taken does not say which position matched
		}
	}
	if !ok {
		return ErrBadSignature
	}
	return nil
}
