package webhookhttp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/truongpx396/intel-payment/billing/adapters/driving/webhookhttp"
	"github.com/truongpx396/intel-payment/billing/ports"
)

// R095: the door a payment provider knocks on. It reads the RAW body — size-limited, never parsed, never changed by a
// middleware — and hands it, the headers and the names the route carries to the ingress, which decides; the door only
// speaks HTTP for it. Everything that matters to money (verify first, persist before acknowledging) is the ingress's and
// proven by its contract; what is owed here is that the door does not undo it: it never alters the bytes a signature
// covers, never forwards a body it could not read whole, never answers 2xx for what the ingress refused, and never writes
// a delivery into a log.

const limit = 1 << 10

// ingress records what it was given and answers what the test says.
type ingress struct {
	mu    sync.Mutex
	calls []call
	reply func(call) ports.IngressResult
}

type call struct {
	ctx               context.Context
	provider, account string
	body              []byte
	header            http.Header
}

func (g *ingress) Handle(ctx context.Context, provider, account string, body []byte, header http.Header) ports.IngressResult {
	c := call{ctx: ctx, provider: provider, account: account, body: append([]byte(nil), body...), header: header.Clone()}
	g.mu.Lock()
	g.calls = append(g.calls, c)
	g.mu.Unlock()
	if g.reply != nil {
		return g.reply(c)
	}
	return ports.IngressResult{Status: http.StatusOK}
}

func (*ingress) Retry(context.Context) error { return nil }

func (g *ingress) seen() []call {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]call(nil), g.calls...)
}

// logs is a logger whose output the test can read.
type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func door(t *testing.T, in ports.WebhookIngress, out io.Writer) http.HandlerFunc {
	t.Helper()
	h, err := webhookhttp.New(webhookhttp.Config{Ingress: in, MaxBody: limit, Logger: slog.New(slog.NewTextHandler(out, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// deliver posts a body to a route and returns the recorded answer.
func deliver(h http.HandlerFunc, provider, account string, body io.Reader, hdr http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/"+provider+"/"+account, body)
	req.SetPathValue("provider", provider)
	req.SetPathValue("account", account)
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var b struct {
		Error struct{ Code string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("the answer %q is not the error envelope: %v", rec.Body.String(), err)
	}
	return b.Error.Code
}

func TestTheIngressIsGivenTheRawBodyTheHeadersAndTheNamesOfTheRoute(t *testing.T) {
	t.Parallel()
	in := &ingress{}
	h := door(t, in, io.Discard)
	// Bytes a re-encoder would change: odd whitespace, a duplicate key, an escape, a byte that is not UTF-8, a BOM.
	body := []byte("\xef\xbb\xbf{ \"id\" : \"evt_1\",\n\t\"id\":\"evt_2\", \"s\":\"\\u00e9\" , \"raw\":\"\xff\xfe\" }  \r\n")
	hdr := http.Header{"Stripe-Signature": {"t=1,v1=abc", "t=2,v1=def"}, "Content-Type": {"application/json; charset=utf-8"}}

	rec := deliver(h, "stripe", "acct-main", bytes.NewReader(body), hdr)

	got := in.seen()
	if len(got) != 1 {
		t.Fatalf("the ingress heard of the delivery %d times, want once", len(got))
	}
	c := got[0]
	if c.provider != "stripe" || c.account != "acct-main" {
		t.Fatalf("the ingress was told %q/%q, want the route's stripe/acct-main", c.provider, c.account)
	}
	if !bytes.Equal(c.body, body) {
		t.Fatalf("the body a signature covers was changed on its way to the ingress:\n got %q\nwant %q", c.body, body)
	}
	if got, want := c.header.Values("Stripe-Signature"), hdr["Stripe-Signature"]; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the signature header reached the ingress as %q, want every value of %q", got, want)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want the ingress's 200", rec.Code)
	}
}

func TestEveryAnswerOfTheIngressIsSpokenAsItsStatusAndCode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		res  ports.IngressResult
		want int
		code string
	}{
		{ports.IngressResult{Status: 200}, 200, ""},
		{ports.IngressResult{Status: 202}, 202, ""},
		{ports.IngressResult{Status: 400, Code: "invalid_signature"}, 400, "invalid_signature"},
		{ports.IngressResult{Status: 404, Code: "unknown_account"}, 404, "unknown_account"},
		{ports.IngressResult{Status: 413, Code: "payload_too_large"}, 413, "payload_too_large"},
		{ports.IngressResult{Status: 500, Code: "internal"}, 500, "internal"},
		{ports.IngressResult{Status: 503, Code: "persist_failed"}, 503, "persist_failed"},
		// An answer that is not an HTTP status is a defect in the ingress, and a provider must never read it as success.
		{ports.IngressResult{}, 500, "internal"},
		{ports.IngressResult{Status: 99}, 500, "internal"},
		{ports.IngressResult{Status: 302, Code: "moved"}, 500, "internal"},
		{ports.IngressResult{Status: 600, Code: "x"}, 500, "internal"},
	} {
		t.Run(fmt.Sprintf("%d_%s", tc.res.Status, tc.res.Code), func(t *testing.T) {
			t.Parallel()
			in := &ingress{reply: func(call) ports.IngressResult { return tc.res }}
			rec := deliver(door(t, in, io.Discard), "stripe", "acct", strings.NewReader(`{}`), nil)
			if rec.Code != tc.want {
				t.Fatalf("the ingress answered %+v and the door %d, want %d", tc.res, rec.Code, tc.want)
			}
			if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
				t.Fatalf("Content-Type %q, want JSON", got)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control %q: an answer to a delivery is not to be cached", got)
			}
			if tc.want < 300 {
				if strings.Contains(rec.Body.String(), "error") {
					t.Fatalf("a success carries an error body: %s", rec.Body.String())
				}
				return
			}
			if got := errorCode(t, rec); got != tc.code {
				t.Fatalf("error code %q, want %q", got, tc.code)
			}
		})
	}
}

func TestABodyOverTheLimitIsRefusedWithoutBeingReadToTheEnd(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		size    int
		want    int
		reached bool
	}{
		{"empty", 0, 200, true},
		{"exactly the limit", limit, 200, true},
		{"one byte over", limit + 1, 413, false},
		{"far over", 64 * limit, 413, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := &ingress{}
			// An endless source: a door that reads it all never answers.
			src := &counting{size: tc.size}
			rec := deliver(door(t, in, io.Discard), "stripe", "acct", src, nil)
			if rec.Code != tc.want || (len(in.seen()) == 1) != tc.reached {
				t.Fatalf("%d bytes: status %d and %d calls, want %d and reached=%v", tc.size, rec.Code, len(in.seen()), tc.want, tc.reached)
			}
			if tc.want == 413 {
				if got := errorCode(t, rec); got != "payload_too_large" {
					t.Fatalf("error code %q, want payload_too_large", got)
				}
			}
			if src.read > limit+1 {
				t.Fatalf("the door read %d bytes of a body whose limit is %d: an attacker chooses what the door buffers", src.read, limit)
			}
		})
	}
}

