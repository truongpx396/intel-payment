package redis

import (
	"errors"
	"fmt"
	"strings"

	goredis "github.com/redis/go-redis/v9"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// statusError maps a hot function's refusal to the typed domain error the Meter acts on. It never
// returns nil for a status that is not a success.
func statusError(status string) error {
	switch status {
	case "COLD_SCOPE":
		return domain.ErrColdScope
	case "COLD_SHARD":
		return domain.ErrColdShard
	case "FROZEN":
		return domain.ErrShardFrozen
	case "BACKPRESSURE":
		return domain.ErrBackpressure
	case "IDEM_CONFLICT":
		return domain.ErrIdemConflict
	case "INSUFFICIENT":
		return domain.ErrInsufficient
	}
	return fmt.Errorf("redis: unexpected hot function status %q", status)
}

// unavailablePrefixes are Redis error replies that mean "the node cannot serve this right now", as
// opposed to a defect in the request or the installed library.
var unavailablePrefixes = []string{"LOADING", "READONLY", "CLUSTERDOWN", "TRYAGAIN", "MASTERDOWN", "MOVED", "ASK", "BUSY", "NOREPLICAS"}

// wrapErr classifies a go-redis error. Anything that stopped the call reaching a serving node — a
// dial error, a timeout, a cancelled context, a node that is loading or read-only — is
// ErrHotStoreUnavailable, so usage is journaled rather than lost. A server error that is a defect
// (a missing function, a bad argument) is returned as-is and loudly: silently journaling around a
// broken deploy would hide it until the journal filled.
func wrapErr(op string, err error) error {
	if err == nil {
		return nil
	}
	var re goredis.Error
	if errors.As(err, &re) {
		msg := re.Error()
		for _, p := range unavailablePrefixes {
			if strings.HasPrefix(msg, p) {
				return fmt.Errorf("%w: %s: %w", domain.ErrHotStoreUnavailable, op, err)
			}
		}
		if strings.Contains(msg, "NOPERM") {
			return fmt.Errorf("redis: %s: the service role lacks a permission the hot path needs: %w", op, err)
		}
		return fmt.Errorf("redis: %s: %w", op, err)
	}
	return fmt.Errorf("%w: %s: %w", domain.ErrHotStoreUnavailable, op, err)
}
