package contracts

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// BusOptions configure one bus under test. Prefix isolates it: every subject the suite uses begins
// with it, so subtests sharing a broker never see each other's traffic.
type BusOptions struct {
	Prefix      string
	AckWait     time.Duration // an un-acked delivery is redelivered to another consumer after this
	MaxAttempts int           // deliveries after which a failing message is dead-lettered
	Backoff     time.Duration // the delay after the first failed attempt
}

// BusHarness is a bus under test and a second handle onto it — another process on the same broker.
type BusHarness struct {
	Bus  ports.Bus
	Peer func(t *testing.T) ports.Bus
}

// BusContract is the specification of the async seam for events and ticks (bus-subjects.md): durable,
// at-least-once, competing consumers within a group and fan-out across groups, redelivery of a stalled
// consumer's work, a dead-letter subject that never drops, wildcards, and a trim that can never remove
// what nobody has acknowledged. It runs against BOTH adapters — the only way "swappable" means
// anything (SC-014). Intents are not in it: they are written by the hot functions, not published.
func BusContract(t *testing.T, newHarness func(t *testing.T, o BusOptions) BusHarness) {
	t.Helper()
	ctx := context.Background()
	var seq atomic.Int64
	sub := func(name string, f func(t *testing.T, p string, h BusHarness)) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := fmt.Sprintf("bus%d", seq.Add(1))
			f(t, p, newHarness(t, BusOptions{Prefix: p, AckWait: 400 * time.Millisecond, MaxAttempts: 3, Backoff: 30 * time.Millisecond}))
		})
	}
	const wait = 15 * time.Second

	publish := func(t *testing.T, b ports.Bus, subject string, payloads ...[]byte) {
		t.Helper()
		for _, pl := range payloads {
			mustNoErr(t, b.Publish(ctx, subject, pl))
		}
	}
	listen := func(t *testing.T, b ports.Bus, subject, group string, h ports.Handler) ports.Subscription {
		t.Helper()
		s, err := b.Subscribe(ctx, subject, group, h)
		mustNoErr(t, err)
		t.Cleanup(func() { _ = s.Close() })
		return s
	}

	sub("a message published before anyone subscribed is delivered, whole and in order", func(t *testing.T, p string, h BusHarness) {
		subj := p + ".warn.a"
		publish(t, h.Bus, subj, []byte("one"), []byte("two"), []byte("three"))
		var c collector
		listen(t, h.Bus, p+".warn.>", "g", c.handler(nil))
		c.waitFor(t, 3, wait)
		for i, want := range []string{"one", "two", "three"} {
			if m := c.at(i); string(m.Payload) != want || m.Subject != subj || m.Attempts != 1 || m.ID == "" {
				t.Fatalf("delivery %d: %+v", i, m)
			}
		}
	})

	sub("binary payloads survive intact", func(t *testing.T, p string, h BusHarness) {
		all := make([]byte, 256)
		for i := range all {
			all[i] = byte(i)
		}
		big := make([]byte, 1<<20)
		_, _ = rand.Read(big)
		publish(t, h.Bus, p+".config.x", all, big, nil)
		var c collector
		listen(t, h.Bus, p+".config.>", "g", c.handler(nil))
		c.waitFor(t, 3, wait)
		if !bytes.Equal(c.at(0).Payload, all) || !bytes.Equal(c.at(1).Payload, big) || len(c.at(2).Payload) != 0 {
			t.Fatal("a payload was altered in transit")
		}
	})

	sub("an acknowledged message is never delivered again, and nothing is left pending", func(t *testing.T, p string, h BusHarness) {
		subj := p + ".warn.a"
		var c collector
		listen(t, h.Bus, subj, "g", c.handler(nil))
		publish(t, h.Bus, subj, []byte("x"), []byte("y"))
		c.waitFor(t, 2, wait)
		eventually(t, wait, "the group has nothing pending", func() bool {
			n, _, err := h.Bus.Pending(ctx, subj, "g")
			return err == nil && n == 0
		})
		time.Sleep(3 * 400 * time.Millisecond) // three AckWaits: a redelivery would have happened
		if c.len() != 2 {
			t.Fatalf("delivered %d times, want 2", c.len())
		}
	})

	sub("messages of one subject are delivered in the order they were published", func(t *testing.T, p string, h BusHarness) {
		subj := p + ".payment.a"
		var c collector
		listen(t, h.Bus, subj, "g", c.handler(nil))
		for i := 0; i < 50; i++ {
			publish(t, h.Bus, subj, []byte(fmt.Sprintf("%03d", i)))
		}
		c.waitFor(t, 50, wait)
		for i := 0; i < 50; i++ {
			if got := string(c.at(i).Payload); got != fmt.Sprintf("%03d", i) {
				t.Fatalf("position %d holds %s", i, got)
			}
		}
	})

	sub("a failing handler is redelivered with a rising attempt count, then dead-lettered whole — never dropped", func(t *testing.T, p string, h BusHarness) {
		subj := p + ".payment.a"
		var c, dead collector
		listen(t, h.Bus, subj, "g", c.handler(func(ports.Message) error { return fmt.Errorf("boom") }))
		listen(t, h.Bus, p+".dlq.>", "dlq", dead.handler(nil))
		publish(t, h.Bus, subj, []byte("poison"))

		c.waitFor(t, 3, wait)
		for i := 0; i < 3; i++ {
			if got := c.at(i).Attempts; got != i+1 {
				t.Fatalf("delivery %d carries attempt %d", i, got)
			}
		}
		dead.waitFor(t, 1, wait)
		var d struct {
			Subject  string `json:"subject"`
			Attempts int    `json:"attempts"`
			Payload  []byte `json:"payload"`
		}
		mustNoErr(t, json.Unmarshal(dead.at(0).Payload, &d))
		if d.Subject != subj || d.Attempts != 3 || string(d.Payload) != "poison" {
			t.Fatalf("the dead letter is the original message and its attempts: %+v", d)
		}
		if dead.at(0).Subject != p+".dlq."+subj {
			t.Fatalf("dead letters go to <prefix>.dlq.<subject>: %s", dead.at(0).Subject)
		}
		time.Sleep(3 * 400 * time.Millisecond)
		if c.len() != 3 || dead.len() != 1 {
			t.Fatalf("after the DLQ it is not redelivered: %d deliveries, %d dead letters", c.len(), dead.len())
		}
		eventually(t, wait, "nothing pending", func() bool {
			n, _, err := h.Bus.Pending(ctx, subj, "g")
			return err == nil && n == 0
		})
	})

	sub("a handler that panics is a failed handler, not a dead subscription", func(t *testing.T, p string, h BusHarness) {
		subj := p + ".payment.a"
		var c collector
		var first atomic.Bool
		listen(t, h.Bus, subj, "g", c.handler(func(ports.Message) error {
			if first.CompareAndSwap(false, true) {
				panic("handler bug")
			}
			return nil
		}))
		publish(t, h.Bus, subj, []byte("x"))
		c.waitFor(t, 2, wait) // the panic, then the redelivery that succeeds
		if c.at(1).Attempts != 2 {
			t.Fatalf("%+v", c.at(1))
		}
	})

	for _, kind := range []string{"payment", "reconcile.tick"} { // events, and ticks
		sub("consumers in a group share the work exactly once; every group sees everything ("+kind+")", func(t *testing.T, p string, h BusHarness) {
			subj := p + "." + kind
			var a, b, other collector
			listen(t, h.Bus, subj, "workers", a.handler(nil))
			listen(t, h.Peer(t), subj, "workers", b.handler(nil))
			listen(t, h.Bus, subj, "audit", other.handler(nil))
			const n = 40
			for i := 0; i < n; i++ {
				publish(t, h.Bus, subj, []byte(fmt.Sprintf("m%02d", i)))
			}
			other.waitFor(t, n, wait)
			eventually(t, wait, "the group has handled every message", func() bool { return a.len()+b.len() >= n })
			time.Sleep(200 * time.Millisecond)
			seen := map[string]int{}
			for _, m := range append(a.all(), b.all()...) {
				seen[string(m.Payload)]++
			}
			if len(seen) != n {
				t.Fatalf("the group handled %d distinct messages of %d", len(seen), n)
			}
			for pl, k := range seen {
				if k != 1 {
					t.Fatalf("%s was handled %d times by a group whose handlers never fail", pl, k)
				}
			}
			if a.len() == 0 || b.len() == 0 {
				t.Logf("note: one consumer took all the work (%d / %d)", a.len(), b.len())
			}
		})
	}

	sub("wildcards select subjects: one token with *, the rest with >", func(t *testing.T, p string, h BusHarness) {
		var warn, star collector
		listen(t, h.Bus, p+".warn.>", "g1", warn.handler(nil))
		listen(t, h.Bus, p+".*.a", "g2", star.handler(nil))
		publish(t, h.Bus, p+".warn.a", []byte("1"))
		publish(t, h.Bus, p+".blocked.a", []byte("2"))
		publish(t, h.Bus, p+".warn.b", []byte("3"))
		publish(t, h.Bus, p+".blocked.b", []byte("4"))
		warn.waitFor(t, 2, wait)
		star.waitFor(t, 2, wait)
		time.Sleep(300 * time.Millisecond)
		if got := payloads(warn.all()); got != "1,3" {
			t.Fatalf("warn.>: %s", got)
		}
		if got := payloads(star.all()); got != "1,2" {
			t.Fatalf("*.a: %s", got)
		}
	})

	sub("one group name over two subject patterns is two subscriptions, each sees only its own", func(t *testing.T, p string, h BusHarness) {
		var warn, blocked collector
		listen(t, h.Bus, p+".warn.>", "outbound", warn.handler(nil)) // the same group name…
		listen(t, h.Bus, p+".blocked.>", "outbound", blocked.handler(nil))
		for i := 0; i < 20; i++ {
			publish(t, h.Bus, p+".warn.a", []byte(fmt.Sprintf("w%d", i)))
			publish(t, h.Bus, p+".blocked.a", []byte(fmt.Sprintf("b%d", i)))
		}
		warn.waitFor(t, 20, wait)
		blocked.waitFor(t, 20, wait)
		time.Sleep(200 * time.Millisecond)
		if warn.len() != 20 || blocked.len() != 20 {
			t.Fatalf("warn saw %d, blocked saw %d: a consumer of one pattern swallowed the other's messages", warn.len(), blocked.len())
		}
	})

	sub("one product's subject can never be consumed as another's", func(t *testing.T, p string, h BusHarness) {
		a := domain.Scope{Realm: "prod-a", Kind: "workspace", ID: "w.1"}
		b := domain.Scope{Realm: "prod-b", Kind: "workspace", ID: "w.1"}
		subjA, subjB := p+".warn."+a.SubjectToken(), p+".warn."+b.SubjectToken()
		var inA, inB collector
		listen(t, h.Bus, subjA, "g", inA.handler(nil))
		listen(t, h.Bus, subjB, "g", inB.handler(nil))
		publish(t, h.Bus, subjA, []byte("for A"))
		inA.waitFor(t, 1, wait)
		time.Sleep(300 * time.Millisecond)
		if inB.len() != 0 {
			t.Fatal("realm B consumed realm A's message")
		}
		if n, _, err := h.Bus.Pending(ctx, subjB, "g"); err != nil || n != 0 {
			t.Fatalf("realm B has %d pending: %v", n, err)
		}
	})

	sub("work a stalled consumer never acknowledged is redelivered to another in its group", func(t *testing.T, p string, h BusHarness) {
		subj := p + ".payment.a"
		release := make(chan struct{})
		var stalled, rescuer collector
		listen(t, h.Bus, subj, "g", stalled.handler(func(ports.Message) error { <-release; return nil })) // takes it and hangs
		t.Cleanup(func() { close(release) })
		publish(t, h.Bus, subj, []byte("x"))
		stalled.waitFor(t, 1, wait)

		listen(t, h.Peer(t), subj, "g", rescuer.handler(nil))
		rescuer.waitFor(t, 1, wait)
		if got := rescuer.at(0); string(got.Payload) != "x" || got.Attempts < 2 {
			t.Fatalf("the takeover is a redelivery: %+v", got)
		}
		eventually(t, wait, "nothing pending once the rescuer acknowledged", func() bool {
			n, _, err := h.Bus.Pending(ctx, subj, "g")
			return err == nil && n == 0
		})
	})

	sub("Pending reports the count and a non-zero age for a stalled consumer, within one ack-wait", func(t *testing.T, p string, h BusHarness) {
		subj := p + ".payment.a"
		release := make(chan struct{})
		var stalled collector
		listen(t, h.Bus, subj, "g", stalled.handler(func(ports.Message) error { <-release; return nil }))
		t.Cleanup(func() { close(release) })
		publish(t, h.Bus, subj, []byte("x"), []byte("y"))
		stalled.waitFor(t, 1, wait)
		time.Sleep(120 * time.Millisecond)
		eventually(t, 400*time.Millisecond+wait/10, "pending shows the stalled work and its age", func() bool {
			n, oldest, err := h.Bus.Pending(ctx, subj, "g")
			return err == nil && n >= 1 && oldest >= 100*time.Millisecond
		})
	})

	sub("Trim never removes what a group has not acknowledged", func(t *testing.T, p string, h BusHarness) {
		subj := p + ".payment.a"
		release := make(chan struct{})
		var c collector
		var n atomic.Int64
		listen(t, h.Bus, subj, "g", c.handler(func(ports.Message) error {
			if n.Add(1) == 4 {
				<-release // the fourth is in hand and un-acknowledged; six more wait behind it
			}
			return nil
		}))
		for i := 0; i < 10; i++ {
			publish(t, h.Bus, subj, []byte(fmt.Sprintf("m%d", i)))
		}
		eventually(t, wait, "three acknowledged, the fourth held", func() bool { return c.len() >= 4 })
		for i := 0; i < 3; i++ {
			mustNoErr(t, h.Bus.Trim(ctx, subj, ports.RetentionPolicy{}))
		}
		close(release)
		c.waitFor(t, 10, wait)
		seen := map[string]bool{}
		for _, m := range c.all() {
			seen[string(m.Payload)] = true
		}
		if len(seen) != 10 {
			t.Fatalf("a trim removed %d un-acknowledged messages", 10-len(seen))
		}
		if err := h.Bus.Trim(ctx, subj, ports.RetentionPolicy{MinAge: time.Hour, MaxLen: 1}); err != nil {
			t.Fatalf("a retention floor is not an error: %v", err)
		}
	})

	sub("a group nobody has joined has nothing pending", func(t *testing.T, p string, h BusHarness) {
		n, oldest, err := h.Bus.Pending(ctx, p+".warn.a", "nobody")
		if err != nil || n != 0 || oldest != 0 {
			t.Fatalf("%d %v %v", n, oldest, err)
		}
	})

	sub("closing a subscription stops delivery and leaves the messages for the group", func(t *testing.T, p string, h BusHarness) {
		subj := p + ".payment.a"
		var first collector
		s := listen(t, h.Bus, subj, "g", first.handler(nil))
		publish(t, h.Bus, subj, []byte("before"))
		first.waitFor(t, 1, wait)
		mustNoErr(t, s.Close())
		mustNoErr(t, s.Close()) // idempotent

		publish(t, h.Bus, subj, []byte("after"))
		time.Sleep(400 * time.Millisecond)
		if first.len() != 1 {
			t.Fatal("a closed subscription delivered")
		}
		var second collector
		listen(t, h.Bus, subj, "g", second.handler(nil))
		second.waitFor(t, 1, wait)
		if string(second.at(0).Payload) != "after" {
			t.Fatalf("the group resumes where it stopped: %s", second.at(0).Payload)
		}
	})

	sub("a bad subject is refused", func(t *testing.T, p string, h BusHarness) {
		for _, bad := range []string{"", "has space", ".lead", "trail.", "a..b", p + ".*", p + ".>"} {
			if err := h.Bus.Publish(ctx, bad, []byte("x")); err == nil {
				t.Errorf("publish %q must be refused", bad)
			}
		}
	})
}

// collector records deliveries.
type collector struct {
	mu   sync.Mutex
	msgs []ports.Message
}

// handler records each message, then defers to f (nil = acknowledge).
func (c *collector) handler(f func(ports.Message) error) ports.Handler {
	return func(_ context.Context, m ports.Message) error {
		c.mu.Lock()
		c.msgs = append(c.msgs, m)
		c.mu.Unlock()
		if f != nil {
			return f(m)
		}
		return nil
	}
}

func (c *collector) len() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.msgs) }
func (c *collector) at(i int) ports.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.msgs[i]
}
func (c *collector) all() []ports.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ports.Message(nil), c.msgs...)
}

func (c *collector) waitFor(t *testing.T, n int, d time.Duration) {
	t.Helper()
	eventually(t, d, fmt.Sprintf("%d deliveries (have %d)", n, c.len()), func() bool { return c.len() >= n })
}

func eventually(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func payloads(ms []ports.Message) string {
	var out []string
	for _, m := range ms {
		out = append(out, string(m.Payload))
	}
	sort.Strings(out)
	s := ""
	for i, o := range out {
		if i > 0 {
			s += ","
		}
		s += o
	}
	return s
}
