package redisstreams

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/truongpx396/intel-payment/metering/ports"
)

// BusOptions configure a Bus.
type BusOptions struct {
	Clock ports.Clock
	// Prefix names the streams (`<prefix>:events`, `<prefix>:ticks`) and the DLQ subject
	// (`<prefix>.dlq.<subject>`). Default "billing" — the config's SubjectPrefix.
	Prefix string
	// Consumer names this process within every group it joins; a subscription adds its own suffix.
	Consumer string
	// AckWait is how long a delivered, un-acknowledged entry is left to its consumer before another
	// takes it over (a dead consumer's work is redelivered after this). Default 30 s.
	AckWait time.Duration
	// MaxAttempts is the number of deliveries after which a failing message goes to the DLQ instead of
	// being redelivered. Default 5.
	MaxAttempts int
	// BackoffBase is the redelivery delay after the first failed attempt; it doubles with each, and is
	// never longer than AckWait. Default 1 s.
	BackoffBase time.Duration
	// Block is how long a subscription waits for a new entry before it looks for reclaimable ones.
	// Default 250 ms; it is also the longest a Close waits for an idle subscription.
	Block time.Duration
	// Batch is how many entries a subscription takes at once. Default 64.
	Batch int
	// OnError, if set, sees the errors a subscription's loop absorbs (a Redis outage, a DLQ failure).
	OnError func(error)
}

// Bus implements ports.Bus over Redis Streams: events on `<prefix>:events`, ticks on `<prefix>:ticks`,
// each entry carrying its subject and payload. It never touches an outbox stream — intents are written
// only by the hot functions.
type Bus struct {
	rdb  goredis.UniversalClient
	o    BusOptions
	subN atomic.Int64

	mu     sync.Mutex
	groups map[string]bool
}

var _ ports.Bus = (*Bus)(nil)

// NewBus builds the default bus.
func NewBus(rdb goredis.UniversalClient, o BusOptions) (*Bus, error) {
	if rdb == nil || o.Clock == nil {
		return nil, errors.New("redisstreams: a client and a Clock are required")
	}
	if o.Consumer == "" {
		return nil, errors.New("redisstreams: a consumer name is required")
	}
	if o.Prefix == "" {
		o.Prefix = "billing"
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
	if o.Block <= 0 {
		o.Block = 250 * time.Millisecond
	}
	if o.Batch <= 0 {
		o.Batch = 64
	}
	return &Bus{rdb: rdb, o: o, groups: map[string]bool{}}, nil
}

func validSubject(s string) error {
	if s == "" || strings.ContainsAny(s, " \t\r\n") || strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") || strings.Contains(s, "..") {
		return fmt.Errorf("redisstreams: %q is not a valid subject", s)
	}
	return nil
}

// streamKey maps a subject (or subscription pattern) to its stream: scheduled ticks are their own
// stream, everything else — including a tick's dead letters — is an event.
func (b *Bus) streamKey(subject string) string {
	if strings.HasSuffix(subject, ".tick") && !strings.HasPrefix(subject, b.o.Prefix+".dlq.") {
		return b.o.Prefix + ":ticks"
	}
	return b.o.Prefix + ":events"
}

// Publish appends the message to its stream. Nothing is trimmed or dropped to make room: retention is
// explicit (Trim), and a Redis that refuses a write says so.
func (b *Bus) Publish(ctx context.Context, subject string, payload []byte) error {
	if err := validSubject(subject); err != nil {
		return err
	}
	if strings.ContainsAny(subject, "*>") {
		return fmt.Errorf("redisstreams: cannot publish to the wildcard subject %q", subject)
	}
	err := b.rdb.XAdd(ctx, &goredis.XAddArgs{
		Stream: b.streamKey(subject), Values: map[string]any{"subject": subject, "payload": payload},
	}).Err()
	if err != nil {
		return fmt.Errorf("redisstreams: publish %s: %w", subject, err)
	}
	return nil
}

