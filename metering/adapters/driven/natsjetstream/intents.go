package natsjetstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// IntentsOptions configure the writer's view of the intents a Relay has forwarded to JetStream.
type IntentsOptions struct {
	Clock ports.Clock
	// Prefix roots the intent subjects: `<prefix>_intents.<shard>`. The underscore keeps them OUT of
	// the bus's `<prefix>.>`: a subscriber to the whole bus must never be handed a money intent.
	Prefix string
	// Stream names the intents stream. Default `<PREFIX>_INTENTS`.
	Stream    string
	Replicas  int           // default 1
	Retention time.Duration // default 7 days
	AckWait   time.Duration // an un-acknowledged intent is redelivered after this; default 30 s
	// FetchWait is how long a Read waits for an intent that has not arrived. Default 100 ms.
	FetchWait time.Duration
}

func (o *IntentsOptions) defaults() {
	if o.Prefix == "" {
		o.Prefix = "billing"
	}
	if o.Stream == "" {
		o.Stream = streamName(o.Prefix) + "_INTENTS"
	}
	if o.Replicas <= 0 {
		o.Replicas = 1
	}
	if o.Retention <= 0 {
		o.Retention = 7 * 24 * time.Hour
	}
	if o.AckWait <= 0 {
		o.AckWait = 30 * time.Second
	}
	if o.FetchWait <= 0 {
		o.FetchWait = 100 * time.Millisecond
	}
}

func (o IntentsOptions) subject(sh domain.Shard) string {
	return fmt.Sprintf("%s_intents.%d", o.Prefix, int(sh))
}

// ensureIntentsStream creates the stream both the Relay and the writer use. Its duplicate window is
// long — an hour — because a relay that crashed between publishing an intent and acknowledging it in
// Redis republishes it, and the message id (shard + Redis entry id) must still recognise it.
func ensureIntentsStream(ctx context.Context, js jetstream.JetStream, o IntentsOptions) (jetstream.Stream, error) {
	st, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name: o.Stream, Subjects: []string{o.Prefix + "_intents.>"},
		Retention: jetstream.LimitsPolicy, Storage: jetstream.FileStorage, Replicas: o.Replicas,
		MaxAge: o.Retention, Discard: jetstream.DiscardOld, Duplicates: time.Hour,
	})
	if err != nil {
		return nil, fmt.Errorf("natsjetstream: intents stream %s: %w", o.Stream, err)
	}
	return st, nil
}

// envelope is what the Relay puts in a JetStream message: the Redis outbox entry, whole.
type envelope struct {
	ID     string            `json:"id"`
	Fields map[string]string `json:"fields"`
}

// Intents implements ports.IntentStream over the intents stream: one durable pull consumer per
// shard, one owner at a time, in order, at least once.
type Intents struct {
	// st is not safe for concurrent use: Info caches into the handle that GetMsg reads. A writer drains
	// shards from several goroutines, so every stream-level call goes through stMu.
	stMu sync.Mutex
	st   jetstream.Stream
	o    IntentsOptions

	relay *Relay

	mu   sync.Mutex
	held map[domain.Shard]map[string]jetstream.Msg // delivered here and not yet acknowledged
}

var _ ports.IntentStream = (*Intents)(nil)

// NewIntents creates the stream if needed and returns the writer's view of it.
func NewIntents(ctx context.Context, nc *nats.Conn, o IntentsOptions) (*Intents, error) {
	if nc == nil || o.Clock == nil {
		return nil, errors.New("natsjetstream: a connection and a Clock are required")
	}
	o.defaults()
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("natsjetstream: jetstream: %w", err)
	}
	st, err := ensureIntentsStream(ctx, js, o)
	if err != nil {
		return nil, err
	}
	return &Intents{st: st, o: o, held: map[domain.Shard]map[string]jetstream.Msg{}}, nil
}

// WithRelay tells the writer's view where its intents come from. A recovery drains a shard to zero
// before it bumps the generation, and an intent still in the Redis outbox — written, not yet relayed —
// is invisible to JetStream: without this a drain could finish with money still in flight. With it,
// Stats counts the outbox's backlog as undelivered, and a Read that takes over the shard (minIdle <= 0)
// relays everything first.
func (s *Intents) WithRelay(r *Relay) *Intents {
	s.relay = r
	return s
}

