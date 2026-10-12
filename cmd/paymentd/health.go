package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const healthcheckTimeout = 5 * time.Second

// healthcheck asks this host's paymentd whether it may be sent traffic. It asks /readyz, not /healthz: a
// container that is alive and half-migrated is not one to route to. The configured address is where the
// process LISTENS — ":8080" is every interface — so the probe dials loopback.
func healthcheck(ctx context.Context, getenv func(string) string, stderr io.Writer) int {
	addr := strings.TrimSpace(getenv("PAYMENT_HTTP_ADDR"))
	if addr == "" {
		addr = ":8080"
	}
	scheme := "http"
	if strings.TrimSpace(getenv("PAYMENT_TLS_CERT_FILE")) != "" {
		scheme = "https"
	}
	tr := &http.Transport{
		// The probe talks to the process's own listener over loopback, so there is no one to be deceived by its
		// certificate; verifying it would need the CA bundle to be present in the probe's environment too.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // loopback probe of this process's own listener
	}
	host := "paymentd"
	if path, ok := strings.CutPrefix(addr, "unix:"); ok {
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}
		scheme = "http"
	} else {
		h, p, err := net.SplitHostPort(addr)
		if err != nil {
			fmt.Fprintf(stderr, "healthcheck: PAYMENT_HTTP_ADDR %q is not host:port\n", addr)
			return 2
		}
		if ip := net.ParseIP(h); h == "" || (ip != nil && ip.IsUnspecified()) {
			h = "127.0.0.1"
		}
		host = net.JoinHostPort(h, p)
	}
	defer tr.CloseIdleConnections()

	ctx, cancel := context.WithTimeout(ctx, healthcheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+host+"/readyz", http.NoBody)
	if err != nil {
		fmt.Fprintln(stderr, "healthcheck:", err)
		return 1
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		fmt.Fprintln(stderr, "healthcheck:", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		fmt.Fprintf(stderr, "healthcheck: /readyz answered %d: %s\n", resp.StatusCode, strings.TrimSpace(string(body)))
		return 1
	}
	return 0
}