// ensureGroup creates the group reading from the very beginning: a message published before the
// first subscriber existed must still be delivered.
func (b *Bus) ensureGroup(ctx context.Context, key, group string) error {
	id := key + "|" + group
	b.mu.Lock()
	done := b.groups[id]
	b.mu.Unlock()
	if done {
		return nil
	}
	if err := b.rdb.XGroupCreateMkStream(ctx, key, group, "0").Err(); err != nil && !strings.HasPrefix(err.Error(), "BUSYGROUP") {
		return fmt.Errorf("redisstreams: create group %s on %s: %w", group, key, err)
	}
	b.mu.Lock()
	b.groups[id] = true
	b.mu.Unlock()
	return nil
}

// redisGroup is the consumer group behind a subscription. All events share one stream, so a Redis
// group named only by the caller would hand a message to a consumer of ANOTHER pattern in it, which
// would skip it as not its own. A group is therefore per (name, pattern): consumers that share both
// compete for the work; a different pattern under the same name is a different subscription.
func redisGroup(group, pattern string) string { return group + "|" + pattern }

func (b *Bus) forget(key, group string) {
	b.mu.Lock()
	delete(b.groups, key+"|"+group)
	b.mu.Unlock()
}

// Subscribe joins the group and delivers messages whose subject matches the pattern at least once. A
// handler that returns nil acknowledges; an error (or a panic) leaves the entry for redelivery after a
// backoff, and after MaxAttempts deliveries it goes to `<prefix>.dlq.<subject>` and is acknowledged —
// it is never dropped.
func (b *Bus) Subscribe(ctx context.Context, subject, group string, h ports.Handler) (ports.Subscription, error) {
	if err := validSubject(subject); err != nil {
		return nil, err
	}
	if group == "" || h == nil {
		return nil, errors.New("redisstreams: a group and a handler are required")
	}
	key, rgroup := b.streamKey(subject), redisGroup(group, subject)
	if err := b.ensureGroup(ctx, key, rgroup); err != nil {
		return nil, err
	}
	lctx, cancel := context.WithCancel(ctx) //nolint:gosec // G118: cancel is owned by the subscription — Close and run both call it
	s := &subscription{
		b: b, ctx: lctx, cancel: cancel, key: key, group: rgroup, pattern: subject, h: h,
		consumer: fmt.Sprintf("%s-%d", b.o.Consumer, b.subN.Add(1)), done: make(chan struct{}),
	}
	go s.run()
	return s, nil
}

// Pending reports what the group has not finished with on the subject's stream: entries delivered and
// not acknowledged plus entries not yet delivered, and the age of the oldest of either.
func (b *Bus) Pending(ctx context.Context, subject, group string) (int64, time.Duration, error) {
	st, err := groupStats(ctx, b.rdb, b.streamKey(subject), redisGroup(group, subject), b.o.Clock.Now())
	if err != nil {
		return 0, 0, err
	}
	return st.Pending + st.Undelivered, max(st.OldestPendingAge, st.OldestUndelivered), nil
}

// Trim drops history that every group has acknowledged and that is older than keep allows. It never
// removes an entry a group has not acknowledged, or has not yet been delivered.
func (b *Bus) Trim(ctx context.Context, subject string, keep ports.RetentionPolicy) error {
	_, err := trimAcked(ctx, b.rdb, b.streamKey(subject), keep, b.o.Clock.Now())
	return err
}

// subscription is one consumer of one group.
type subscription struct {
	b        *Bus
	ctx      context.Context
	cancel   context.CancelFunc
	key      string
	group    string
	pattern  string
	consumer string
	h        ports.Handler
	done     chan struct{}
	once     sync.Once
}

// Close stops the subscription and waits for its loop — and the handler in flight — to finish.
func (s *subscription) Close() error {
	s.once.Do(s.cancel)
	<-s.done
	return nil
}

