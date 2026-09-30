//go:build integration

package natsjetstream_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/truongpx396/intel-payment/metering/adapters/driven/natsjetstream"
	hotredis "github.com/truongpx396/intel-payment/metering/adapters/driven/redis"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/redisstreams"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/system"
	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// relayed is the outbox as a JetStream deployment sees it: the hot functions write Redis, a Relay
// forwards, and the writer reads JetStream.
type relayed struct {
	t      *testing.T
	rdb    goredis.UniversalClient
	shard  domain.Shard
	relay  *natsjetstream.Relay
	prefix string
}

func newRelayed(t *testing.T, src ports.IntentStream, o natsjetstream.IntentsOptions, shard domain.Shard, rdb goredis.UniversalClient) *relayed {
	t.Helper()
	r, err := natsjetstream.NewRelay(context.Background(), connect(t), src, natsjetstream.RelayOptions{Intents: o, AckWait: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return &relayed{t: t, rdb: rdb, shard: shard, relay: r, prefix: o.Prefix}
}

// pump forwards until the outbox is empty.
func (r *relayed) pump() {
	r.t.Helper()
	for i := 0; i < 50; i++ {
		n, err := r.relay.Pump(context.Background(), r.shard)
		if err != nil {
			r.t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
}

func (r *relayed) xadd(fields map[string]string) {
	r.t.Helper()
	vals := map[string]any{}
	for k, v := range fields {
		vals[k] = v
	}
	if err := r.rdb.XAdd(context.Background(), &goredis.XAddArgs{Stream: hotredis.OutboxKey(r.shard), Values: vals}).Err(); err != nil {
		r.t.Fatal(err)
	}
}

func relaySource(t *testing.T, rdb goredis.UniversalClient) *redisstreams.Stream {
	t.Helper()
	src, err := redisstreams.NewStream(rdb, redisstreams.IntentOptions{Consumer: "relay-1", Group: fmt.Sprintf("relay-%d", seq.Add(1)), Clock: system.Clock{}})
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func TestIntentStreamContract(t *testing.T) {
	t.Parallel()
	contracts.IntentStreamContract(t, func(t *testing.T, o contracts.IntentStreamOptions) contracts.IntentStreamHarness {
		rdb := rds.Client()
		t.Cleanup(func() { _ = rdb.Close() })
		shard := domain.Shard(30000 + seq.Add(1))
		io := natsjetstream.IntentsOptions{Clock: system.Clock{}, Prefix: fmt.Sprintf("ist%d", seq.Add(1)), AckWait: o.AckWait, FetchWait: 100 * time.Millisecond}
		r := newRelayed(t, relaySource(t, rdb), io, shard, rdb)
		open := func(t *testing.T) *natsjetstream.Intents {
			s, err := natsjetstream.NewIntents(context.Background(), connect(t), io)
			if err != nil {
				t.Fatal(err)
			}
			return s
		}
		return contracts.IntentStreamHarness{
			Stream: open(t), Shard: shard,
			Peer: func(t *testing.T) ports.IntentStream { return open(t) },
			Emit: func(t *testing.T, ins ...domain.Intent) {
				t.Helper()
				for _, in := range ins {
					f, err := in.Fields()
					if err != nil {
						t.Fatal(err)
					}
					r.xadd(f)
				}
				r.pump()
			},
			EmitRaw: func(t *testing.T, f map[string]string) { t.Helper(); r.xadd(f); r.pump() },
		}
	})
}

// ackSpy wraps the relay's source: it can refuse an acknowledgement (the relay crashed before it
// reached Redis) and it looks at JetStream at the moment an acknowledgement arrives.
type ackSpy struct {
	ports.IntentStream
	refuse  bool
	onAck   func(n int)
	crashes int
}

func (a *ackSpy) Ack(ctx context.Context, sh domain.Shard, ids ...string) error {
	if a.onAck != nil {
		a.onAck(len(ids))
	}
	if a.refuse {
		a.crashes++
		return fmt.Errorf("relay crashed before acknowledging")
	}
	return a.IntentStream.Ack(ctx, sh, ids...)
}

func emitN(t *testing.T, r *relayed, s domain.Scope, from, n int) {
	t.Helper()
	for i := from; i < from+n; i++ {
		f, err := domain.Intent{
			Op: domain.OpGrant, Scope: s, Gen: 1, Seq: int64(i), IdemKey: fmt.Sprintf("k%d", i), Fingerprint: "fp",
			Draws: []domain.PoolDelta{{Pool: domain.GeneralPool, Delta: 10}},
		}.Fields()
		if err != nil {
			t.Fatal(err)
		}
		r.xadd(f)
	}
}

// The relay acknowledges an outbox entry only after JetStream has stored it, and a relay that dies
// between the two republishes: JetStream recognises the message id, so the writer sees each intent once.
func TestARelayThatDiedBetweenPublishAndAckRepublishesAndJetStreamRecognisesIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rdb := rds.Client()
	t.Cleanup(func() { _ = rdb.Close() })
	shard := domain.Shard(40000 + seq.Add(1))
	io := natsjetstream.IntentsOptions{Clock: system.Clock{}, Prefix: fmt.Sprintf("rly%d", seq.Add(1)), AckWait: time.Minute}
	writer, err := natsjetstream.NewIntents(ctx, connect(t), io)
	if err != nil {
		t.Fatal(err)
	}
	src := &ackSpy{IntentStream: relaySource(t, rdb), refuse: true}
	stored := func() int64 { st, _ := writer.Stats(ctx, shard); return st.Length }
	src.onAck = func(n int) {
		if stored() < int64(n) {
			t.Errorf("acknowledged %d outbox entries in Redis while JetStream holds %d: an entry was acknowledged before it was stored", n, stored())
		}
	}
	r := newRelayed(t, src, io, shard, rdb)
	sc := domain.Scope{Realm: "t1", Kind: "workspace", ID: "w"}
	emitN(t, r, sc, 1, 5)

	if _, err := r.relay.Pump(ctx, shard); err == nil {
		t.Fatal("the crash must surface")
	}
	if stored() != 5 {
		t.Fatalf("JetStream stored %d, want 5", stored())
	}

	// The relay restarts. The entries were never acknowledged in Redis, so it reads them again after
	// its ack wait and publishes them again.
	src.refuse = false
	time.Sleep(300 * time.Millisecond)
	var forwarded int
	for i := 0; i < 20 && forwarded < 5; i++ {
		n, err := r.relay.Pump(ctx, shard)
		if err != nil {
			t.Fatal(err)
		}
		forwarded += n
		time.Sleep(20 * time.Millisecond)
	}
	if forwarded != 5 {
		t.Fatalf("the restarted relay forwarded %d, want 5", forwarded)
	}
	if stored() != 5 {
		t.Fatalf("JetStream holds %d messages after the republish: the duplicates were not recognised", stored())
	}

	got, err := writer.Read(ctx, shard, 100, time.Minute)
	if err != nil || len(got) != 5 {
		t.Fatalf("the writer sees %d intents, %v", len(got), err)
	}
	for i, d := range got {
		if d.Intent.Seq != int64(i+1) {
			t.Fatalf("delivery %d has seq %d", i, d.Intent.Seq)
		}
	}
}

func TestTheRelayedOutboxCanBeTrimmedOnceForwarded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rdb := rds.Client()
	t.Cleanup(func() { _ = rdb.Close() })
	shard := domain.Shard(50000 + seq.Add(1))
	io := natsjetstream.IntentsOptions{Clock: system.Clock{}, Prefix: fmt.Sprintf("trm%d", seq.Add(1))}
	src := relaySource(t, rdb)
	r := newRelayed(t, src, io, shard, rdb)
	emitN(t, r, domain.Scope{Realm: "t1", Kind: "workspace", ID: "w"}, 1, 4)
	if n, err := src.Trim(ctx, shard, ports.RetentionPolicy{}); err != nil || n != 0 {
		t.Fatalf("not yet forwarded: removed %d %v", n, err)
	}
	r.pump()
	if n, err := src.Trim(ctx, shard, ports.RetentionPolicy{}); err != nil || n != 4 {
		t.Fatalf("forwarded and acknowledged: removed %d %v", n, err)
	}
}

// A recovery drains a shard to zero before it bumps the generation. An intent written to the Redis
// outbox and not yet relayed is invisible to JetStream, so with a relay attached the outbox's backlog
// counts as undelivered, and a Read that takes over the shard forwards it first.
func TestAnIntentStillInTheOutboxIsCountedAndForwardedByARecoveryRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rdb := rds.Client()
	t.Cleanup(func() { _ = rdb.Close() })
	shard := domain.Shard(60000 + seq.Add(1))
	io := natsjetstream.IntentsOptions{Clock: system.Clock{}, Prefix: fmt.Sprintf("rec%d", seq.Add(1)), AckWait: time.Minute}
	r := newRelayed(t, relaySource(t, rdb), io, shard, rdb)
	writer, err := natsjetstream.NewIntents(ctx, connect(t), io)
	if err != nil {
		t.Fatal(err)
	}
	writer.WithRelay(r.relay)
	emitN(t, r, domain.Scope{Realm: "t1", Kind: "workspace", ID: "w"}, 1, 3) // in Redis, not in JetStream

	st, err := writer.Stats(ctx, shard)
	if err != nil || st.Undelivered != 3 || st.OldestUndelivered < 0 {
		t.Fatalf("a drain must not think the shard is empty while money sits in the outbox: %+v %v", st, err)
	}
	got, err := writer.Read(ctx, shard, 10, 0) // the recovery drain: it owns the shard
	if err != nil || len(got) != 3 {
		t.Fatalf("the recovery read forwards the outbox first: %d %v", len(got), err)
	}
	if st, _ = writer.Stats(ctx, shard); st.Pending != 3 || st.Undelivered != 0 {
		t.Fatalf("%+v", st)
	}
}
