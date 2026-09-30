package redis

import (
	"context"
	"errors"
	"fmt"
	"testing"

	goredis "github.com/redis/go-redis/v9"

	"github.com/truongpx396/intel-payment/metering/domain"
)

type serverErr string

func (e serverErr) Error() string { return string(e) }
func (serverErr) RedisError()     {}

var _ goredis.Error = serverErr("")

func TestStatusErrors(t *testing.T) {
	t.Parallel()
	for status, want := range map[string]error{
		"COLD_SCOPE": domain.ErrColdScope, "COLD_SHARD": domain.ErrColdShard, "FROZEN": domain.ErrShardFrozen,
		"BACKPRESSURE": domain.ErrBackpressure, "IDEM_CONFLICT": domain.ErrIdemConflict, "INSUFFICIENT": domain.ErrInsufficient,
	} {
		if err := statusError(status); !errors.Is(err, want) {
			t.Errorf("%s → %v, want %v", status, err, want)
		}
	}
	if err := statusError("NEW_STATUS"); err == nil {
		t.Fatal("an unknown status must be an error, never a success")
	}
}

func TestWrapErrClassifiesWhatJournalingMayAbsorb(t *testing.T) {
	t.Parallel()
	unavailable := []error{
		context.DeadlineExceeded, context.Canceled, errors.New("dial tcp: connection refused"),
		serverErr("LOADING Redis is loading the dataset in memory"), serverErr("READONLY You can't write against a read only replica."),
		serverErr("CLUSTERDOWN The cluster is down"), serverErr("MOVED 3999 127.0.0.1:6381"), serverErr("TRYAGAIN Multiple keys request during rehashing of slot"),
	}
	for _, e := range unavailable {
		if err := wrapErr("ip_debit", e); !errors.Is(err, domain.ErrHotStoreUnavailable) {
			t.Errorf("%v must be ErrHotStoreUnavailable so the charge is journaled, got %v", e, err)
		}
	}
	loud := []error{
		serverErr("ERR Function not found"), serverErr("ERR amount must be >= 0"),
		serverErr("NOPERM User payment has no permissions to run the 'function|load' command"),
	}
	for _, e := range loud {
		err := wrapErr("ip_debit", e)
		if errors.Is(err, domain.ErrHotStoreUnavailable) {
			t.Errorf("%v is a defect, not an outage: journaling around it would hide a broken deploy", e)
		}
		if err == nil {
			t.Errorf("%v vanished", e)
		}
	}
	if wrapErr("x", nil) != nil {
		t.Fatal("nil stays nil")
	}
	if !errors.Is(wrapErr("x", fmt.Errorf("wrapped: %w", context.DeadlineExceeded)), context.DeadlineExceeded) {
		t.Fatal("the cause must stay reachable")
	}
}
