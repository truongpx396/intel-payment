// Command payment-worker is the sole durable writer: it owns shards, drains the Redis outbox into the books in
// per-scope sequence order, and runs the sweeps that finish what a crash left half done — journal replay,
// transfer re-drive, correction re-issue, credit expiry, reconcile and audit.
//
// One worker owns a shard at a time, by a Postgres session advisory lock; replicas divide the shards between them
// and a replica that dies, or is stopped, hands its shards on. Correctness does not rest on that: every booking is
// guarded by a durable idempotency row, so two owners overlapping during a takeover still book each intent once.
//
// A worker holds one Postgres connection per shard it owns (see docs/operations.md), so PAYMENT_SHARDS
// bounds its connection use. Everything it is told arrives as PAYMENT_* variables (docs/configuration.md), read here
// and nowhere else, and checked before anything is dialled.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run returns the process exit code: 0 a clean stop, 1 it ran and failed or refused to start against what it
// found, 2 usage or configuration.
func run(ctx context.Context, args []string, getenv func(string) string, _, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "usage: payment-worker   (payment-worker %s; configured by PAYMENT_* variables)\n", version)
		return 2
	}
	return serve(ctx, getenv, stderr)
}
