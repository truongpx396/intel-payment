package redistest

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go/network"
)

// What follows is how a test destroys a Redis on purpose — the rollback a truncated AOF or a failover
// to a lagging replica causes (hot-path-consistency.md §5). It works on a standalone Redis started
// with FixedPort (so its address survives a restart) and, for the AOF, AppendFsync "always".

// NewNetwork creates a Docker network for containers that must reach each other by name.
func NewNetwork(ctx context.Context) (name string, remove func(), err error) {
	n, err := network.New(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("redistest: network: %w", err)
	}
	return n.Name, func() { _ = n.Remove(context.Background()) }, nil
}

// Stop stops the container; the data directory survives.
func (r *Redis) Stop(ctx context.Context) error {
	d := 10 * time.Second
	return r.Container.Stop(ctx, &d)
}

// Start starts it again and waits until it answers.
func (r *Redis) Start(ctx context.Context) error {
	if err := r.Container.Start(ctx); err != nil {
		return err
	}
	c := goredis.NewClient(&goredis.Options{Addr: r.Addr})
	defer func() { _ = c.Close() }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := c.Ping(ctx).Err(); err == nil {
			return nil
		} else if time.Now().After(deadline) {
			return fmt.Errorf("redistest: redis did not come back: %w", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// aofFile is the newest incremental AOF file: what a rollback truncates.
func (r *Redis) aofFile(ctx context.Context) (string, error) {
	code, out, err := execIn(ctx, r.Container, "sh", "-c", "ls /data/appendonlydir/*.incr.aof | sort | tail -n 1")
	if err != nil || code != 0 {
		return "", fmt.Errorf("redistest: no incremental AOF: code %d %q: %w", code, out, err)
	}
	return strings.TrimSpace(out), nil
}

// AOFSize is the size of the incremental AOF now. With appendfsync always, everything acknowledged
// so far is inside it.
func (r *Redis) AOFSize(ctx context.Context) (int64, error) {
	f, err := r.aofFile(ctx)
	if err != nil {
		return 0, err
	}
	r.aofPath = f
	code, out, err := execIn(ctx, r.Container, "stat", "-c", "%s", f)
	if err != nil || code != 0 {
		return 0, fmt.Errorf("redistest: stat %s: code %d %q: %w", f, code, out, err)
	}
	return strconv.ParseInt(strings.TrimSpace(out), 10, 64)
}

// TruncateAOF cuts the incremental AOF back to size. The container must be stopped: this is the disk
// a crash left behind, not a live edit. Cut at a size AOFSize reported and the file ends at a command
// boundary; anywhere else Redis loads what is intact (aof-load-truncated).
func (r *Redis) TruncateAOF(ctx context.Context, size int64) error {
	f := r.aofPath
	if f == "" {
		return fmt.Errorf("redistest: call AOFSize before stopping, so the file to truncate is known")
	}
	rc, err := r.Container.CopyFileFromContainer(ctx, f)
	if err != nil {
		return fmt.Errorf("redistest: read %s: %w", f, err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	if int64(len(b)) < size {
		return fmt.Errorf("redistest: AOF is %d bytes, cannot truncate to %d", len(b), size)
	}
	// 0666: the file is written by the image's redis user, not by root.
	return r.Container.CopyToContainer(ctx, b[:size], f, 0o666)
}

// Promote turns a replica into a standalone primary at whatever it has received: it stops
// receiving here, and takes a replication id of its own.
func (r *Redis) Promote(ctx context.Context) error {
	c := goredis.NewClient(&goredis.Options{Addr: r.Addr})
	defer func() { _ = c.Close() }()
	return c.Do(ctx, "REPLICAOF", "NO", "ONE").Err()
}
