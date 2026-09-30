package redis

import (
	"context"
	_ "embed"
	"errors"
	"fmt"

	goredis "github.com/redis/go-redis/v9"
)

// LibraryName is the Redis Functions library the hot path lives in (`#!lua name=intelpay`).
const LibraryName = "intelpay"

// Library is the reference hot path, byte for byte. It is a copy of
// specs/001-metering-billing-core/contracts/reference/hot_path.lua because go:embed cannot reach
// outside the package; TestEmbeddedLibraryIsTheReferenceFile fails on any difference.
//
//go:embed lua/hot_path.lua
var Library string

// ErrLibraryMissing means the hot path is not loaded, or a different version is.
var ErrLibraryMissing = errors.New("redis: the intelpay function library is not loaded (or differs from this build's)")

// EnsureInstalled loads the library if it is absent or differs (FUNCTION LOAD REPLACE) and reports
// whether it changed anything. It needs FUNCTION LOAD, which only the deploy role has
// (deploy/redis/README.md): the request tier never installs code into the money store.
//
// Against a Redis Cluster the library must be on every primary; pass a ClusterClient and it is
// loaded on each one.
func EnsureInstalled(ctx context.Context, rdb goredis.UniversalClient) (changed bool, err error) {
	install := func(ctx context.Context, c goredis.Cmdable) error {
		ok, err := libraryCurrent(ctx, c)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		changed = true
		return c.FunctionLoadReplace(ctx, Library).Err()
	}
	if cc, ok := rdb.(*goredis.ClusterClient); ok {
		return changed, cc.ForEachMaster(ctx, func(ctx context.Context, node *goredis.Client) error { return install(ctx, node) })
	}
	return changed, install(ctx, rdb)
}

// CheckInstalled verifies (read-only) that the loaded library is exactly this build's. It needs only
// FUNCTION LIST, which the service role has, so it can back /readyz.
func CheckInstalled(ctx context.Context, rdb goredis.UniversalClient) error {
	check := func(ctx context.Context, c goredis.Cmdable) error {
		ok, err := libraryCurrent(ctx, c)
		if err != nil {
			return err
		}
		if !ok {
			return ErrLibraryMissing
		}
		return nil
	}
	if cc, ok := rdb.(*goredis.ClusterClient); ok {
		return cc.ForEachMaster(ctx, func(ctx context.Context, node *goredis.Client) error { return check(ctx, node) })
	}
	return check(ctx, rdb)
}

func libraryCurrent(ctx context.Context, c goredis.Cmdable) (bool, error) {
	libs, err := c.FunctionList(ctx, goredis.FunctionListQuery{LibraryNamePattern: LibraryName, WithCode: true}).Result()
	if err != nil {
		return false, fmt.Errorf("redis: FUNCTION LIST: %w", err)
	}
	for _, l := range libs {
		if l.Name == LibraryName {
			return l.Code == Library, nil
		}
	}
	return false, nil
}