func consumerFor(sh domain.Shard) string { return "writer-" + strconv.Itoa(int(sh)) }

func (s *Intents) config(sh domain.Shard) jetstream.ConsumerConfig {
	return jetstream.ConsumerConfig{
		Durable: consumerFor(sh), FilterSubject: s.o.subject(sh), AckPolicy: jetstream.AckExplicitPolicy,
		AckWait: s.o.AckWait, MaxDeliver: -1, DeliverPolicy: jetstream.DeliverAllPolicy, MaxAckPending: 100_000,
	}
}

// Read returns up to count intents. A redelivery of one a consumer left un-acknowledged arrives on its
// own once AckWait has passed; with minIdle <= 0 the caller is saying it OWNS the shard and wants
// everything un-acknowledged now — the recovery drain — so the consumer is recreated at its
// acknowledgement floor, and everything after it is delivered again. That is safe by construction:
// every intent is booked idempotently, so a redelivery of one already booked is recognised.
func (s *Intents) Read(ctx context.Context, sh domain.Shard, count int, minIdle time.Duration) ([]ports.Delivery, error) {
	if count < 1 {
		return nil, nil
	}
	if minIdle <= 0 {
		if s.relay != nil {
			if err := s.relay.Drain(ctx, sh); err != nil {
				return nil, err
			}
		}
		if err := s.takeOver(ctx, sh); err != nil {
			return nil, err
		}
	}
	cons, err := s.consumer(ctx, sh)
	if err != nil {
		return nil, err
	}
	batch, err := cons.Fetch(count, jetstream.FetchMaxWait(s.o.FetchWait))
	if err != nil {
		return nil, fmt.Errorf("natsjetstream: fetch: %w", err)
	}
	var out []ports.Delivery
	for msg := range batch.Messages() {
		meta, err := msg.Metadata()
		if err != nil {
			_ = msg.Nak()
			continue
		}
		id := strconv.FormatUint(meta.Sequence.Stream, 10)
		d := ports.Delivery{ID: id, Attempts: int(meta.NumDelivered)} //nolint:gosec // a delivery count
		if d.Attempts > 1 {
			d.Idle = s.o.AckWait
		}
		var env envelope
		if err := json.Unmarshal(msg.Data(), &env); err != nil || env.Fields == nil {
			d.Fields = map[string]string{"raw": string(msg.Data())}
			d.ParseErr = fmt.Errorf("natsjetstream: intent envelope: %w", errors.Join(err, errors.New("not a relayed outbox entry")))
		} else {
			d.Fields = env.Fields
			d.Intent, d.ParseErr = domain.IntentFromFields(env.Fields)
		}
		s.hold(sh, id, msg)
		out = append(out, d)
	}
	if err := batch.Error(); err != nil && !isQuietFetchEnd(err) {
		return out, fmt.Errorf("natsjetstream: fetch: %w", err)
	}
	return out, nil
}

// consumer finds the shard's durable consumer or creates it. It is never "updated": a consumer that a
// takeover recreated at its floor has a different start position, which JetStream will not change.
func (s *Intents) consumer(ctx context.Context, sh domain.Shard) (jetstream.Consumer, error) {
	cons, err := s.st.Consumer(ctx, consumerFor(sh))
	if err == nil {
		return cons, nil
	}
	if !errors.Is(err, jetstream.ErrConsumerNotFound) {
		return nil, fmt.Errorf("natsjetstream: writer consumer for shard %d: %w", sh, err)
	}
	if cons, err = s.st.CreateConsumer(ctx, s.config(sh)); err != nil {
		return nil, fmt.Errorf("natsjetstream: create writer consumer for shard %d: %w", sh, err)
	}
	return cons, nil
}

// isQuietFetchEnd is a fetch that simply found nothing more to wait for.
func isQuietFetchEnd(err error) bool {
	return errors.Is(err, nats.ErrTimeout) || errors.Is(err, jetstream.ErrNoMessages) || errors.Is(err, context.DeadlineExceeded)
}

func (s *Intents) hold(sh domain.Shard, id string, msg jetstream.Msg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held[sh] == nil {
		s.held[sh] = map[string]jetstream.Msg{}
	}
	s.held[sh][id] = msg
}

