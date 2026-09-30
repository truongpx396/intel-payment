// Package redistest starts a real Redis for tests, in standalone or cluster mode. It is what makes
// the hot path's one-slot rule a tested property rather than a claim: only a cluster refuses a
// function that touches two hash slots, so the same suite runs against both.
package redistest

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	goredis "github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Image is the Redis under test. WAITAOF (HotAckWait) needs 7.2+.
const Image = "redis:7-alpine"

// Mode selects the topology.
type Mode string

const (
	Standalone Mode = "standalone"
	Cluster    Mode = "cluster"
)

// Redis is a running instance.
type Redis struct {
	Mode      Mode
	Addr      string
	Container testcontainers.Container

	aofPath string // the incremental AOF, learned by AOFSize while the container is up
}

// Client returns a fresh client for it. Each caller owns and closes its own.
func (r *Redis) Client() goredis.UniversalClient {
	if r.Mode == Cluster {
		return goredis.NewClusterClient(&goredis.ClusterOptions{Addrs: []string{r.Addr}})
	}
	return goredis.NewClient(&goredis.Options{Addr: r.Addr})
}

// Start launches Redis and stops it with the test. noeviction + AOF, as production requires.
func Start(ctx context.Context, tb testing.TB, mode Mode) *Redis {
	tb.Helper()
	r, err := Run(ctx, mode)
	if err != nil {
		tb.Fatalf("redistest: %v", err)
	}
	tb.Cleanup(func() { _ = r.Terminate(context.Background()) })
	return r
}

// Terminate stops the container.
func (r *Redis) Terminate(_ context.Context) error {
	return testcontainers.TerminateContainer(r.Container)
}

// Run launches Redis without a testing.TB, for a package-level TestMain.
func Run(ctx context.Context, mode Mode) (*Redis, error) { return RunWith(ctx, Options{Mode: mode}) }

// Options tune a launch.
type Options struct {
	Mode Mode
	// ACLFile is a host path to an ACL file (standalone only). With it the default user is whatever
	// the file says — deploy/redis/users.acl turns it off — so a test can run as the service role.
	ACLFile string

	// The rest is for a test that destroys a Redis on purpose — standalone only.
	//
	// AppendFsync sets appendfsync ("always" makes the AOF exactly what has been acknowledged, so a
	// test can truncate it at a known boundary). FixedPort publishes a port that survives Stop/Start.
	// Network and Alias join a Docker network under a name other containers can reach, and ReplicaOf
	// ("primary 6379") starts this Redis as a replica of one of them.
	AppendFsync string
	FixedPort   bool
	Network     string
	Alias       string
	ReplicaOf   string
}

// RunWith launches Redis with options.
func RunWith(ctx context.Context, o Options) (*Redis, error) {
	mode := o.Mode
	args := []string{"redis-server", "--appendonly", "yes", "--maxmemory-policy", "noeviction"}
	req := testcontainers.ContainerRequest{Image: Image, WaitingFor: wait.ForLog("Ready to accept connections").WithStartupTimeout(time.Minute)}
	if o.ACLFile != "" {
		if mode == Cluster {
			return nil, fmt.Errorf("redistest: an ACL file is supported in standalone mode only")
		}
		args = append(args, "--aclfile", "/etc/redis/users.acl")
		req.Files = []testcontainers.ContainerFile{{HostFilePath: o.ACLFile, ContainerFilePath: "/etc/redis/users.acl", FileMode: 0o644}}
	}
	port := "6379"

	if mode == Cluster {
		// A Redis Cluster tells clients to connect to the address it ANNOUNCES. A container's own
		// address is unreachable from the host, so publish the same port number on both sides and
		// announce 127.0.0.1 — then the client can follow the cluster's redirects.
		p, err := freePort()
		if err != nil {
			return nil, err
		}
		port = fmt.Sprint(p)
		args = append(args, "--port", port, "--cluster-enabled", "yes", "--cluster-announce-ip", "127.0.0.1",
			"--cluster-announce-port", port, "--cluster-node-timeout", "5000")
		req.ExposedPorts = []string{port + "/tcp"}
		req.HostConfigModifier = func(hc *container.HostConfig) {
			hc.PortBindings = network.PortMap{network.MustParsePort(port + "/tcp"): {{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: port}}}
		}
	} else if o.FixedPort {
		p, err := freePort()
		if err != nil {
			return nil, err
		}
		port = fmt.Sprint(p)
		args = append(args, "--port", port)
		req.ExposedPorts = []string{port + "/tcp"}
		req.HostConfigModifier = func(hc *container.HostConfig) {
			hc.PortBindings = network.PortMap{network.MustParsePort(port + "/tcp"): {{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: port}}}
		}
	} else {
		req.ExposedPorts = []string{"6379/tcp"}
	}
	if o.AppendFsync != "" {
		args = append(args, "--appendfsync", o.AppendFsync)
	}
	if o.ReplicaOf != "" {
		host, port, ok := strings.Cut(o.ReplicaOf, " ")
		if !ok {
			return nil, fmt.Errorf("redistest: ReplicaOf must be \"host port\", got %q", o.ReplicaOf)
		}
		args = append(args, "--replicaof", host, port)
	}
	if o.Network != "" {
		req.Networks = []string{o.Network}
		if o.Alias != "" {
			req.NetworkAliases = map[string][]string{o.Network: {o.Alias}}
		}
	}
	req.Cmd = args

	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{ContainerRequest: req, Started: true})
	if err != nil {
		return nil, fmt.Errorf("start redis (%s): %w", mode, err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(host, port)
	if mode == Standalone {
		if !o.FixedPort {
			mapped, err := c.MappedPort(ctx, "6379/tcp")
			if err != nil {
				return nil, err
			}
			addr = net.JoinHostPort(host, mapped.Port())
		}
	} else {
		// Own every slot, as a one-node cluster.
		if code, out, err := execIn(ctx, c, "redis-cli", "-p", port, "cluster", "addslotsrange", "0", "16383"); err != nil || code != 0 {
			return nil, fmt.Errorf("cluster addslotsrange: code %d %s: %w", code, out, err)
		}
		rdb := goredis.NewClient(&goredis.Options{Addr: addr})
		defer func() { _ = rdb.Close() }()
		deadline := time.Now().Add(30 * time.Second)
		for {
			info, _ := rdb.ClusterInfo(ctx).Result()
			if contains(info, "cluster_state:ok") {
				break
			}
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("cluster did not become ready: %s", info)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	return &Redis{Mode: mode, Addr: addr, Container: c}, nil
}

// freePort finds an unused port at most 55535: a Redis Cluster's bus listens on port+10000, and
// 65535 is the ceiling. (The OS would hand out ephemeral ports above that range.)
func freePort() (int, error) {
	for i := 0; i < 200; i++ {
		p := 20000 + rand.IntN(35000) //nolint:gosec // a test port, not a secret
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			continue
		}
		_ = l.Close()
		bus, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p+10000))
		if err != nil {
			continue
		}
		_ = bus.Close()
		return p, nil
	}
	return 0, fmt.Errorf("no free port found for a cluster node")
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