func (s *subscription) run() {
	defer close(s.done)
	defer s.cancel() // the loop ends when its context does; release the context either way
	for s.ctx.Err() == nil {
		if err := s.step(); err != nil && s.ctx.Err() == nil {
			if s.b.o.OnError != nil {
				s.b.o.OnError(err)
			}
			select {
			case <-s.ctx.Done():
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
}

// step takes one batch — first what a dead consumer left past AckWait, then new entries — and handles it.
func (s *subscription) step() error {
	o := s.b.o
	claimed, _, err := s.b.rdb.XAutoClaim(s.ctx, &goredis.XAutoClaimArgs{
		Stream: s.key, Group: s.group, Consumer: s.consumer, MinIdle: o.AckWait, Start: "0-0", Count: int64(o.Batch),
	}).Result()
	if err != nil {
		return s.absorb(err, "xautoclaim")
	}
	msgs := claimed
	if left := o.Batch - len(msgs); left > 0 {
		block := o.Block
		if len(msgs) > 0 {
			block = -1 // there is work in hand: do not wait for more
		}
		res, err := s.b.rdb.XReadGroup(s.ctx, &goredis.XReadGroupArgs{
			Group: s.group, Consumer: s.consumer, Streams: []string{s.key, ">"}, Count: int64(left), Block: block,
		}).Result()
		switch {
		case errors.Is(err, goredis.Nil):
		case err != nil:
			return s.absorb(err, "xreadgroup")
		default:
			for _, st := range res {
				msgs = append(msgs, st.Messages...)
			}
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	attempts := deliveryCounts(s.ctx, s.b.rdb, s.key, s.group, msgs)
	for _, m := range msgs {
		if s.ctx.Err() != nil {
			return nil
		}
		n := max(attempts[m.ID], 1)
		if err := s.handle(m, n); err != nil {
			return err
		}
	}
	return nil
}

func (s *subscription) absorb(err error, op string) error {
	if s.ctx.Err() != nil {
		return nil
	}
	if strings.Contains(err.Error(), "NOGROUP") {
		s.b.forget(s.key, s.group) // lost with a rollback: created again, from the start
		if gerr := s.b.ensureGroup(s.ctx, s.key, s.group); gerr != nil {
			return gerr
		}
		return nil
	}
	return fmt.Errorf("redisstreams: %s: %w", op, err)
}

func (s *subscription) handle(m goredis.XMessage, attempts int) error {
	subject, _ := m.Values["subject"].(string)
	payload, _ := m.Values["payload"].(string)
	if !matches(s.pattern, subject) {
		return s.ack(m.ID) // not this subscription's: another group reads it
	}
	err := s.call(ports.Message{Subject: subject, ID: m.ID, Payload: []byte(payload), Attempts: attempts})
	if err == nil {
		return s.ack(m.ID)
	}
	if attempts >= s.b.o.MaxAttempts {
		if derr := s.deadLetter(subject, m.ID, payload, attempts, err); derr != nil {
			return derr // the entry stays pending; the next pass tries the DLQ again
		}
		return s.ack(m.ID)
	}
	return s.nack(m.ID, attempts)
}

func (s *subscription) call(m ports.Message) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panicked: %v", r)
		}
	}()
	return s.h(s.ctx, m)
}

func (s *subscription) ack(id string) error {
	if err := s.b.rdb.XAck(s.ctx, s.key, s.group, id).Err(); err != nil && s.ctx.Err() == nil {
		return fmt.Errorf("redisstreams: xack: %w", err)
	}
	return nil
}

// nack schedules a redelivery after the backoff by making the entry look as if it has already been
// idle for AckWait minus the delay: the normal reclaim then takes it once the delay has passed.
func (s *subscription) nack(id string, attempts int) error {
	o := s.b.o
	delay := o.BackoffBase << min(attempts-1, 20)
	delay = min(delay, o.AckWait)
	idle := o.AckWait - delay
	err := s.b.rdb.Do(s.ctx, "XCLAIM", s.key, s.group, s.consumer, 0, id, "IDLE", idle.Milliseconds(), "JUSTID").Err()
	if err != nil && s.ctx.Err() == nil {
		return fmt.Errorf("redisstreams: xclaim: %w", err)
	}
	return nil
}

// deadLetter parks a message that has used its attempts on `<prefix>.dlq.<subject>`, whole.
func (s *subscription) deadLetter(subject, id, payload string, attempts int, cause error) error {
	body, err := json.Marshal(struct {
		Subject  string `json:"subject"`
		ID       string `json:"id"`
		Attempts int    `json:"attempts"`
		Error    string `json:"error"`
		Payload  []byte `json:"payload"`
	}{subject, id, attempts, cause.Error(), []byte(payload)})
	if err != nil {
		return err
	}
	return s.b.Publish(s.ctx, s.b.o.Prefix+".dlq."+subject, body)
}
