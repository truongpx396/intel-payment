package httpsend_test

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/events"
	"github.com/truongpx396/intel-payment/events/adapters/driven/httpsend"
)

// R115: "an endpoint resolving to a private, loopback, link-local or IPv4-mapped address is refused before any request is sent, a
// redirect is not followed, and the body is capped." A test server listens on loopback, so these tests can see whether it was reached.

type hits struct{ n atomic.Int64 }

func server(t *testing.T, h *hits, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.n.Add(1)
		handler(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func TestADestinationOnLoopbackIsRefusedBeforeAnyRequestIsSent(t *testing.T) {
	t.Parallel()
	var h hits
	s := server(t, &h, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	for name, url := range map[string]string{
		"the literal address": s.URL, "a hostname that resolves to it": strings.Replace(s.URL, "127.0.0.1", "localhost", 1),
		"the IPv4-mapped form": strings.Replace(s.URL, "127.0.0.1", "[::ffff:127.0.0.1]", 1),
	} {
		status, err := httpsend.New(httpsend.Options{}).Send(context.Background(), url, nil, []byte("{}"))
		if status != 0 || err == nil {
			t.Errorf("%s: status %d, err %v", name, status, err)
		}
		if h.n.Load() != 0 {
			t.Fatalf("%s: the destination was reached %d times before it was refused", name, h.n.Load())
		}
	}
	// An http URL is refused outright, whatever it points at.
	if _, err := httpsend.New(httpsend.Options{}).Send(context.Background(), "http://example.com/hook", nil, nil); !errors.Is(err, events.ErrForbiddenAddress) {
		t.Errorf("a plain-http destination: %v", err)
	}
}

func TestAHostnameThatResolvesToALoopbackAddressIsRefusedWhenItIsConnectedTo(t *testing.T) {
	t.Parallel()
	var h hits
	s := server(t, &h, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	// https: the URL check passes (it is a hostname), and the refusal can only come from the check on the address being connected to.
	url := "https://localhost:" + s.Listener.Addr().(*net.TCPAddr).AddrPort().String()[len("127.0.0.1:"):] + "/hook"
	_, err := httpsend.New(httpsend.Options{}).Send(context.Background(), url, nil, []byte("{}"))
	if !errors.Is(err, events.ErrForbiddenAddress) {
		t.Fatalf("got %v: the resolved address must be judged, not only the name", err)
	}
	if h.n.Load() != 0 {
		t.Fatalf("reached %d times", h.n.Load())
	}
}

func TestWithDevelopmentLoopbackAllowedTheRequestIsSentWhole(t *testing.T) {
	t.Parallel()
	var h hits
	var gotBody, gotSig, gotID, gotType, gotMethod string
	s := server(t, &h, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotSig, gotID, gotType, gotMethod = string(b), r.Header.Get(events.HeaderSignature), r.Header.Get(events.HeaderEventID), r.Header.Get("Content-Type"), r.Method
		w.WriteHeader(204)
	})
	status, err := httpsend.New(httpsend.Options{Guard: events.Guard{AllowPrivate: true}}).Send(context.Background(), s.URL+"/hook",
		map[string]string{events.HeaderSignature: "t=1,v1=ab", events.HeaderEventID: "evt-1", "Content-Type": "application/json"}, []byte(`{"id":"evt-1"}`))
	if err != nil || status != 204 {
		t.Fatalf("%d, %v", status, err)
	}
	if gotBody != `{"id":"evt-1"}` || gotSig != "t=1,v1=ab" || gotID != "evt-1" || gotType != "application/json" || gotMethod != "POST" {
		t.Fatalf("the endpoint received %q %q %q %q %q", gotMethod, gotBody, gotSig, gotID, gotType)
	}
}

func TestAnHTTPSEndpointIsReachedWhenItsCertificateIsTrusted(t *testing.T) {
	t.Parallel()
	var h hits
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { h.n.Add(1); w.WriteHeader(200) }))
	t.Cleanup(s.Close)
	pool := x509.NewCertPool()
	pool.AddCert(s.Certificate())
	status, err := httpsend.New(httpsend.Options{Guard: events.Guard{AllowPrivate: true}, RootCAs: pool}).Send(context.Background(), s.URL, nil, []byte("{}"))
	if err != nil || status != 200 || h.n.Load() != 1 {
		t.Fatalf("%d, %v, %d hits", status, err, h.n.Load())
	}
	// And not when it is not: a certificate nobody vouches for is a failed delivery, not a delivery.
	if _, err := httpsend.New(httpsend.Options{Guard: events.Guard{AllowPrivate: true}}).Send(context.Background(), s.URL, nil, []byte("{}")); err == nil {
		t.Fatal("an untrusted certificate was accepted")
	}
}

func TestARedirectIsNotFollowedAndItsStatusIsTheAnswer(t *testing.T) {
	t.Parallel()
	var target, origin hits
	to := server(t, &target, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	from := server(t, &origin, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, to.URL+"/elsewhere", http.StatusFound)
	})
	status, err := httpsend.New(httpsend.Options{Guard: events.Guard{AllowPrivate: true}}).Send(context.Background(), from.URL, nil, []byte("{}"))
	if err != nil || status != http.StatusFound {
		t.Fatalf("%d, %v", status, err)
	}
	if target.n.Load() != 0 || origin.n.Load() != 1 {
		t.Fatalf("the redirect target was reached %d times, the origin %d", target.n.Load(), origin.n.Load())
	}
}

