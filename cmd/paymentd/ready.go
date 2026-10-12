package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/truongpx396/intel-payment/metering/ports"
	"github.com/truongpx396/intel-payment/migrate"
)

// shardBooks is what readiness needs of the books: the shard count they were written under.
type shardBooks interface {
	HotConfig(ctx context.Context) (shards int, found bool, err error)
}

// readiness is /readyz: the service may be sent traffic only when the schema is at head and the books were
// written under the shard count this process is configured with (FR-042). It does not ask Redis. A hot tier
// that is down is not a reason to pull the service out of rotation: Record journals to Postgres and answers
// 202, and that fallback exists precisely so an outage drops no usage.
type readiness struct {
	db     *sql.DB
	ms     []migrate.Migration
	books  shardBooks
	shards int
}

func (r readiness) Check(ctx context.Context) ports.ReadyReport {
	rep := ports.ReadyReport{Migrations: "head"}
	st, err := migrate.Check(ctx, r.db, r.ms)
	switch {
	case errors.Is(err, migrate.ErrBehind):
		rep.Migrations = "behind"
		rep.Problems = append(rep.Problems, fmt.Sprintf("%d migrations are not applied (at %04d, head is %04d): run payment-migrate up", len(st.Pending), st.Current, st.Head))
	case err != nil:
		rep.Migrations = "unknown"
		rep.Problems = append(rep.Problems, "the schema could not be checked: "+err.Error())
	}
	n, found, err := r.books.HotConfig(ctx)
	switch {
	case err != nil:
		rep.Problems = append(rep.Problems, "the recorded shard count could not be read: "+err.Error())
	case found && n != r.shards:
		rep.Shards = n
		rep.Problems = append(rep.Problems, fmt.Sprintf("the books were written under %d shards and this process is configured for %d: changing it is a planned reshard", n, r.shards))
	case found:
		rep.Shards = n
	default:
		rep.Shards = r.shards // nothing recorded yet: the first process to start records its own
	}
	return rep
}
