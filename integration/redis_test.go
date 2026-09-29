//go:build integration

package integration

import goredis "github.com/redis/go-redis/v9"

func hotredisClient(addr string) goredis.UniversalClient {
	return goredis.NewClient(&goredis.Options{Addr: addr, DialTimeout: 300 * 1e6, MaxRetries: -1})
}
