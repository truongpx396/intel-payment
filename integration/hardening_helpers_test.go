//go:build integration && hardening

package integration

// Phase 7 — Hardening. These are measurements, not features (tasks.md, T130–T135): each one runs the real stores and
// the real binaries and ends in a verdict against a number the design or a success criterion states. They sit behind
// the `hardening` build tag because they take minutes and are sensitive to what else is running on the machine, so
// `make test-integration` does not run them; `make hardening` does.
//
// They are deliberately NOT t.Parallel(): two measurements sharing a CPU measure each other. A test that is not
// parallel runs before any parallel one resumes, and `make hardening` selects only these, so nothing else competes.

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// loadResult is what one paced or saturating run produced.
type loadResult struct {
	ops, errs int64
	elapsed   time.Duration
	lat       []int64 // nanoseconds, one per operation, sorted
}

func (r loadResult) rate() float64 { return float64(r.ops) / r.elapsed.Seconds() }

// pct is the p-th percentile (0 < p <= 1) by the nearest-rank method, which never interpolates a latency nobody saw.
func (r loadResult) pct(p float64) time.Duration {
	if len(r.lat) == 0 {
		return 0
	}
	i := int(math.Ceil(p*float64(len(r.lat)))) - 1
	return time.Duration(r.lat[max(0, min(i, len(r.lat)-1))])
}

func (r loadResult) String() string {
	return fmt.Sprintf("ops=%d errs=%d rate=%.0f/s p50=%v p99=%v p99.9=%v max=%v",
		r.ops, r.errs, r.rate(), r.pct(0.50), r.pct(0.99), r.pct(0.999), r.pct(1))
}

// runLoad drives fn from `workers` goroutines for dur.
//
// rate > 0 is OPEN-LOOP: operations fall due on a fixed schedule, and each one's latency is measured from the
// instant it was DUE, not from when a worker got round to sending it. A stall therefore charges every operation that
// queued behind it, which is what a real caller waiting on the gate experiences. (Measuring from the send instant
// instead lets a slow server quietly slow the load generator down and report a flattering p99: coordinated omission.)
//
// rate == 0 is CLOSED-LOOP: every worker sends the next as soon as the last returns. That finds the ceiling; its
// latencies are queueing time and say nothing about the latency of an unloaded call.
func runLoad(workers int, rate float64, dur time.Duration, fn func(worker int, n int64) error) loadResult {
	var (
		wg    sync.WaitGroup
		errs  atomic.Int64
		start = time.Now()
		end   = start.Add(dur)
		per   = make([][]int64, workers)
	)
	var interval time.Duration
	if rate > 0 {
		interval = time.Duration(float64(workers) / rate * float64(time.Second))
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var n int64
			next := start.Add(time.Duration(w) * interval / time.Duration(workers)) // stagger the workers across one interval
			for {
				due := time.Now()
				if rate > 0 {
					due = next
					next = next.Add(interval)
					if !due.Before(end) {
						return
					}
					if d := time.Until(due); d > 0 {
						time.Sleep(d)
					}
				} else if !due.Before(end) {
					return
				}
				if err := fn(w, n); err != nil {
					errs.Add(1)
				}
				per[w] = append(per[w], int64(time.Since(due)))
				n++
			}
		}()
	}
	wg.Wait()
	r := loadResult{errs: errs.Load(), elapsed: time.Since(start)}
	for _, l := range per {
		r.lat = append(r.lat, l...)
	}
	r.ops = int64(len(r.lat))
	sort.Slice(r.lat, func(i, j int) bool { return r.lat[i] < r.lat[j] })
	return r
}

// serverLatency reads Redis's own per-command latency histograms for cmds and merges them — the time the PRIMARY
// spent executing them, which is what "p99 at the primary" (the scale envelope) means and which a client behind
// Docker Desktop's network cannot see. Redis counts FCALL and FCALL_RO as different commands, so a caller that mixes
// them must ask for both. Buckets are powers of two microseconds, so a percentile is an upper bound within a factor
// of two. It FAILS the test when the server saw no call at all: a percentile over no data would pass any threshold.
func serverLatency(ctx context.Context, tb testing.TB, rdb goredis.UniversalClient, cmds ...string) (calls int64, p50, p99, p999 time.Duration) {
	tb.Helper()
	res, err := rdb.Do(ctx, append([]any{"LATENCY", "HISTOGRAM"}, anys(cmds)...)...).Result()
	if err != nil {
		tb.Fatalf("LATENCY HISTOGRAM %v: %v", cmds, err)
	}
	top, ok := res.(map[any]any)
	if !ok {
		tb.Fatalf("LATENCY HISTOGRAM %v: unexpected reply %T %v", cmds, res, res)
	}
	perBucket := map[int64]int64{} // upper bound in µs -> calls that landed in exactly that bucket
	for _, cmd := range cmds {
		entry, ok := top[cmd].(map[any]any)
		if !ok {
			continue // this command was never called
		}
		n, _ := entry["calls"].(int64)
		calls += n
		hist, _ := entry["histogram_usec"].(map[any]any)
		type bucket struct{ upper, cum int64 }
		var bs []bucket
		for k, v := range hist {
			u, _ := k.(int64)
			c, _ := v.(int64)
			bs = append(bs, bucket{u, c})
		}
		sort.Slice(bs, func(i, j int) bool { return bs[i].upper < bs[j].upper })
		var prev int64
		for _, b := range bs { // the reply is cumulative; a merge needs what each bucket holds on its own
			perBucket[b.upper] += b.cum - prev
			prev = b.cum
		}
	}
	if calls == 0 {
		tb.Fatalf("the server's latency histogram for %v is empty: nothing was measured, so no verdict may rest on it", cmds)
	}
	var uppers []int64
	for u := range perBucket {
		uppers = append(uppers, u)
	}
	sort.Slice(uppers, func(i, j int) bool { return uppers[i] < uppers[j] })
	at := func(p float64) time.Duration {
		want, cum := int64(math.Ceil(p*float64(calls))), int64(0)
		for _, u := range uppers {
			if cum += perBucket[u]; cum >= want {
				return time.Duration(u) * time.Microsecond
			}
		}
		return 0
	}
	return calls, at(0.50), at(0.99), at(0.999)
}

