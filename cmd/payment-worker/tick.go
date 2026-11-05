package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/truongpx396/intel-payment/cmd/internal/envcfg"
	"github.com/truongpx396/intel-payment/cmd/internal/wiring"
	hotredis "github.com/truongpx396/intel-payment/metering/adapters/driven/redis"
	"github.com/truongpx396/intel-payment/metering/app"
)

// publishTick is `payment-worker tick <name> [payload]`: the door an external scheduler (a cron, a Kubernetes CronJob) publishes a
// scheduled tick through (bus-subjects.md: "Scheduled work is externally triggered"). It publishes one message to
// `<prefix>.<name>.tick` and exits; whichever worker replica receives it does the work, under the tick's claim, so publishing it twice is
// harmless. It needs the same PAYMENT_* variables as the worker, and only the bus.
//
//	payment-worker tick journal
//	payment-worker tick reconcile '{"shard":3}'
//	payment-worker tick reconcile '{"shard":3,"kind":"full"}'
//
// A per-shard tick (reconcile, audit) must name its shard: a scheduler publishes one per shard.
func publishTick(ctx context.Context, s settings, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 || len(args) > 2 {
		fmt.Fprintf(stderr, "usage: payment-worker tick <name> [json payload]   (name is one of: %s)\n", strings.Join(app.TickNames, ", "))
		return 2
	}
	name := args[0]
	if !slices.Contains(app.TickNames, name) {
		fmt.Fprintf(stderr, "payment-worker tick: %q is not a tick (they are %s)\n", name, strings.Join(app.TickNames, ", "))
		return 2
	}
	payload := []byte(`{}`)
	if len(args) == 2 {
		if !json.Valid([]byte(args[1])) {
			fmt.Fprintf(stderr, "payment-worker tick: the payload is not JSON\n")
			return 2
		}
		payload = []byte(args[1])
	}
	if name == "reconcile" || name == "audit" {
		if _, err := app.ParseShard(payload, s.Engine.Shards); err != nil {
			fmt.Fprintf(stderr, "payment-worker tick: %v\n", err)
			return 2
		}
	}
	if name == "usage.rollup" {
		if _, _, err := app.ParseRollupDays(payload); err != nil {
			fmt.Fprintf(stderr, "payment-worker tick: %v\n", err)
			return 2
		}
	}
	rdb, err := hotredis.Connect(s.Engine.BalanceRedisURL)
	if err != nil {
		fmt.Fprintln(stderr, "payment-worker tick: PAYMENT_BALANCE_REDIS_URL is not a redis:// or redis+cluster:// URL")
		return 2
	}
	defer func() { _ = rdb.Close() }()
	bus, closeBus, err := wiring.NewBus(ctx, envcfg.Bus{Kind: envcfg.BusStreams}, s.Engine, rdb)
	if err != nil {
		fmt.Fprintf(stderr, "payment-worker tick: %v\n", err)
		return 1
	}
	defer closeBus()
	subject := app.TickSubject(s.Engine.SubjectPrefix, name)
	if err := bus.Publish(ctx, subject, payload); err != nil {
		fmt.Fprintf(stderr, "payment-worker tick: publish %s: %v\n", subject, err)
		return 1
	}
	fmt.Fprintf(stdout, "published %s %s\n", subject, payload)
	return 0
}
