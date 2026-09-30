//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	hotredis "github.com/truongpx396/intel-payment/metering/adapters/driven/redis"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/redis/redistest"
	"github.com/truongpx396/intel-payment/metering/contracts"
)

// The hot tier goes back in time in the two ways the design names (hot-path-consistency.md §5): Redis
// restarts from an AOF that lost its tail, and traffic fails over to a replica that had stopped
// receiving. Each subtest gets Redis containers of its own — it destroys them.

func TestConsistencyContract_RestartFromATruncatedAOF(t *testing.T) {
	t.Parallel()
	contracts.ConsistencyContract(t, truncatedAOF(false))
}

// The same rollbacks with the intents travelling by the optional bus. The Relay's own consumer group
// can be lost with the rollback too — it then reads the outbox again from the start, and JetStream
// recognises every entry it had already stored by its message id.
func TestConsistencyContract_RestartFromATruncatedAOF_OverJetStream(t *testing.T) {
	t.Parallel()
	contracts.ConsistencyContract(t, truncatedAOF(true))
}

func TestConsistencyContract_FailoverToALaggingReplica(t *testing.T) {
	t.Parallel()
	contracts.ConsistencyContract(t, laggingReplica(false))
}

func TestConsistencyContract_FailoverToALaggingReplica_OverJetStream(t *testing.T) {
	t.Parallel()
	contracts.ConsistencyContract(t, laggingReplica(true))
}

func truncatedAOF(jetstream bool) func(t *testing.T) contracts.ConsistencyHarness {
	return func(t *testing.T) contracts.ConsistencyHarness {
		t.Helper()
		ctx := context.Background()
		s := newWriterStack(t, wopts{jetstream: jetstream, dedicated: true, redisOpts: redistest.Options{AppendFsync: "always", FixedPort: true}})
		var size int64
		return contracts.ConsistencyHarness{
			WriterHarness: s.writerHarness(),
			Checkpoint: func(t *testing.T) {
				t.Helper()
				var err error
				size, err = s.srv.AOFSize(ctx)
				must(t, err)
			},
			Rollback: func(t *testing.T) {
				t.Helper()
				must(t, s.srv.Stop(ctx))
				must(t, s.srv.TruncateAOF(ctx, size))
				must(t, s.srv.Start(ctx))
			},
		}
	}
}

func laggingReplica(jetstream bool) func(t *testing.T) contracts.ConsistencyHarness {
	return func(t *testing.T) contracts.ConsistencyHarness {
		t.Helper()
		ctx := context.Background()
		netName, removeNet, err := redistest.NewNetwork(ctx)
		must(t, err)
		t.Cleanup(removeNet)
		primary, err := redistest.RunWith(ctx, redistest.Options{Mode: redistest.Standalone, Network: netName, Alias: "primary"})
		must(t, err)
		t.Cleanup(func() { _ = primary.Terminate(context.Background()) })
		replica, err := redistest.RunWith(ctx, redistest.Options{Mode: redistest.Standalone, Network: netName, Alias: "replica", ReplicaOf: "primary 6379"})
		must(t, err)
		t.Cleanup(func() { _ = replica.Terminate(context.Background()) })

		pc := primary.Client()
		defer func() { _ = pc.Close() }()
		_, err = hotredis.EnsureInstalled(ctx, pc)
		must(t, err)
		waitReplicated(t, primary, replica)

		s := newWriterStack(t, wopts{jetstream: jetstream, srv: primary, proxied: true})
		return contracts.ConsistencyHarness{
			WriterHarness: s.writerHarness(),
			// The replica has everything up to here; from here it receives nothing more. That is what a
			// replica behind by the replication lag looks like when it is promoted.
			Checkpoint: func(t *testing.T) {
				t.Helper()
				waitReplicated(t, primary, replica)
				must(t, replica.Promote(ctx))
			},
			Rollback: func(t *testing.T) {
				t.Helper()
				s.proxy.Retarget(replica.Addr)
			},
		}
	}
}

// waitReplicated waits until the replica has applied everything the primary has written.
func waitReplicated(t *testing.T, primary, replica *redistest.Redis) {
	t.Helper()
	ctx := context.Background()
	p, r := primary.Client(), replica.Client()
	defer func() { _ = p.Close(); _ = r.Close() }()
	deadline := time.Now().Add(20 * time.Second)
	for {
		pi, _ := p.Info(ctx, "replication").Result()
		ri, _ := r.Info(ctx, "replication").Result()
		po, ro := field(pi, "master_repl_offset"), field(ri, "master_repl_offset")
		if strings.Contains(ri, "master_link_status:up") && po != "" && po == ro {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the replica did not catch up: primary %q replica %q\n%s", po, ro, ri)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func field(info, name string) string {
	for _, line := range strings.Split(info, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), name+":"); ok {
			return v
		}
	}
	return ""
}
