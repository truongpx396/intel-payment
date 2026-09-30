package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

func TestGrantPassesThroughAndIsNeverJournaled(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{})
	rc, err := r.m.Grant(context.Background(), domain.Grant{Scope: ws, Amount: 100, IdemKey: "pay-1", Reason: "purchase"})
	if err != nil || !rc.Applied {
		t.Fatalf("%+v %v", rc, err)
	}
	// During an outage a grant is an error, not "accepted": nothing is minted until the hot tier says so.
	r.bal.grant = func(domain.Grant) (domain.Receipt, error) { return domain.Receipt{}, domain.ErrShardFrozen }
	rc, err = r.m.Grant(context.Background(), domain.Grant{Scope: ws, Amount: 100, IdemKey: "pay-2", Reason: "purchase"})
	if !errors.Is(err, domain.ErrShardFrozen) || rc.Applied || len(r.journal.entries) != 0 {
		t.Fatalf("%+v %v", rc, err)
	}
	r.bal.grant = func(domain.Grant) (domain.Receipt, error) { return domain.Receipt{}, domain.ErrIdemConflict }
	if _, err := r.m.Grant(context.Background(), domain.Grant{Scope: ws, Amount: 1, IdemKey: "k", Reason: "x"}); !errors.Is(err, domain.ErrIdemConflict) || r.met.count("metering_idem_conflict_total{grant}") != 1 {
		t.Fatal(err)
	}
	if _, err := r.m.Grant(context.Background(), domain.Grant{Scope: ws, IdemKey: "k", Reason: "x"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("a zero grant: %v", err)
	}
}

func TestGrantRehydratesAColdScope(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{})
	n := 0
	r.bal.grant = func(g domain.Grant) (domain.Receipt, error) {
		n++
		if n == 1 {
			return domain.Receipt{}, domain.ErrColdScope
		}
		return domain.Receipt{IdemKey: g.IdemKey, Applied: true, Seq: 1}, nil
	}
	if rc, err := r.m.Grant(context.Background(), domain.Grant{Scope: ws, Amount: 5, IdemKey: "k", Reason: "signup_grant"}); err != nil || !rc.Applied {
		t.Fatalf("%+v %v", rc, err)
	}
}

func TestTransferReplayReadsItsStatusFromTheBooks(t *testing.T) {
	t.Parallel()
	to := domain.Scope{Realm: "t1", Kind: "organization", ID: "o1"}
	tr := domain.Transfer{From: ws, To: to, Amount: 100, IdemKey: "alloc-1", Reason: "allocation"}
	r := newRig(t, rigOpt{})

	// Phase 1 applied.
	r.bal.transfer = func(domain.Transfer) (domain.TransferReceipt, error) {
		return domain.TransferReceipt{IdemKey: "alloc-1", Applied: true, Amount: 100, FromBalance: 900, Status: domain.TransferInTransit}, nil
	}
	rc, err := r.m.Transfer(context.Background(), tr)
	if err != nil || rc.Status != domain.TransferInTransit || rc.FromBalance != 900 {
		t.Fatalf("%+v %v", rc, err)
	}

	// A replay: the hot tier does not know the status; the books do.
	r.bal.transfer = func(domain.Transfer) (domain.TransferReceipt, error) {
		return domain.TransferReceipt{IdemKey: "alloc-1", Amount: 100}, nil
	}
	rc, _ = r.m.Transfer(context.Background(), tr)
	if rc.Applied || rc.Status != domain.TransferInTransit {
		t.Fatalf("the writer has not booked it yet: in transit. %+v", rc)
	}
	r.books.transfer = &ports.TransferRecord{Status: domain.TransferSettled}
	rc, _ = r.m.Transfer(context.Background(), tr)
	if rc.Status != domain.TransferSettled {
		t.Fatalf("settled by the writer: %+v", rc)
	}

	for want, cause := range map[error]error{domain.ErrInsufficient: domain.ErrInsufficient, domain.ErrIdemConflict: domain.ErrIdemConflict, domain.ErrDestBalanceCap: domain.ErrDestBalanceCap} {
		r.bal.transfer = func(domain.Transfer) (domain.TransferReceipt, error) { return domain.TransferReceipt{}, cause }
		if _, err := r.m.Transfer(context.Background(), tr); !errors.Is(err, want) {
			t.Fatalf("want %v, got %v", want, err)
		}
	}
	// Validation happens before the hot tier: a cross-realm transfer never reaches it.
	bad := tr
	bad.To = domain.Scope{Realm: "t2", Kind: "organization", ID: "o1"}
	r.bal.transfer = func(domain.Transfer) (domain.TransferReceipt, error) {
		t.Fatal("reached the hot tier")
		return domain.TransferReceipt{}, nil
	}
	if _, err := r.m.Transfer(context.Background(), bad); !errors.Is(err, domain.ErrCrossRealm) {
		t.Fatalf("%v", err)
	}
}
