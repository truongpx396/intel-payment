package app

import (
	"fmt"
	"testing"
	"time"
)

// stepClock is a clock the test moves by hand.
type stepClock struct{ now time.Time }

func (c *stepClock) Now() time.Time { return c.now }

func TestTheCoalescerReopensExactlyAtTheEndOfItsWindow(t *testing.T) {
	t.Parallel()
	clk := &stepClock{now: time.Unix(1_700_000_000, 0)}
	c := newCoalescer(clk, time.Minute)
	if !c.allow("k") {
		t.Fatal("the first notification always goes")
	}
	clk.now = clk.now.Add(time.Minute - time.Nanosecond)
	if c.allow("k") {
		t.Fatal("still inside the window")
	}
	clk.now = clk.now.Add(time.Nanosecond) // exactly one window since the last one that went
	if !c.allow("k") {
		t.Fatal("a window is a minute, not a minute and a tick: the notification is due")
	}
}

// The map that remembers when each key last published is bounded: what has aged out is dropped once
// it grows past a few thousand keys, so a flood of distinct scopes cannot grow it without limit.
func TestTheCoalescerForgetsWhatHasAgedOut(t *testing.T) {
	t.Parallel()
	clk := &stepClock{now: time.Unix(1_700_000_000, 0)}
	c := newCoalescer(clk, time.Minute)
	for i := 0; i < 6000; i++ {
		c.allow(fmt.Sprintf("scope-%d", i))
	}
	clk.now = clk.now.Add(2 * time.Minute) // all of them have aged out
	c.allow("one-more")
	if n := len(c.last); n > 4097 {
		t.Fatalf("aged-out keys must be dropped once the map is large; it holds %d", n)
	}
}

// Dropping what has aged out must never drop what has NOT: a key still inside its window stays
// suppressed across the cleanup, or a flood of other scopes would re-open every block notification.
func TestTheCoalescerKeepsWhatIsStillInsideItsWindowWhenItCleansUp(t *testing.T) {
	t.Parallel()
	clk := &stepClock{now: time.Unix(1_700_000_000, 0)}
	c := newCoalescer(clk, time.Minute)
	for i := 0; i < 6000; i++ {
		c.allow(fmt.Sprintf("scope-%d", i))
	}
	clk.now = clk.now.Add(30 * time.Second) // half a window: nothing has aged out
	c.allow("one-more")                     // large enough to trigger the cleanup
	if c.allow("scope-0") {
		t.Fatal("scope-0 published 30s ago and is still inside its minute: the cleanup must not have forgotten it")
	}
}
