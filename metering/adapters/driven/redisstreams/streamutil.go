package redisstreams

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/truongpx396/intel-payment/metering/ports"
)

// groupStats describes one consumer group's view of one stream: its length, what it has been
// delivered and not acknowledged, and what it has not yet been delivered — each with the age of the
// oldest entry. A stream or group that does not exist yet is simply empty.
func groupStats(ctx context.Context, rdb goredis.UniversalClient, key, group string, now time.Time) (ports.StreamStats, error) {
	var st ports.StreamStats
	n, err := rdb.XLen(ctx, key).Result()
	if err != nil {
		return st, fmt.Errorf("redisstreams: xlen: %w", err)
	}
	st.Length = n

	pend, err := rdb.XPending(ctx, key, group).Result()
	if err != nil {
		if isNoGroup(err) {
			return st, nil
		}
		return st, fmt.Errorf("redisstreams: xpending: %w", err)
	}
	st.Pending = pend.Count
	if pend.Count > 0 {
		st.OldestPendingAge = age(now, pend.Lower)
	}

	groups, err := rdb.XInfoGroups(ctx, key).Result()
	if err != nil {
		return st, fmt.Errorf("redisstreams: xinfo groups: %w", err)
	}
	for _, g := range groups {
		if g.Name != group {
			continue
		}
		st.Undelivered = g.Lag
		if g.Lag > 0 {
			first, err := rdb.XRangeN(ctx, key, "("+g.LastDeliveredID, "+", 1).Result()
			if err == nil && len(first) == 1 {
				st.OldestUndelivered = age(now, first[0].ID)
			}
		}
	}
	return st, nil
}

func isNoGroup(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "NOGROUP") || strings.Contains(err.Error(), "no such key"))
}

// streamID is a Redis stream entry id, `<ms>-<seq>`.
type streamID struct{ ms, seq uint64 }

func parseID(s string) (streamID, error) {
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return streamID{}, fmt.Errorf("redisstreams: bad stream id %q", s)
	}
	ms, err := strconv.ParseUint(a, 10, 64)
	if err != nil {
		return streamID{}, fmt.Errorf("redisstreams: bad stream id %q", s)
	}
	seq, err := strconv.ParseUint(b, 10, 64)
	if err != nil {
		return streamID{}, fmt.Errorf("redisstreams: bad stream id %q", s)
	}
	return streamID{ms, seq}, nil
}

func (i streamID) String() string { return fmt.Sprintf("%d-%d", i.ms, i.seq) }

func (i streamID) less(o streamID) bool { return i.ms < o.ms || (i.ms == o.ms && i.seq < o.seq) }

// next is the smallest id greater than i.
func (i streamID) next() streamID {
	if i.seq == ^uint64(0) {
		return streamID{i.ms + 1, 0}
	}
	return streamID{i.ms, i.seq + 1}
}

func minID(a, b streamID) streamID {
	if b.less(a) {
		return b
	}
	return a
}

// age is how long ago a stream entry id (`<ms>-<seq>`) was created.
func age(now time.Time, id string) time.Duration {
	i, err := parseID(id)
	if err != nil {
		return 0
	}
	d := now.Sub(time.UnixMilli(int64(i.ms))) //nolint:gosec // a millisecond timestamp
	if d < 0 {
		return 0
	}
	return d
}

// trimAcked removes history that is BOTH acknowledged by every consumer group AND past the retention
// floor, and reports how many entries went.
//
// An entry is safe to remove only when every group has been delivered it and has acknowledged it: it
// lies below the group's last-delivered id and below its oldest pending entry. With no group at all
// nothing has been read by anyone, so nothing is removable — a stream is never trimmed out from under
// a consumer that has not started yet. MinAge is always kept; MaxLen, when set, is the length to trim
// toward once past it, so it never removes more than needed.
func trimAcked(ctx context.Context, rdb goredis.UniversalClient, key string, keep ports.RetentionPolicy, now time.Time) (int64, error) {
	groups, err := rdb.XInfoGroups(ctx, key).Result()
	if err != nil {
		if isNoGroup(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("redisstreams: xinfo groups: %w", err)
	}
	if len(groups) == 0 {
		return 0, nil
	}
	bound := streamID{ms: ^uint64(0), seq: ^uint64(0)}
	for _, g := range groups {
		delivered, err := parseID(g.LastDeliveredID)
		if err != nil {
			return 0, err
		}
		b := delivered.next()
		if g.Pending > 0 {
			pend, err := rdb.XPending(ctx, key, g.Name).Result()
			if err != nil {
				return 0, fmt.Errorf("redisstreams: xpending: %w", err)
			}
			lowest, err := parseID(pend.Lower)
			if err != nil {
				return 0, err
			}
			b = minID(b, lowest)
		}
		bound = minID(bound, b)
	}

	cut := minID(bound, streamID{ms: uint64(max(int64(0), now.Add(-keep.MinAge).UnixMilli()))}) //nolint:gosec // non-negative by construction
	n, err := rdb.XLen(ctx, key).Result()
	if err != nil {
		return 0, fmt.Errorf("redisstreams: xlen: %w", err)
	}
	if keep.MaxLen > 0 {
		excess := n - keep.MaxLen
		if excess <= 0 {
			return 0, nil
		}
		last, err := nthID(ctx, rdb, key, excess)
		if err != nil {
			return 0, err
		}
		cut = minID(cut, last.next())
	}
	if cut == (streamID{}) {
		return 0, nil
	}
	if err := rdb.XTrimMinID(ctx, key, cut.String()).Err(); err != nil {
		return 0, fmt.Errorf("redisstreams: xtrim: %w", err)
	}
	after, err := rdb.XLen(ctx, key).Result()
	if err != nil {
		return 0, fmt.Errorf("redisstreams: xlen: %w", err)
	}
	return max(int64(0), n-after), nil
}

// nthID is the id of the n-th oldest entry (1-based), read a page at a time.
func nthID(ctx context.Context, rdb goredis.UniversalClient, key string, n int64) (streamID, error) {
	const page = 1000
	start := "-"
	var seen int64
	for {
		ms, err := rdb.XRangeN(ctx, key, start, "+", page).Result()
		if err != nil {
			return streamID{}, fmt.Errorf("redisstreams: xrange: %w", err)
		}
		if len(ms) == 0 {
			return streamID{}, fmt.Errorf("redisstreams: stream %s has fewer than %d entries", key, n)
		}
		if seen+int64(len(ms)) >= n {
			return parseID(ms[n-seen-1].ID)
		}
		seen += int64(len(ms))
		last, err := parseID(ms[len(ms)-1].ID)
		if err != nil {
			return streamID{}, err
		}
		start = last.next().String()
	}
}

// deliveryCounts reads how many times each entry has been delivered to the group (XPENDING's retry
// count), so an entry is escalated after MaxAttempts even across a consumer restart. A failure here
// only makes the count read as one.
func deliveryCounts(ctx context.Context, rdb goredis.UniversalClient, key, group string, msgs []goredis.XMessage) map[string]int {
	out := make(map[string]int, len(msgs))
	if len(msgs) == 0 {
		return out
	}
	pend, err := rdb.XPendingExt(ctx, &goredis.XPendingExtArgs{
		Stream: key, Group: group, Start: msgs[0].ID, End: msgs[len(msgs)-1].ID, Count: int64(len(msgs)) * 4,
	}).Result()
	if err != nil {
		return out
	}
	for _, p := range pend {
		out[p.ID] = int(p.RetryCount)
	}
	return out
}
