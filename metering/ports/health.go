package ports

import "context"

// ReadyReport is what a readiness check found. The service may be sent traffic only at migration head with the
// configured shard count (FR-042): a rollout must not serve money against a half-migrated schema or a keyspace
// sharded differently from the one the books were written under.
type ReadyReport struct {
	// Migrations is "head", "behind", or "unknown" when the schema could not be read at all.
	Migrations string
	// Shards is the shard count recorded in the books, 0 when unknown.
	Shards int
	// Problems say, in words an operator can act on, everything that is wrong. Empty with Migrations "head" is ready.
	Problems []string
}

// Ready reports whether the service may be sent traffic.
func (r ReadyReport) Ready() bool { return r.Migrations == "head" && len(r.Problems) == 0 }

// Readiness checks, within the context's deadline, whether the service may be sent traffic. It reads the
// dependencies; liveness never does.
type Readiness interface {
	Check(ctx context.Context) ReadyReport
}