// takeOver recreates the shard's consumer at its acknowledgement floor.
func (s *Intents) takeOver(ctx context.Context, sh domain.Shard) error {
	cons, err := s.st.Consumer(ctx, consumerFor(sh))
	if errors.Is(err, jetstream.ErrConsumerNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("natsjetstream: consumer: %w", err)
	}
	info, err := cons.Info(ctx)
	if err != nil {
		return fmt.Errorf("natsjetstream: consumer info: %w", err)
	}
	if err := s.st.DeleteConsumer(ctx, consumerFor(sh)); err != nil && !errors.Is(err, jetstream.ErrConsumerNotFound) {
		return fmt.Errorf("natsjetstream: delete consumer: %w", err)
	}
	cfg := s.config(sh)
	cfg.DeliverPolicy, cfg.OptStartSeq = jetstream.DeliverByStartSequencePolicy, info.AckFloor.Stream+1
	if _, err := s.st.CreateConsumer(ctx, cfg); err != nil {
		return fmt.Errorf("natsjetstream: recreate consumer: %w", err)
	}
	s.mu.Lock()
	delete(s.held, sh)
	s.mu.Unlock()
	return nil
}

// Ack acknowledges intents the writer has booked (or parked). An id this instance is not holding — it
// was delivered to a process that has since restarted — is left to be redelivered and recognised.
func (s *Intents) Ack(_ context.Context, sh domain.Shard, ids ...string) error {
	var firstErr error
	for _, id := range ids {
		s.mu.Lock()
		msg := s.held[sh][id]
		delete(s.held[sh], id)
		s.mu.Unlock()
		if msg == nil {
			continue
		}
		if err := msg.Ack(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("natsjetstream: ack %s: %w", id, err)
		}
	}
	return firstErr
}

// Stats describes the shard's intents: how many are stored, how many are delivered and un-acknowledged,
// how many not yet delivered, and the age of the oldest of each.
func (s *Intents) Stats(ctx context.Context, sh domain.Shard) (ports.StreamStats, error) {
	st, err := s.streamStats(ctx, sh)
	if err != nil || s.relay == nil {
		return st, err
	}
	backlog, oldest, err := s.relay.Backlog(ctx, sh)
	if err != nil {
		return st, err
	}
	st.Undelivered += backlog
	st.OldestUndelivered = max(st.OldestUndelivered, oldest)
	return st, nil
}

func (s *Intents) streamStats(ctx context.Context, sh domain.Shard) (ports.StreamStats, error) {
	var st ports.StreamStats
	subj := s.o.subject(sh)
	info, err := s.streamInfo(ctx, subj)
	if err != nil {
		return st, fmt.Errorf("natsjetstream: stream info: %w", err)
	}
	st.Length = int64(info.State.Subjects[subj]) //nolint:gosec // a count
	cons, err := s.st.Consumer(ctx, consumerFor(sh))
	if errors.Is(err, jetstream.ErrConsumerNotFound) {
		st.Undelivered = st.Length
		if st.Length > 0 {
			st.OldestUndelivered = s.ageAt(ctx, info.State.FirstSeq, subj)
		}
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("natsjetstream: consumer: %w", err)
	}
	ci, err := cons.Info(ctx)
	if err != nil {
		return st, fmt.Errorf("natsjetstream: consumer info: %w", err)
	}
	st.Pending, st.Undelivered = int64(ci.NumAckPending), int64(ci.NumPending) //nolint:gosec // counts
	if st.Pending > 0 {
		st.OldestPendingAge = s.ageAt(ctx, ci.AckFloor.Stream+1, subj)
	}
	if st.Undelivered > 0 {
		st.OldestUndelivered = s.ageAt(ctx, ci.Delivered.Stream+1, subj)
	}
	return st, nil
}

func (s *Intents) streamInfo(ctx context.Context, subj string) (*jetstream.StreamInfo, error) {
	s.stMu.Lock()
	defer s.stMu.Unlock()
	return s.st.Info(ctx, jetstream.WithSubjectFilter(subj))
}

