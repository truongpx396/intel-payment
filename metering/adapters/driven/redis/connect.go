package redis

import (
	"fmt"
	"strings"

	goredis "github.com/redis/go-redis/v9"
)

// Connect dials the balance store from a URL:
//
//	redis://[user:pass@]host:port/db          a single primary (rediss:// for TLS)
//	redis+cluster://[user:pass@]host:port?addr=host2:port&addr=host3:port    a Redis Cluster
//
// It returns a UniversalClient so the adapter is the same code on both. The connection is lazy: it
// does not check reachability — readiness does (Store.Ping).
func Connect(url string) (goredis.UniversalClient, error) {
	if rest, ok := strings.CutPrefix(url, "redis+cluster://"); ok {
		opts, err := goredis.ParseClusterURL("redis://" + rest)
		if err != nil {
			return nil, fmt.Errorf("redis: cluster url: %w", err)
		}
		return goredis.NewClusterClient(opts), nil
	}
	opts, err := goredis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("redis: url: %w", err)
	}
	return goredis.NewClient(opts), nil
}
