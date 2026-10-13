package resthandler_test

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// rest-api.md: "`Idempotency-Key` is required on every mutating request, and it IS the operation's
// `idem_key` (a body `idem_key` that differs is 400)." A server-generated key would make every retry a
// new charge, so a request without one is refused, and the engine is not reached.

var keyed = []route{recordRoute, grantRoute, transferRoute}

func TestARequestThatMovesCreditsNeedsAnIdempotencyKey(t *testing.T) {
	t.Parallel()
	for _, r := range keyed {
		for name, hdr := range map[string][]string{
			"absent":                     nil,
			"empty":                      {"Idempotency-Key", ""},
			"blank":                      {"Idempotency-Key", "   "},
			"present but the wrong name": {"Idempotency-Id", "k-1"},
		} {
			t.Run(r.name+"/"+name, func(t *testing.T) {
				t.Parallel()
				stub := &stubMeter{}
				rec := newAPI(t, stub, apiOpts{}).call("all", r.method, r.path, r.body, hdr...)
				if rec.Code != 400 {
					t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
				}
				e := decodeErr(t, rec)
				if e.Error.Code != "invalid_request" || !strings.Contains(e.Error.Message, "Idempotency-Key") {
					t.Fatalf("the message should name the header: %+v", e.Error)
				}
				if stub.calls.Load() != 0 {
					t.Fatal("the engine was reached without an idempotency key: a generated one makes every retry a new charge")
				}
			})
		}
	}
}

func TestTwoIdempotencyKeysAreAmbiguousAndRefused(t *testing.T) {
	t.Parallel()
	stub := &stubMeter{}
	a := newAPI(t, stub, apiOpts{})
	req := httptest.NewRequest("POST", recordRoute.path, strings.NewReader(recordRoute.body))
	req.Header.Set("Authorization", "Bearer all")
	req.Header.Add("Idempotency-Key", "k-1")
	req.Header.Add("Idempotency-Key", "k-2")
	rec := serve(a, req)
	if rec.Code != 400 || stub.calls.Load() != 0 {
		t.Fatalf("%d %s (engine calls %d)", rec.Code, rec.Body.String(), stub.calls.Load())
	}
}

func TestTheHeaderIsTheEnginesKeyExactly(t *testing.T) {
	t.Parallel()
	const key = "call-abc/42:retry-é"
	for _, r := range keyed {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			stub := &stubMeter{}
			if rec := newAPI(t, stub, apiOpts{}).call("all", r.method, r.path, r.body, "Idempotency-Key", key); rec.Code != 200 {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			got := map[string]string{"record": stub.event.IdemKey, "grant": stub.gotGrant.IdemKey, "transfer": stub.gotXfer.IdemKey}[r.name]
			if got != key {
				t.Fatalf("the engine's key is %q, the header said %q", got, key)
			}
		})
	}
}

func TestABodyKeyThatDiffersFromTheHeaderIs400(t *testing.T) {
	t.Parallel()
	withBody := func(r route, key string) string {
		return strings.Replace(r.body, `{`, `{"idem_key":"`+key+`",`, 1)
	}
	for _, r := range keyed {
		t.Run(r.name+"/differs", func(t *testing.T) {
			t.Parallel()
			stub := &stubMeter{}
			rec := newAPI(t, stub, apiOpts{}).call("all", r.method, r.path, withBody(r, "other"), "Idempotency-Key", "k-1")
			if rec.Code != 400 || !strings.Contains(decodeErr(t, rec).Error.Message, "idem_key") || stub.calls.Load() != 0 {
				t.Fatalf("%d %s (engine calls %d)", rec.Code, rec.Body.String(), stub.calls.Load())
			}
		})
		t.Run(r.name+"/agrees", func(t *testing.T) {
			t.Parallel()
			stub := &stubMeter{}
			if rec := newAPI(t, stub, apiOpts{}).call("all", r.method, r.path, withBody(r, "k-1"), "Idempotency-Key", "k-1"); rec.Code != 200 {
				t.Fatalf("a body key that matches the header is the same key: %d %s", rec.Code, rec.Body.String())
			}
		})
		t.Run(r.name+"/body only is not enough", func(t *testing.T) {
			t.Parallel()
			stub := &stubMeter{}
			rec := newAPI(t, stub, apiOpts{}).call("all", r.method, r.path, withBody(r, "k-1"))
			if rec.Code != 400 || stub.calls.Load() != 0 {
				t.Fatalf("the header is what is required: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// Admit reserves nothing and reads are reads: there is no operation behind them to be idempotent.
func TestAdmitAndReadsNeedNoKey(t *testing.T) {
	t.Parallel()
	for _, r := range []route{admitRoute, balanceRoute} {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			if rec := newAPI(t, &stubMeter{}, apiOpts{}).call("all", r.method, r.path, r.body); rec.Code != 200 {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
		})
	}
}
