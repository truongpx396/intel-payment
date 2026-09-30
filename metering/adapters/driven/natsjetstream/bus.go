// Package natsjetstream is the optional bus adapter: NATS JetStream, chosen for quorum-replicated or
// cross-region traffic or to consolidate on an existing JetStream estate (bus-subjects.md).
//
// Be precise about what it buys. Money intents are STILL written by the hot functions into the Redis
// outbox — a Redis Function cannot publish to NATS — and a Relay forwards them here once they are
// durable there. JetStream therefore improves what happens after the outbox; it does not change the
// hot path's own loss window. Switching is a wiring change: no subject, payload, handler or invariant
// differs, and the same BusContract holds both adapters to it.
package natsjetstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/truongpx396/intel-payment/metering/ports"
)

// BusOptions configure a Bus.
type BusOptions struct {
	Clock ports.Clock
	// Prefix is the subject root: every event is `<prefix>.…`. Default "billing".
	Prefix string
	// Stream names the JetStream stream that holds them. Default: the prefix upper-cased.
	Stream string
	// Replicas is the stream's replication (R3 or R5 in production). Default 1.
	Replicas int
	// Retention is how long history is kept (BusRetention). Un-acknowledged messages older than this
	// are lost to the stream's own limits, so it must exceed the longest outage a consumer is allowed;
	// alarm on Pending's age long before it. Default 7 days.
	Retention time.Duration
	// AckWait is how long a delivered, un-acknowledged message is left to its consumer before another
	// takes it over. Default 30 s.
	AckWait time.Duration
	// MaxAttempts is the number of deliveries after which a failing message goes to the DLQ. Default 5.
	MaxAttempts int
	// BackoffBase is the redelivery delay after the first failure; it doubles, never beyond AckWait. Default 1 s.
	BackoffBase time.Duration
	// Batch is how many messages a subscription buffers. Default 64.
	Batch int
	// OnError, if set, sees errors a subscription absorbs.
	OnError func(error)
}

func (o *BusOptions) defaults() {
	if o.Prefix == "" {
		o.Prefix = "billing"
	}
	if o.Stream == "" {
		o.Stream = streamName(o.Prefix)
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
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 5
	}
	if o.BackoffBase <= 0 {
		o.BackoffBase = time.Second
	}
	if o.Batch <= 0 {
		o.Batch = 64
	}
}

// streamName makes a valid stream name (no dots, wildcards, separators or whitespace) from a prefix.
func streamName(prefix string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		}
		return '_'
	}, prefix)
}

// Bus implements ports.Bus over one JetStream stream holding `<prefix>.>`.
type Bus struct {
	js jetstream.JetStream
	st jetstream.Stream
	o  BusOptions
}

var _ ports.Bus = (*Bus)(nil)

// NewBus creates (or updates) the stream and returns the bus.
func NewBus(ctx context.Context, nc *nats.Conn, o BusOptions) (*Bus, error) {
	if nc == nil || o.Clock == nil {
		return nil, errors.New("natsjetstream: a connection and a Clock are required")
	}
	o.defaults()
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("natsjetstream: jetstream: %w", err)
	}
	st, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name: o.Stream, Subjects: []string{o.Prefix + ".>"},
		Retention: jetstream.LimitsPolicy, Storage: jetstream.FileStorage, Replicas: o.Replicas,
		MaxAge: o.Retention, Discard: jetstream.DiscardOld,
	})
	if err != nil {
		return nil, fmt.Errorf("natsjetstream: stream %s: %w", o.Stream, err)
	}
	return &Bus{js: js, st: st, o: o}, nil
}

func validSubject(s string) error {
	if s == "" || strings.ContainsAny(s, " \t\r\n") || strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") || strings.Contains(s, "..") {
		return fmt.Errorf("natsjetstream: %q is not a valid subject", s)
	}
	return nil
}

// Publish appends the message to the stream and returns once the stream has stored it.
func (b *Bus) Publish(ctx context.Context, subject string, payload []byte) error {
	if err := validSubject(subject); err != nil {
		return err
	}
	if strings.ContainsAny(subject, "*>") {
		return fmt.Errorf("natsjetstream: cannot publish to the wildcard subject %q", subject)
	}
	if !strings.HasPrefix(subject, b.o.Prefix+".") {
		return fmt.Errorf("natsjetstream: subject %q is outside the stream's %s.>", subject, b.o.Prefix)
	}
	if _, err := b.js.Publish(ctx, subject, payload); err != nil {
		return fmt.Errorf("natsjetstream: publish %s: %w", subject, err)
	}
	return nil
}

// ConsumerName is the durable consumer behind a subscription: per (group, pattern), so consumers that
// share both compete for the work and a different pattern under the same name is another subscription.
// It is exported so an operator can find the consumer to inspect.
func ConsumerName(group, pattern string) string {
	safe := strings.Map(func(r rune) rune {
		if r == '.' || r == '*' || r == '>' || r == '/' || r == '\\' || r <= ' ' || r == 0x7f {
			return '_'
		}
		return r
	}, group)
	if len(safe) > 32 {
		safe = safe[:32]
	}
	sum := sha256.Sum256([]byte(group + "\x00" + pattern))
	return safe + "-" + hex.EncodeToString(sum[:8])
}