func anys(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// redisCPUSeconds is the CPU the Redis server process has used (user + system), whole-process. Redis executes
// commands on one thread, so the growth of this number over a run's wall time is that thread's utilisation, give or
// take background threads: near 1.0 means the primary was the limit; well under it means something else was.
func redisCPUSeconds(ctx context.Context, tb testing.TB, rdb goredis.UniversalClient) float64 {
	tb.Helper()
	info, err := rdb.Info(ctx, "cpu").Result()
	if err != nil {
		tb.Fatalf("INFO cpu: %v", err)
	}
	var total float64
	for _, line := range strings.Split(info, "\r\n") {
		for _, k := range []string{"used_cpu_sys:", "used_cpu_user:"} {
			if v, ok := strings.CutPrefix(line, k); ok {
				f, err := strconv.ParseFloat(v, 64)
				if err != nil {
					tb.Fatalf("INFO cpu %q: %v", line, err)
				}
				total += f
			}
		}
	}
	return total
}

// selfCPUSeconds is the CPU this test process (the load generator and the embedded Meter) has used.
func selfCPUSeconds() float64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return float64(ru.Utime.Sec) + float64(ru.Utime.Usec)/1e6 + float64(ru.Stime.Sec) + float64(ru.Stime.Usec)/1e6
}

// usecPerCall is the server's average execution time per call of cmd since the stats were last reset.
func usecPerCall(ctx context.Context, tb testing.TB, rdb goredis.UniversalClient, cmd string) float64 {
	tb.Helper()
	info, err := rdb.Info(ctx, "commandstats").Result()
	if err != nil {
		tb.Fatalf("INFO commandstats: %v", err)
	}
	for _, line := range strings.Split(info, "\r\n") {
		if rest, ok := strings.CutPrefix(line, "cmdstat_"+cmd+":"); ok {
			for _, f := range strings.Split(rest, ",") {
				if v, ok := strings.CutPrefix(f, "usec_per_call="); ok {
					x, _ := strconv.ParseFloat(v, 64)
					return x
				}
			}
		}
	}
	return 0
}

// span is a window of a run: wall time, and the CPU the generator and the primary each burned in it.
type span struct {
	ctx        context.Context
	tb         testing.TB
	rdb        goredis.UniversalClient
	t0         time.Time
	self0, rd0 float64
}

func beginSpan(ctx context.Context, tb testing.TB, rdb goredis.UniversalClient) *span {
	tb.Helper()
	return &span{ctx: ctx, tb: tb, rdb: rdb, t0: time.Now(), self0: selfCPUSeconds(), rd0: redisCPUSeconds(ctx, tb, rdb)}
}

// end returns the cores the generator used and the fraction of one core the Redis server used over the span.
func (sp *span) end() (generatorCores, redisCores float64) {
	wall := time.Since(sp.t0).Seconds()
	return (selfCPUSeconds() - sp.self0) / wall, (redisCPUSeconds(sp.ctx, sp.tb, sp.rdb) - sp.rd0) / wall
}

// resetServerStats clears the per-command histograms so a phase is measured alone.
func resetServerStats(ctx context.Context, tb testing.TB, rdb goredis.UniversalClient) {
	tb.Helper()
	if err := rdb.ConfigResetStat(ctx).Err(); err != nil {
		tb.Fatalf("CONFIG RESETSTAT: %v", err)
	}
	// The latency monitor records an event whenever the server itself stalls for 20 ms or more — a fork for an AOF
	// rewrite, an fsync, a slow command — and says which. Without it a spike in a client's p99 is only a spike.
	if err := rdb.ConfigSet(ctx, "latency-monitor-threshold", "20").Err(); err != nil {
		tb.Fatalf("CONFIG SET latency-monitor-threshold: %v", err)
	}
	if err := rdb.Do(ctx, "LATENCY", "RESET").Err(); err != nil {
		tb.Fatalf("LATENCY RESET: %v", err)
	}
}

// serverStalls names what the server itself stalled on since the last reset, e.g. "fork=312ms aof-write=40ms", or
// "none". A client-side tail with "none" here came from outside the primary.
func serverStalls(ctx context.Context, tb testing.TB, rdb goredis.UniversalClient) string {
	tb.Helper()
	res, err := rdb.Do(ctx, "LATENCY", "LATEST").Result()
	if err != nil {
		tb.Fatalf("LATENCY LATEST: %v", err)
	}
	events, _ := res.([]any)
	var out []string
	for _, e := range events {
		f, _ := e.([]any)
		if len(f) < 4 {
			continue
		}
		name, _ := f[0].(string)
		worst, _ := f[3].(int64)
		out = append(out, fmt.Sprintf("%s=%dms", name, worst))
	}
	if len(out) == 0 {
		return "none"
	}
	return strings.Join(out, ",")
}

// measured reports one finding in a form a person and a script can both read. The numbers in docs/hardening.md are
// pasted from these lines, never retyped.
func measured(t *testing.T, id, what string, kv ...any) {
	t.Helper()
	line := "MEASURED " + id + " " + what
	for i := 0; i+1 < len(kv); i += 2 {
		line += " " + fmt.Sprint(kv[i]) + "=" + fmt.Sprint(kv[i+1])
	}
	t.Log(line)
}
