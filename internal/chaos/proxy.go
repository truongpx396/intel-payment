// Package chaos injects the failures the design claims to survive. Its first tool is a TCP proxy
// that can be cut and restored: a test points a client at the proxy instead of the server, and
// "Redis went away" becomes something that can be caused on demand, mid-traffic, without touching a
// container. (Killing and restarting the container itself is Phase 7's chaos suite, T131.)
package chaos

import (
	"io"
	"net"
	"sync"
)

// Proxy forwards TCP to a target until it is Cut.
type Proxy struct {
	target string
	ln     net.Listener

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	cut   bool
	wg    sync.WaitGroup
}

// NewProxy starts a proxy on a free local port that forwards to target.
func NewProxy(target string) (*Proxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &Proxy{target: target, ln: ln, conns: map[net.Conn]struct{}{}}
	p.wg.Add(1)
	go p.accept()
	return p, nil
}

// Addr is where clients should connect.
func (p *Proxy) Addr() string { return p.ln.Addr().String() }

func (p *Proxy) accept() {
	defer p.wg.Done()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		if p.cut {
			p.mu.Unlock()
			_ = c.Close()
			continue
		}
		p.conns[c] = struct{}{}
		p.mu.Unlock()
		p.wg.Add(1)
		go p.pipe(c)
	}
}

func (p *Proxy) pipe(client net.Conn) {
	defer p.wg.Done()
	p.mu.Lock()
	target := p.target
	p.mu.Unlock()
	up, err := net.Dial("tcp", target)
	if err != nil {
		p.drop(client)
		return
	}
	p.mu.Lock()
	p.conns[up] = struct{}{}
	p.mu.Unlock()
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go cp(up, client)
	go cp(client, up)
	<-done
	p.drop(client)
	p.drop(up)
	<-done
}

func (p *Proxy) drop(c net.Conn) {
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
	_ = c.Close()
}

// Cut drops every open connection and refuses new ones, as an outage does.
func (p *Proxy) Cut() {
	p.mu.Lock()
	p.cut = true
	open := make([]net.Conn, 0, len(p.conns))
	for c := range p.conns {
		open = append(open, c)
	}
	p.mu.Unlock()
	for _, c := range open {
		_ = c.Close()
	}
}

// Retarget points the proxy at another server and drops every open connection, so each client
// reconnects to it: a failover, as the client sees it. Connections are refused only while cut.
func (p *Proxy) Retarget(target string) {
	p.mu.Lock()
	p.target = target
	open := make([]net.Conn, 0, len(p.conns))
	for c := range p.conns {
		open = append(open, c)
	}
	p.mu.Unlock()
	for _, c := range open {
		_ = c.Close()
	}
}

// Restore accepts connections again.
func (p *Proxy) Restore() {
	p.mu.Lock()
	p.cut = false
	p.mu.Unlock()
}

// Close stops the proxy and waits for its goroutines.
func (p *Proxy) Close() {
	_ = p.ln.Close()
	p.Cut()
	p.wg.Wait()
}
