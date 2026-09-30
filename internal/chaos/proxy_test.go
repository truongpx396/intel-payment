package chaos

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	code := m.Run()
	if code == 0 {
		if err := goleak.Find(); err != nil {
			fmt.Fprintln(os.Stderr, "goleak:", err)
			code = 1
		}
	}
	os.Exit(code)
}

// server answers every line with its own name, so a test can tell which backend it reached.
func server(t *testing.T, name string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { _ = ln.Close(); <-done })
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				r := bufio.NewReader(c)
				for {
					if _, err := r.ReadString('\n'); err != nil {
						return
					}
					_, _ = fmt.Fprintln(c, name)
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func ask(t *testing.T, addr string) (string, error) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(time.Second))
	if _, err := fmt.Fprintln(c, "who"); err != nil {
		return "", err
	}
	return bufio.NewReader(c).ReadString('\n')
}

func TestProxyForwardsCutsRestoresAndRetargets(t *testing.T) {
	t.Parallel()
	a, b := server(t, "a"), server(t, "b")
	p, err := NewProxy(a)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)

	if got, err := ask(t, p.Addr()); err != nil || got != "a\n" {
		t.Fatalf("forwarded to %q: %v", got, err)
	}

	p.Cut()
	if got, err := ask(t, p.Addr()); err == nil {
		t.Fatalf("a cut proxy must not answer, got %q", got)
	}
	p.Restore()
	if got, err := ask(t, p.Addr()); err != nil || got != "a\n" {
		t.Fatalf("restored: %q %v", got, err)
	}

	// A failover: the same address now reaches the other server, and a connection opened before it is
	// dropped so that its client reconnects.
	c, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(time.Second))
	r := bufio.NewReader(c)
	// One round trip first: only then is the connection certainly established through the proxy.
	_, _ = fmt.Fprintln(c, "who")
	if got, err := r.ReadString('\n'); err != nil || got != "a\n" {
		t.Fatalf("the open connection reached %q: %v", got, err)
	}
	p.Retarget(b)
	_, _ = fmt.Fprintln(c, "who")
	if _, err := r.ReadString('\n'); err == nil {
		t.Fatal("an open connection must be dropped by a retarget")
	}
	if got, err := ask(t, p.Addr()); err != nil || got != "b\n" {
		t.Fatalf("retargeted: %q %v", got, err)
	}
}
