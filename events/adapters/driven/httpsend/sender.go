// Package httpsend sends a signed webhook. It is where the SSRF guard bites: a destination the guard refuses is an error before any
// byte is sent, a redirect is not followed, no proxy is consulted, and the response is read only up to a cap.
package httpsend

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/truongpx396/intel-payment/events"
)

// Defaults.
const (
	DefaultTimeout     = 10 * time.Second // a 2xx within 10 s is delivered (outbound-events.md)
	DefaultMaxResponse = 64 << 10         // a webhook's answer is a status; nothing it says is kept
)

// Options configure a Sender.
type Options struct {
	Guard events.Guard
	// Timeout bounds the whole exchange. Default DefaultTimeout.
	Timeout time.Duration
	// MaxResponse is how much of a response body is read before the connection is dropped. Default DefaultMaxResponse.
	MaxResponse int64
	// RootCAs replaces the system roots, for a receiver with a private certificate authority and for tests. Nil = the system's.
	RootCAs *x509.CertPool
}

// Sender implements events.Sender.
type Sender struct {
	client *http.Client
	max    int64
}

var _ events.Sender = (*Sender)(nil)

// New builds a sender.
func New(o Options) *Sender {
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.MaxResponse <= 0 {
		o.MaxResponse = DefaultMaxResponse
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: o.Guard.Control}
	transport := &http.Transport{
		// No proxy, whatever the environment says: a proxy would be dialled instead of the destination, and the guard would judge the
		// proxy's address and not the one the request is for.
		Proxy:                  nil,
		DialContext:            dialer.DialContext,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: o.RootCAs},
		TLSHandshakeTimeout:    5 * time.Second,
		ResponseHeaderTimeout:  o.Timeout,
		MaxResponseHeaderBytes: 16 << 10,
		DisableKeepAlives:      true, // a delivery is rare, and a kept connection is a standing route to whatever it resolved to
		ForceAttemptHTTP2:      true,
	}
	guard := o.Guard
	return &Sender{
		max: o.MaxResponse,
		client: &http.Client{
			Transport: guarded{rt: transport, guard: guard},
			Timeout:   o.Timeout,
			// A redirect is a different destination nobody vetted. It is not followed; its status is the answer, and a 3xx is not a 2xx.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// guarded judges the URL before the transport is asked to connect to anything.
type guarded struct {
	rt    http.RoundTripper
	guard events.Guard
}

func (g guarded) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := g.guard.CheckURL(r.URL.String()); err != nil {
		return nil, err
	}
	return g.rt.RoundTrip(r)
}

// Send POSTs body to url with the headers given and returns the endpoint's status. A status is not an error: whether it is a success is
// the dispatcher's to say. An error is a request that got no answer, and carries status 0.
func (s *Sender) Send(ctx context.Context, url string, header map[string]string, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("%w: not a URL", events.ErrForbiddenAddress)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	req.Header.Set("User-Agent", "intel-payment-webhooks")
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, scrub(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, s.max))
	return resp.StatusCode, nil
}

// scrub keeps the cause an operator needs and drops the URL net/http puts in its errors: a webhook URL can carry a token in its path.
func scrub(err error) error {
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) {
		if inner := ue.Unwrap(); inner != nil {
			return inner
		}
	}
	return err
}