// counting yields size bytes of 'x' and counts what was taken.
type counting struct{ size, read int }

func (c *counting) Read(p []byte) (int, error) {
	if c.read >= c.size {
		return 0, io.EOF
	}
	n := min(len(p), c.size-c.read)
	for i := range n {
		p[i] = 'x'
	}
	c.read += n
	return n, nil
}

func TestABodyThatCouldNotBeReadWholeIsNeverForwarded(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err  error
		want int
		code string
	}{
		"the client went away":      {io.ErrUnexpectedEOF, 400, "invalid_request"},
		"a reset connection":        {errors.New("read: connection reset by peer"), 400, "invalid_request"},
		"the server's own body cap": {&http.MaxBytesError{Limit: 5}, 413, "payload_too_large"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			in := &ingress{}
			body := io.MultiReader(strings.NewReader(`{"half":`), &failing{err: tc.err})
			rec := deliver(door(t, in, io.Discard), "stripe", "acct", body, nil)
			if len(in.seen()) != 0 {
				t.Fatalf("a body cut short was given to the ingress: %q", in.seen()[0].body)
			}
			if rec.Code != tc.want || errorCode(t, rec) != tc.code {
				t.Fatalf("status %d %q, want %d %q", rec.Code, errorCode(t, rec), tc.want, tc.code)
			}
		})
	}
}

type failing struct{ err error }

func (f *failing) Read([]byte) (int, error) { return 0, f.err }

func TestARouteThatNamesNothingReachesNothing(t *testing.T) {
	t.Parallel()
	for name, names := range map[string][2]string{"no provider": {"", "acct"}, "no account": {"stripe", ""}, "neither": {"", ""}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			in := &ingress{}
			rec := deliver(door(t, in, io.Discard), names[0], names[1], strings.NewReader(`{}`), nil)
			if len(in.seen()) != 0 || rec.Code != http.StatusNotFound || errorCode(t, rec) != "unknown_account" {
				t.Fatalf("status %d, %d calls: a delivery that names no account is unknown_account and verifies nothing", rec.Code, len(in.seen()))
			}
		})
	}
}

