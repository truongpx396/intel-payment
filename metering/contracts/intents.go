package contracts

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// IntentStreamOptions configure one intent stream under test.
type IntentStreamOptions struct {
	AckWait time.Duration // an un-acknowledged delivery is redelivered after this
}

// IntentStreamHarness is the writer's view of one shard's intents, plus the means to put intents there
// the way a deployment does — Redis Streams reads the outbox itself; JetStream reads what a Relay has
// forwarded to it.
type IntentStreamHarness struct {
	Stream ports.IntentStream
	// Peer is another consumer of the same shard: another writer process.
	Peer func(t *testing.T) ports.IntentStream
	// Shard is a shard nothing else uses.
	Shard domain.Shard
	// Emit makes intents available to the writer, in order. EmitRaw does so for an entry that is not a
	// valid intent — the outbox is only ever written by the hot functions, but the writer must not
	// wedge on what a bug or a version skew leaves there.
	Emit    func(t *testing.T, ins ...domain.Intent)
	EmitRaw func(t *testing.T, fields map[string]string)
}

// IntentStreamContract is the specification of the writer's view of the outbox (hot-path-consistency.md
// §2–§3): entries in order, at least once, a dead consumer's work reclaimed, everything un-acked taken
// over at once by an owner that says so, malformed entries surfaced whole, honest statistics, and a trim
// that can never remove a money intent nobody has booked. It runs against the Redis outbox and the
// JetStream relay alike.
func IntentStreamContract(t *testing.T, newHarness func(t *testing.T, o IntentStreamOptions) IntentStreamHarness) {
	t.Helper()
	ctx := context.Background()
	var seq atomic.Int64
	sub := func(name string, f func(t *testing.T, h IntentStreamHarness)) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f(t, newHarness(t, IntentStreamOptions{AckWait: 400 * time.Millisecond}))
		})
	}
	const wait = 15 * time.Second
	scope := func() domain.Scope {
		return domain.Scope{Realm: "t1", Kind: "workspace", ID: fmt.Sprintf("w-%d", seq.Add(1))}
	}
	intents := func(s domain.Scope, from, n int) []domain.Intent {
		var out []domain.Intent
		for i := from; i < from+n; i++ {
			out = append(out, domain.Intent{
				Op: domain.OpGrant, Scope: s, Gen: 1, Seq: int64(i), IdemKey: fmt.Sprintf("k%d", i), Fingerprint: fmt.Sprintf("fp%d", i),
				Draws: []domain.PoolDelta{{Pool: domain.GeneralPool, Delta: domain.Credits(10 * i)}}, Payload: domain.Payload{Reason: "promo"},
			})
		}
		return out
	}
	// read collects up to n deliveries, reading until they have all arrived.
	read := func(t *testing.T, s ports.IntentStream, sh domain.Shard, n int, minIdle time.Duration) []ports.Delivery {
		t.Helper()
		var got []ports.Delivery
		deadline := time.Now().Add(wait)
		for len(got) < n {
			ds, err := s.Read(ctx, sh, n-len(got), minIdle)
			mustNoErr(t, err)
			got = append(got, ds...)
			if len(ds) == 0 {
				if time.Now().After(deadline) {
					t.Fatalf("read %d of %d deliveries", len(got), n)
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		return got
	}
	ids := func(ds []ports.Delivery) []string {
		out := make([]string, len(ds))
		for i, d := range ds {
			out[i] = d.ID
		}
		return out
	}

	sub("intents arrive whole and in the order they were written", func(t *testing.T, h IntentStreamHarness) {
		s := scope()
		want := intents(s, 1, 10)
		h.Emit(t, want...)
		got := read(t, h.Stream, h.Shard, 10, time.Minute)
		seen := map[string]bool{}
		for i, d := range got {
			if d.ParseErr != nil || d.ID == "" || seen[d.ID] || d.Attempts != 1 {
				t.Fatalf("delivery %d: %+v", i, d)
			}
			seen[d.ID] = true
			w := want[i]
			if d.Intent.Op != w.Op || d.Intent.Scope != w.Scope || d.Intent.Seq != w.Seq || d.Intent.Gen != w.Gen ||
				d.Intent.IdemKey != w.IdemKey || d.Intent.Fingerprint != w.Fingerprint || len(d.Intent.Draws) != 1 || d.Intent.Draws[0] != w.Draws[0] {
				t.Fatalf("delivery %d is not what was written:\n got %+v\nwant %+v", i, d.Intent, w)
			}
		}
	})

	sub("a read never returns more than it was asked for", func(t *testing.T, h IntentStreamHarness) {
		h.Emit(t, intents(scope(), 1, 10)...)
		got := read(t, h.Stream, h.Shard, 1, time.Minute)
		ds, err := h.Stream.Read(ctx, h.Shard, 4, time.Minute)
		mustNoErr(t, err)
		if len(got) != 1 || len(ds) > 4 {
			t.Fatalf("%d then %d", len(got), len(ds))
		}
	})

	sub("an acknowledged intent is never delivered again; an un-acknowledged one is, oldest first, with a rising count", func(t *testing.T, h IntentStreamHarness) {
		h.Emit(t, intents(scope(), 1, 6)...)
		first := read(t, h.Stream, h.Shard, 6, time.Minute)
		mustNoErr(t, h.Stream.Ack(ctx, h.Shard, ids(first[:3])...)) // 1–3 booked; 4–6 not

		time.Sleep(3 * 400 * time.Millisecond) // past the ack wait
		again := read(t, h.Stream, h.Shard, 3, 400*time.Millisecond)
		for i, d := range again {
			if d.Intent.Seq != int64(i+4) || d.Attempts < 2 {
				t.Fatalf("redelivery %d: seq %d attempts %d", i, d.Intent.Seq, d.Attempts)
			}
		}
		time.Sleep(200 * time.Millisecond)
		if ds, err := h.Stream.Read(ctx, h.Shard, 10, time.Minute); err != nil {
			t.Fatal(err)
		} else {
			for _, d := range ds {
				if d.Intent.Seq <= 3 {
					t.Fatalf("seq %d was acknowledged and delivered again", d.Intent.Seq)
				}
			}
		}
	})

	sub("another consumer takes over what a dead one never acknowledged", func(t *testing.T, h IntentStreamHarness) {
		h.Emit(t, intents(scope(), 1, 5)...)
		read(t, h.Stream, h.Shard, 5, time.Minute) // taken, then the process dies
		time.Sleep(3 * 400 * time.Millisecond)
		got := read(t, h.Peer(t), h.Shard, 5, 400*time.Millisecond)
		for i, d := range got {
			if d.Intent.Seq != int64(i+1) || d.Attempts < 2 {
				t.Fatalf("takeover %d: %+v", i, d)
			}
		}
	})

	sub("an owner that asks for minIdle 0 gets everything un-acknowledged at once — the recovery drain", func(t *testing.T, h IntentStreamHarness) {
		h.Emit(t, intents(scope(), 1, 5)...)
		first := read(t, h.Stream, h.Shard, 5, time.Minute)
		mustNoErr(t, h.Stream.Ack(ctx, h.Shard, ids(first[:2])...))
		got := read(t, h.Stream, h.Shard, 3, 0) // no waiting for the ack wait
		for i, d := range got {
			if d.Intent.Seq < 3 || d.Intent.Seq > 5 || d.Intent.Seq != got[0].Intent.Seq+int64(i) {
				t.Fatalf("seq %d out of order or already acknowledged", d.Intent.Seq)
			}
		}
	})

	sub("an entry that is not a valid intent is surfaced whole, never dropped", func(t *testing.T, h IntentStreamHarness) {
		h.EmitRaw(t, map[string]string{"op": "usage", "realm": "t1", "kind": "workspace", "id": "w-x", "seq": "not-a-number", "idem": "k"})
		d := read(t, h.Stream, h.Shard, 1, time.Minute)[0]
		if d.ParseErr == nil || d.Fields["seq"] != "not-a-number" || d.Fields["idem"] != "k" {
			t.Fatalf("%+v", d)
		}
		mustNoErr(t, h.Stream.Ack(ctx, h.Shard, d.ID))
	})

	sub("acknowledging what this consumer never held is harmless", func(t *testing.T, h IntentStreamHarness) {
		if err := h.Stream.Ack(ctx, h.Shard, "999999999-0", "424242"); err != nil {
			t.Fatalf("%v", err)
		}
		if err := h.Stream.Ack(ctx, h.Shard); err != nil {
			t.Fatalf("%v", err)
		}
	})

	sub("statistics are honest: stored, un-acknowledged, not yet delivered, and their ages", func(t *testing.T, h IntentStreamHarness) {
		h.Emit(t, intents(scope(), 1, 6)...)
		time.Sleep(150 * time.Millisecond)
		st, err := h.Stream.Stats(ctx, h.Shard)
		mustNoErr(t, err)
		if st.Length != 6 || st.Undelivered != 6 || st.Pending != 0 || st.OldestUndelivered < 100*time.Millisecond {
			t.Fatalf("before any read: %+v", st)
		}
		first := read(t, h.Stream, h.Shard, 4, time.Minute)
		st, _ = h.Stream.Stats(ctx, h.Shard)
		if st.Pending != 4 || st.Undelivered != 2 || st.OldestPendingAge < 100*time.Millisecond {
			t.Fatalf("four delivered, two waiting: %+v", st)
		}
		mustNoErr(t, h.Stream.Ack(ctx, h.Shard, ids(first)...))
		eventually(t, wait, "nothing pending after the ack", func() bool {
			st, err := h.Stream.Stats(ctx, h.Shard)
			return err == nil && st.Pending == 0 && st.Undelivered == 2
		})
	})

	sub("Trim never removes an intent nobody has booked", func(t *testing.T, h IntentStreamHarness) {
		if n, err := h.Stream.Trim(ctx, h.Shard, ports.RetentionPolicy{}); err != nil || n != 0 {
			t.Fatalf("a shard nobody has read: removed %d %v", n, err)
		}
		h.Emit(t, intents(scope(), 1, 6)...)
		first := read(t, h.Stream, h.Shard, 4, time.Minute)
		if n, err := h.Stream.Trim(ctx, h.Shard, ports.RetentionPolicy{}); err != nil || n != 0 {
			t.Fatalf("delivered but un-acknowledged: removed %d %v", n, err)
		}
		mustNoErr(t, h.Stream.Ack(ctx, h.Shard, ids(first[:2])...))
		time.Sleep(50 * time.Millisecond)
		if n, err := h.Stream.Trim(ctx, h.Shard, ports.RetentionPolicy{MinAge: time.Hour}); err != nil || n != 0 {
			t.Fatalf("the floor keeps recent history: removed %d %v", n, err)
		}
		n, err := h.Stream.Trim(ctx, h.Shard, ports.RetentionPolicy{})
		mustNoErr(t, err)
		st, _ := h.Stream.Stats(ctx, h.Shard)
		if n != 2 || st.Length != 4 {
			t.Fatalf("the two acknowledged go, the four others stay: removed %d, length %d", n, st.Length)
		}
		// What is left is still delivered: the two that were never read (and, once the ack wait has
		// passed, the two that were read and not acknowledged).
		seen := map[int64]bool{}
		deadline := time.Now().Add(wait)
		for !seen[5] || !seen[6] {
			ds, err := h.Stream.Read(ctx, h.Shard, 10, 400*time.Millisecond)
			mustNoErr(t, err)
			for _, d := range ds {
				seen[d.Intent.Seq] = true
			}
			if time.Now().After(deadline) {
				t.Fatalf("the intents behind the trim were never delivered: %v", seen)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}
