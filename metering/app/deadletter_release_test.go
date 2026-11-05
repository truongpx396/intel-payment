package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/truongpx396/intel-payment/metering/app"
	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// releaseFailsStore cannot give a claim back: the database went away between the publish and the release.
type releaseFailsStore struct {
	*contracts.MemDeadLetters
	err error
}

func (s *releaseFailsStore) ReleaseReplay(ctx context.Context, id string) error {
	if s.err != nil {
		return s.err
	}
	return s.MemDeadLetters.ReleaseReplay(ctx, id)
}

var _ ports.DeadLetterStore = (*releaseFailsStore)(nil)

// A replay whose publish failed and whose claim then could not be given back says both: the operator reading the error must know
// that the letter stays claimed until the lease runs out, and not only that the bus was down. And a release that worked is not
// mentioned at all — an error that blames a release that succeeded sends the operator after the wrong fault.
func TestAFailedPublishAndAFailedReleaseAreBothReportedAndASuccessfulReleaseIsNot(t *testing.T) {
	t.Parallel()
	clock := contracts.NewFakeClock(epoch)
	store := &releaseFailsStore{MemDeadLetters: contracts.NewMemDeadLetters(clock)}
	bus := &dlBus{}
	dl, err := app.NewDeadLetters(app.DeadLetterDeps{Store: store, Bus: bus, Prefix: dlPrefix})
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{"1-0", "2-0"} {
		if err := dl.Handle(context.Background(), envelope("billing.payment.t1", src, 5, []byte(`{"realm":"acme"}`))); err != nil {
			t.Fatal(err)
		}
	}
	idOf := func(src string) string {
		l, _, err := store.Park(context.Background(), ports.DeadLetter{Subject: "billing.payment.t1", SourceID: src, Realm: "probe"})
		if err != nil {
			t.Fatal(err)
		}
		return l.ID
	}

	// The claim cannot be given back.
	bus.failNext(errors.New("bus unavailable"))
	store.err = errors.New("connection reset")
	_, _, err = dl.Replay(context.Background(), idOf("1-0"))
	if err == nil || !strings.Contains(err.Error(), "bus unavailable") || !strings.Contains(err.Error(), "release the claim") || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("the error must name the failed publish and the claim that could not be released: %v", err)
	}
	if out, _, _ := dl.Replay(context.Background(), idOf("1-0")); out != app.ReplayInProgress {
		t.Fatalf("the claim that was not released still holds: %v", out)
	}

	// The claim is given back.
	store.err = nil
	bus.failNext(errors.New("bus unavailable"))
	_, _, err = dl.Replay(context.Background(), idOf("2-0"))
	if err == nil || !strings.Contains(err.Error(), "bus unavailable") || strings.Contains(err.Error(), "release") {
		t.Fatalf("a release that worked must not be reported as a fault: %v", err)
	}
}