func (s *Intents) getMsg(ctx context.Context, seq uint64, subj string) (*jetstream.RawStreamMsg, error) {
	s.stMu.Lock()
	defer s.stMu.Unlock()
	return s.st.GetMsg(ctx, seq, jetstream.WithGetMsgSubject(subj))
}

func (s *Intents) purge(ctx context.Context, subj string, below uint64) error {
	s.stMu.Lock()
	defer s.stMu.Unlock()
	return s.st.Purge(ctx, jetstream.WithPurgeSubject(subj), jetstream.WithPurgeSequence(below))
}

// ageAt is the age of the first stored message for the subject at or after seq; 0 if there is none.
func (s *Intents) ageAt(ctx context.Context, seq uint64, subj string) time.Duration {
	m, err := s.getMsg(ctx, seq, subj)
	if err != nil {
		return 0
	}
	return max(0, s.o.Clock.Now().Sub(m.Time))
}

// Trim removes intents every reader has acknowledged and that are older than the floor, and returns
// how many went. JetStream tracks one consumer per shard, so the acknowledgement floor is the bound:
// nothing at or after the first un-acknowledged intent, and nothing never delivered, is touched. A
// shard nobody has read is not trimmed.
func (s *Intents) Trim(ctx context.Context, sh domain.Shard, keep ports.RetentionPolicy) (int64, error) {
	cons, err := s.st.Consumer(ctx, consumerFor(sh))
	if errors.Is(err, jetstream.ErrConsumerNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("natsjetstream: consumer: %w", err)
	}
	ci, err := cons.Info(ctx)
	if err != nil {
		return 0, fmt.Errorf("natsjetstream: consumer info: %w", err)
	}
	subj := s.o.subject(sh)
	bound := ci.AckFloor.Stream + 1 // purge strictly below this sequence

	cut := s.o.Clock.Now().Add(-keep.MinAge)
	seq, err := s.firstNotOlderThan(ctx, subj, bound, cut)
	if err != nil {
		return 0, err
	}
	bound = min(bound, seq)

	if keep.MaxLen > 0 {
		info, err := s.streamInfo(ctx, subj)
		if err != nil {
			return 0, fmt.Errorf("natsjetstream: stream info: %w", err)
		}
		excess := int64(info.State.Subjects[subj]) - keep.MaxLen //nolint:gosec // a count
		if excess <= 0 {
			return 0, nil
		}
		next := info.State.FirstSeq
		var last uint64
		for i := int64(0); i < excess; i++ {
			m, err := s.getMsg(ctx, next, subj)
			if err != nil {
				break
			}
			last, next = m.Sequence, m.Sequence+1
		}
		if last == 0 {
			return 0, nil
		}
		bound = min(bound, last+1)
	}
	if bound <= 1 {
		return 0, nil
	}
	before, err := s.count(ctx, subj)
	if err != nil {
		return 0, err
	}
	if err := s.purge(ctx, subj, bound); err != nil {
		return 0, fmt.Errorf("natsjetstream: purge: %w", err)
	}
	after, err := s.count(ctx, subj)
	if err != nil {
		return 0, err
	}
	return max(0, before-after), nil
}

func (s *Intents) count(ctx context.Context, subj string) (int64, error) {
	info, err := s.streamInfo(ctx, subj)
	if err != nil {
		return 0, fmt.Errorf("natsjetstream: stream info: %w", err)
	}
	return int64(info.State.Subjects[subj]), nil //nolint:gosec // a count
}

// firstNotOlderThan finds, by bisection over sequence numbers (which are time-ordered), the first
// sequence below limit whose message is not older than cut — limit itself if there is none.
func (s *Intents) firstNotOlderThan(ctx context.Context, subj string, limit uint64, cut time.Time) (uint64, error) {
	lo, hi := uint64(1), limit
	for lo < hi {
		mid := lo + (hi-lo)/2
		m, err := s.getMsg(ctx, mid, subj)
		switch {
		case err != nil && strings.Contains(err.Error(), "no message found"):
			hi = mid // nothing at or after mid for this subject
		case err != nil:
			return 0, fmt.Errorf("natsjetstream: read message %d: %w", mid, err)
		case m.Time.Before(cut):
			lo = m.Sequence + 1
		default:
			hi = mid
		}
	}
	return lo, nil
}
