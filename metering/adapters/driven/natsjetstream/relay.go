package natsjetstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// RelayOptions configure a Relay.
type RelayOptions struct {
	// Intents must match the IntentsOptions the writer uses: same prefix, same stream.
	Intents IntentsOptions
	// Batch is how many outbox entries a pass forwards. Default 256.
	Batch int
	// AckWait is how long an entry a crashed relay had read is left before another relay takes it
	// over. Default 30 s.
	AckWait time.Duration
}

// Relay forwards the Redis outbox to JetStream. Intents are written only by the hot functions, in the
// same atomic call that moves the balance; a Redis Function cannot publish to NATS, so under JetStream
// the intent is durable in the outbox FIRST and this hop follows it. An outbox entry is acknowledged in
// Redis only after JetStream has stored it, and the JetStream message id is the shard and the Redis
// entry id — so a relay that crashes between the two republishes the entry and JetStream recognises it.
// Should the duplicate window ever have passed, booking is idempotent: the writer recognises a
// redelivery, so an intent is never booked twice.
type Relay struct {
	src ports.IntentStream // the Redis outbox, read through a group of the relay's own
	js  jetstream.JetStream
	o   RelayOptions
}

// NewRelay creates the intents stream if needed and returns the relay. src is the Redis outbox as read
// by a consumer group of the relay's own (redisstreams.NewStream with Group "relay"), never the
// writer's.
func NewRelay(ctx context.Context, nc *nats.Conn, src ports.IntentStream, o RelayOptions) (*Relay, error) {
	if nc == nil || src == nil || o.Intents.Clock == nil {
		return nil, errors.New("natsjetstream: a connection, a source and a Clock are required")
	}
	o.Intents.defaults()
	if o.Batch <= 0 {
		o.Batch = 256
	}
	if o.AckWait <= 0 {
		o.AckWait = 30 * time.Second
	}
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("natsjetstream: jetstream: %w", err)
	}
	if _, err := ensureIntentsStream(ctx, js, o.Intents); err != nil {
		return nil, err
	}
	return &Relay{src: src, js: js, o: o}, nil
}

// Pump forwards one batch of the shard's outbox, in order, and returns how many entries it forwarded.
// It stops at the first entry JetStream does not accept, having acknowledged those before it: order
// within a shard is never traded for progress.
func (r *Relay) Pump(ctx context.Context, shard domain.Shard) (int, error) {
	ds, err := r.src.Read(ctx, shard, r.o.Batch, r.o.AckWait)
	if err != nil {
		return 0, err
	}
	subject := r.o.Intents.subject(shard)
	var done []string
	var pubErr error
	for _, d := range ds {
		body, err := json.Marshal(envelope{ID: d.ID, Fields: d.Fields})
		if err != nil {
			pubErr = fmt.Errorf("natsjetstream: relay entry %s: %w", d.ID, err)
			break
		}
		if _, err := r.js.Publish(ctx, subject, body, jetstream.WithMsgID(fmt.Sprintf("%d-%s", int(shard), d.ID))); err != nil {
			pubErr = fmt.Errorf("natsjetstream: relay entry %s: %w", d.ID, err)
			break
		}
		done = append(done, d.ID)
	}
	if err := r.src.Ack(ctx, shard, done...); err != nil {
		return len(done), errors.Join(pubErr, err)
	}
	return len(done), pubErr
}

// Drain forwards the shard's outbox until nothing is left in it.
func (r *Relay) Drain(ctx context.Context, shard domain.Shard) error {
	for {
		n, err := r.Pump(ctx, shard)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
}

// Backlog is what the outbox holds that JetStream does not yet: entries read and not acknowledged, and
// entries not yet read, with the age of the older of the two.
func (r *Relay) Backlog(ctx context.Context, shard domain.Shard) (int64, time.Duration, error) {
	st, err := r.src.Stats(ctx, shard)
	if err != nil {
		return 0, 0, err
	}
	return st.Pending + st.Undelivered, max(st.OldestPendingAge, st.OldestUndelivered), nil
}
