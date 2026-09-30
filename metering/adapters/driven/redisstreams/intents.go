package redisstreams

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"

	hotredis "github.com/truongpx396/intel-payment/metering/adapters/driven/redis"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// DefaultGroup is the consumer group every shard's writer reads through.
const DefaultGroup = "drain"

// IntentOptions configure a Stream.
type IntentOptions struct {
	// Consumer names this writer within the group; it must be unique per worker process.
	Consumer string
	// Group defaults to DefaultGroup.
	Group string
	Clock ports.Clock
}

// Stream implements ports.IntentStream.
type Stream struct {
	rdb      goredis.UniversalClient
	consumer string
	group    string
	clock    ports.Clock

	mu     sync.Mutex
	groups map[domain.Shard]bool
}

var _ ports.IntentStream = (*Stream)(nil)

// NewStream builds the writer's view of the outbox streams.
func NewStream(rdb goredis.UniversalClient, o IntentOptions) (*Stream, error) {
	if rdb == nil || o.Clock == nil {
		return nil, errors.New("redisstreams: a client and a Clock are required")
	}
	if o.Consumer == "" {
		return nil, errors.New("redisstreams: a consumer name is required")
	}
	if o.Group == "" {
		o.Group = DefaultGroup
	}
	return &Stream{rdb: rdb, consumer: o.Consumer, group: o.Group, clock: o.Clock, groups: map[domain.Shard]bool{}}, nil
}

// ensureGroup creates the shard's consumer group (and the stream) reading from the very beginning:
// an intent written before the group existed must still be booked.
func (s *Stream) ensureGroup(ctx context.Context, sh domain.Shard) error {
	s.mu.Lock()
	done := s.groups[sh]
	s.mu.Unlock()
	if done {
		return nil
	}
	err := s.rdb.XGroupCreateMkStream(ctx, hotredis.OutboxKey(sh), s.group, "0").Err()
	if err != nil && !strings.HasPrefix(err.Error(), "BUSYGROUP") {
		return fmt.Errorf("redisstreams: create group: %w", err)
	}
	s.mu.Lock()
	s.groups[sh] = true
	s.mu.Unlock()
	return nil
}

func (s *Stream) forget(sh domain.Shard) {
	s.mu.Lock()
	delete(s.groups, sh)
	s.mu.Unlock()
}

// Read returns up to count deliveries: first entries a dead consumer left un-acked past minIdle,
// oldest first (so order survives a takeover), then new ones.
func (s *Stream) Read(ctx context.Context, sh domain.Shard, count int, minIdle time.Duration) ([]ports.Delivery, error) {
	ds, err := s.read(ctx, sh, count, minIdle)
	if errors.Is(err, errNoGroup) {
		// The group is gone — the stream was rebuilt from an older state (a rollback, a failover to a
		// replica that had not yet seen it created). Create it again, from the start, and read once more.
		s.forget(sh)
		ds, err = s.read(ctx, sh, count, minIdle)
	}
	return ds, err
}

// errNoGroup marks a read that found the consumer group missing.
var errNoGroup = errors.New("redisstreams: consumer group missing")

func (s *Stream) read(ctx context.Context, sh domain.Shard, count int, minIdle time.Duration) ([]ports.Delivery, error) {
	if count < 1 {
		return nil, nil
	}
	if err := s.ensureGroup(ctx, sh); err != nil {
		return nil, err
	}
	key := hotredis.OutboxKey(sh)

	var msgs []goredis.XMessage
	claimed := map[string]bool{}
	start := "0-0"
	for len(msgs) < count {
		got, next, err := s.rdb.XAutoClaim(ctx, &goredis.XAutoClaimArgs{
			Stream: key, Group: s.group, Consumer: s.consumer, MinIdle: minIdle, Start: start, Count: int64(count - len(msgs)),
		}).Result()
		if err != nil {
			if strings.HasPrefix(err.Error(), "NOGROUP") {
				return nil, errNoGroup
			}
			return nil, fmt.Errorf("redisstreams: xautoclaim: %w", err)
		}
		for _, m := range got {
			claimed[m.ID] = true
		}
		msgs = append(msgs, got...)
		if next == "0-0" || next == "" {
			break
		}
		start = next
	}
	if left := count - len(msgs); left > 0 {
		res, err := s.rdb.XReadGroup(ctx, &goredis.XReadGroupArgs{
			Group: s.group, Consumer: s.consumer, Streams: []string{key, ">"}, Count: int64(left), Block: -1,
		}).Result()
		switch {
		case errors.Is(err, goredis.Nil):
		case err != nil:
			if strings.HasPrefix(err.Error(), "NOGROUP") {
				return nil, errNoGroup
			}
			return nil, fmt.Errorf("redisstreams: xreadgroup: %w", err)
		default:
			for _, st := range res {
				msgs = append(msgs, st.Messages...)
			}
		}
	}
	if len(msgs) == 0 {
		return nil, nil
	}

	attempts := deliveryCounts(ctx, s.rdb, key, s.group, msgs)
	out := make([]ports.Delivery, 0, len(msgs))
	for _, m := range msgs {
		d := ports.Delivery{ID: m.ID, Fields: fieldsOf(m), Attempts: attempts[m.ID]}
		if d.Attempts == 0 {
			d.Attempts = 1
		}
		if claimed[m.ID] {
			d.Idle = minIdle
		}
		d.Intent, d.ParseErr = domain.IntentFromFields(d.Fields)
		out = append(out, d)
	}
	return out, nil
}

func fieldsOf(m goredis.XMessage) map[string]string {
	f := make(map[string]string, len(m.Values))
	for k, v := range m.Values {
		f[k] = fmt.Sprint(v)
	}
	return f
}

// Ack acknowledges entries the writer has booked (or parked). It never deletes them: retention is
// explicit, and an entry leaves the stream only through Trim, once acked AND old enough.
func (s *Stream) Ack(ctx context.Context, sh domain.Shard, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	if err := s.rdb.XAck(ctx, hotredis.OutboxKey(sh), s.group, ids...).Err(); err != nil {
		return fmt.Errorf("redisstreams: xack: %w", err)
	}
	return nil
}

// Stats describes the shard's outbox: its length, what is un-acked, and the age of the oldest
// entry not yet booked — the figure behind metering_outbox_age_seconds.
func (s *Stream) Stats(ctx context.Context, sh domain.Shard) (ports.StreamStats, error) {
	st, err := s.stats(ctx, sh)
	if err != nil && strings.Contains(err.Error(), "NOGROUP") {
		s.forget(sh) // see Read: the group was lost with a rollback
		return s.stats(ctx, sh)
	}
	return st, err
}

func (s *Stream) stats(ctx context.Context, sh domain.Shard) (ports.StreamStats, error) {
	if err := s.ensureGroup(ctx, sh); err != nil {
		return ports.StreamStats{}, err
	}
	return groupStats(ctx, s.rdb, hotredis.OutboxKey(sh), s.group, s.clock.Now())
}

// Trim drops acknowledged history older than keep allows. The outbox is never trimmed by the hot
// functions (a full stream is BACKPRESSURE, not a reason to drop), so this is the only thing that
// bounds it.
func (s *Stream) Trim(ctx context.Context, sh domain.Shard, keep ports.RetentionPolicy) (int64, error) {
	return trimAcked(ctx, s.rdb, hotredis.OutboxKey(sh), keep, s.clock.Now())
}