func TestAResponseBodyIsReadOnlyUpToTheCap(t *testing.T) {
	t.Parallel()
	var written atomic.Int64
	var h hits
	s := server(t, &h, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		chunk := make([]byte, 32<<10)
		for range 2000 { // 64 MB if anyone keeps reading
			n, err := w.Write(chunk)
			written.Add(int64(n))
			if err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	})
	start := time.Now()
	status, err := httpsend.New(httpsend.Options{Guard: events.Guard{AllowPrivate: true}, MaxResponse: 4 << 10}).Send(context.Background(), s.URL, nil, []byte("{}"))
	if err != nil || status != 200 {
		t.Fatalf("%d, %v", status, err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("took %v", took)
	}
	time.Sleep(200 * time.Millisecond)
	if got := written.Load(); got > 16<<20 {
		t.Fatalf("the server wrote %d bytes into a connection nobody was reading: the body is not capped", got)
	}
}

func TestAnEndpointThatNeverAnswersIsAFailureWithinTheTimeoutAndLeaksNothing(t *testing.T) {
	t.Parallel()
	var h hits
	release := make(chan struct{})
	s := server(t, &h, func(http.ResponseWriter, *http.Request) { <-release })
	defer close(release)
	start := time.Now()
	status, err := httpsend.New(httpsend.Options{Guard: events.Guard{AllowPrivate: true}, Timeout: 150 * time.Millisecond}).Send(context.Background(), s.URL, nil, []byte("{}"))
	if status != 0 || err == nil {
		t.Fatalf("%d, %v", status, err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("took %v", took)
	}
}

func TestACancelledContextStopsTheRequest(t *testing.T) {
	t.Parallel()
	var h hits
	release := make(chan struct{})
	s := server(t, &h, func(http.ResponseWriter, *http.Request) { <-release })
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := httpsend.New(httpsend.Options{Guard: events.Guard{AllowPrivate: true}}).Send(ctx, s.URL, nil, nil); err == nil {
		t.Fatal("a cancelled request reported success")
	}
}

func TestAnErrorDoesNotCarryTheDestinationURLWhichMayHoldAToken(t *testing.T) {
	t.Parallel()
	_, err := httpsend.New(httpsend.Options{}).Send(context.Background(), "https://127.0.0.1:1/hooks/SECRET-TOKEN-IN-THE-PATH", nil, nil)
	if err == nil || strings.Contains(err.Error(), "SECRET-TOKEN") {
		t.Fatalf("%v", err)
	}
}