// Subscribe delivers messages matching the pattern to the handler at least once. A handler that
// returns nil acknowledges; an error (or a panic) redelivers after a backoff, and once MaxAttempts
// deliveries are used the message goes to `<prefix>.dlq.<subject>` — it is never dropped.
func (b *Bus) Subscribe(ctx context.Context, subject, group string, h ports.Handler) (ports.Subscription, error) {
	if err := validSubject(subject); err != nil {
		return nil, err
	}
	if group == "" || h == nil {
		return nil, errors.New("natsjetstream: a group and a handler are required")
	}
	cons, err := b.st.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable: ConsumerName(group, subject), FilterSubject: subject, AckPolicy: jetstream.AckExplicitPolicy,
		AckWait: b.o.AckWait, MaxDeliver: -1, DeliverPolicy: jetstream.DeliverAllPolicy, MaxAckPending: b.o.Batch * 16,
	})
	if err != nil {
		return nil, fmt.Errorf("natsjetstream: consumer for %s: %w", subject, err)
	}
	s := &subscription{b: b, h: h, ctx: ctx}
	s.cc, err = cons.Consume(s.deliver, jetstream.PullMaxMessages(b.o.Batch), jetstream.ConsumeErrHandler(func(_ jetstream.ConsumeContext, err error) {
		if b.o.OnError != nil {
			b.o.OnError(err)
		}
	}))
	if err != nil {
		return nil, fmt.Errorf("natsjetstream: consume %s: %w", subject, err)
	}
	return s, nil
}

type subscription struct {
	b    *Bus
	h    ports.Handler
	ctx  context.Context
	cc   jetstream.ConsumeContext
	wg   sync.WaitGroup
	once sync.Once
}

// Close stops delivery and waits for the handler in flight.
func (s *subscription) Close() error {
	s.once.Do(func() {
		s.cc.Stop()
		<-s.cc.Closed()
		s.wg.Wait()
	})
	return nil
}

func (s *subscription) deliver(msg jetstream.Msg) {
	s.wg.Add(1)
	defer s.wg.Done()
	o := s.b.o
	meta, err := msg.Metadata()
	if err != nil {
		_ = msg.Nak()
		return
	}
	attempts := int(meta.NumDelivered) //nolint:gosec // a delivery count
	// A message already delivered more often than it may be — its consumers kept dying on it — goes to
	// the DLQ without another attempt.
	if attempts > o.MaxAttempts {
		s.deadLetter(msg, meta, attempts, errors.New("delivered more often than MaxAttempts without an outcome"))
		return
	}
	err = s.call(ports.Message{Subject: msg.Subject(), ID: fmt.Sprint(meta.Sequence.Stream), Payload: msg.Data(), Attempts: attempts})
	switch {
	case err == nil:
		_ = msg.Ack()
	case attempts >= o.MaxAttempts:
		s.deadLetter(msg, meta, attempts, err)
	default:
		_ = msg.NakWithDelay(min(o.BackoffBase<<min(attempts-1, 20), o.AckWait))
	}
}

func (s *subscription) call(m ports.Message) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panicked: %v", r)
		}
	}()
	return s.h(s.ctx, m)
}

// deadLetter parks a message that has used its attempts on `<prefix>.dlq.<subject>`, whole, and only
// then acknowledges it. If the DLQ cannot be written the message stays for another pass.
func (s *subscription) deadLetter(msg jetstream.Msg, meta *jetstream.MsgMetadata, attempts int, cause error) {
	body, err := json.Marshal(struct {
		Subject  string `json:"subject"`
		ID       string `json:"id"`
		Attempts int    `json:"attempts"`
		Error    string `json:"error"`
		Payload  []byte `json:"payload"`
	}{msg.Subject(), fmt.Sprint(meta.Sequence.Stream), attempts, cause.Error(), msg.Data()})
	if err == nil {
		err = s.b.Publish(s.ctx, s.b.o.Prefix+".dlq."+msg.Subject(), body)
	}
	if err != nil {
		if s.b.o.OnError != nil {
			s.b.o.OnError(fmt.Errorf("dead-letter %s: %w", msg.Subject(), err))
		}
		_ = msg.NakWithDelay(s.b.o.BackoffBase)
		return
	}
	_ = msg.Ack()
}

// Pending reports what the subscription's group has not finished with — delivered and un-acknowledged
// plus not yet delivered — and the age of the oldest of either.
func (b *Bus) Pending(ctx context.Context, subject, group string) (int64, time.Duration, error) {
	cons, err := b.st.Consumer(ctx, ConsumerName(group, subject))
	if err != nil {
		if errors.Is(err, jetstream.ErrConsumerNotFound) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("natsjetstream: consumer info: %w", err)
	}
	info, err := cons.Info(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("natsjetstream: consumer info: %w", err)
	}
	n := int64(info.NumAckPending) + int64(info.NumPending) //nolint:gosec // counts
	if n == 0 {
		return 0, 0, nil
	}
	oldest, err := b.st.GetMsg(ctx, info.AckFloor.Stream+1, jetstream.WithGetMsgSubject(subject))
	if err != nil {
		return n, 0, nil //nolint:nilerr // the count is the answer; the age is best effort
	}
	return n, max(0, b.o.Clock.Now().Sub(oldest.Time)), nil
}

// Trim is a no-op: the stream's own limits (MaxAge) bound history, and JetStream keeps an entry until
// they do — an un-acknowledged message is never removed by a consumer's progress.
func (b *Bus) Trim(context.Context, string, ports.RetentionPolicy) error { return nil }