func TestNothingOfADeliveryIsWrittenToALog(t *testing.T) {
	t.Parallel()
	const bodyMark, headerMark, tokenMark = "PAN-4242-4242-4242-4242", "v1=deadbeefsignature", "whsec_live_secret_token"
	for name, res := range map[string]ports.IngressResult{
		"accepted":   {Status: 200},
		"forged":     {Status: 400, Code: "invalid_signature"},
		"unknown":    {Status: 404, Code: "unknown_account"},
		"refused":    {Status: 503, Code: "persist_failed"},
		"defective":  {Status: 500, Code: "internal"},
		"not a code": {Status: 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out := &logs{}
			in := &ingress{reply: func(call) ports.IngressResult { return res }}
			deliver(door(t, in, out), "stripe", "acct-main", strings.NewReader(`{"card":"`+bodyMark+`"}`),
				http.Header{"Stripe-Signature": {headerMark}, "Authorization": {"Bearer " + tokenMark}})
			for _, secret := range []string{bodyMark, headerMark, tokenMark} {
				if strings.Contains(out.String(), secret) {
					t.Fatalf("the log holds %q from the delivery:\n%s", secret, out)
				}
			}
			if res.Status >= 500 || res.Status == 0 {
				for _, want := range []string{"stripe", "acct-main"} {
					if !strings.Contains(out.String(), want) {
						t.Fatalf("a failure the operator must act on does not name %q:\n%s", want, out)
					}
				}
			}
		})
	}
}

func TestTheRequestsContextReachesTheIngress(t *testing.T) {
	t.Parallel()
	type key struct{}
	in := &ingress{}
	h := door(t, in, io.Discard)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe/acct", strings.NewReader(`{}`))
	req.SetPathValue("provider", "stripe")
	req.SetPathValue("account", "acct")
	ctx, cancel := context.WithCancel(context.WithValue(req.Context(), key{}, "carried"))
	cancel()
	h.ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx))
	got := in.seen()
	if len(got) != 1 || got[0].ctx.Value(key{}) != "carried" || got[0].ctx.Err() == nil {
		t.Fatal("the ingress was not given the request's context: a provider that hangs up must stop the verification it started")
	}
}

func TestConcurrentDeliveriesEachKeepTheirOwnBodyAndNames(t *testing.T) {
	t.Parallel()
	in := &ingress{reply: func(c call) ports.IngressResult {
		// The answer depends on the delivery, so a door that mixes two up answers the wrong one.
		if bytes.Contains(c.body, []byte("bad")) {
			return ports.IngressResult{Status: 400, Code: "invalid_signature"}
		}
		return ports.IngressResult{Status: 200}
	}}
	h := door(t, in, io.Discard)
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			acct, body, want := fmt.Sprintf("acct-%d", i), fmt.Sprintf(`{"n":%d}`, i), 200
			if i%3 == 0 {
				body, want = fmt.Sprintf(`{"n":%d,"bad":true}`, i), 400
			}
			rec := deliver(h, "stripe", acct, strings.NewReader(body), nil)
			if rec.Code != want {
				t.Errorf("delivery %d: status %d, want %d", i, rec.Code, want)
			}
		}()
	}
	wg.Wait()
	seen := map[string]string{}
	for _, c := range in.seen() {
		seen[c.account] = string(c.body)
	}
	for i := range 64 {
		if got := seen[fmt.Sprintf("acct-%d", i)]; !strings.Contains(got, fmt.Sprintf(`"n":%d`, i)) {
			t.Fatalf("account acct-%d was given the body %q", i, got)
		}
	}
}

func TestADoorWithoutWhatItNeedsIsRefusedAtStart(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]webhookhttp.Config{
		"no ingress":         {MaxBody: limit},
		"no limit":           {Ingress: &ingress{}},
		"a negative limit":   {Ingress: &ingress{}, MaxBody: -1},
		"neither of the two": {},
	} {
		if _, err := webhookhttp.New(cfg); err == nil {
			t.Errorf("%s: the door was built, and would answer 2xx or panic at the first delivery", name)
		}
	}
	if webhookhttp.Path != "/webhooks/{provider}/{account}" {
		t.Errorf("Path is %q: the contract names /webhooks/{provider}/{account}", webhookhttp.Path)
	}
}
