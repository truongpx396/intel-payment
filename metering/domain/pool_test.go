package domain_test

import (
	"slices"
	"testing"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// Draw order decides WHICH credits a charge spends, so it decides which lot expires unused. It is
// total and deterministic: ascending priority, ties by name, general always last.
func TestPoolsAreDrawnByAscendingPriorityThenNameWithGeneralLast(t *testing.T) {
	t.Parallel()
	defs := []domain.PoolDef{
		{Name: "bonus", Priority: 30},
		{Name: "promo", Priority: 10},
		{Name: "trial", Priority: 20},
		{Name: "annual", Priority: 20}, // ties with trial: by name
		{Name: "earlybird", Priority: 10},
	}
	got := domain.EligiblePools(defs, "llm.chat")
	want := []domain.Pool{"earlybird", "promo", "annual", "trial", "bonus", domain.GeneralPool}
	if !slices.Equal(got, want) {
		t.Fatalf("draw order\n got %v\nwant %v", got, want)
	}

	// The same set in any input order draws identically.
	rev := slices.Clone(defs)
	slices.Reverse(rev)
	if again := domain.EligiblePools(rev, "llm.chat"); !slices.Equal(again, want) {
		t.Fatalf("input order leaked into draw order: %v", again)
	}
}

func TestANegativePriorityDrawsBeforeZero(t *testing.T) {
	t.Parallel()
	got := domain.EligiblePools([]domain.PoolDef{{Name: "later", Priority: 0}, {Name: "first", Priority: -5}}, "x")
	if want := []domain.Pool{"first", "later", domain.GeneralPool}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAResourceScopedPoolPaysOnlyForItsResources(t *testing.T) {
	t.Parallel()
	gpu := domain.PoolDef{Name: "gpu", Priority: 1, AppliesTo: []string{"gpu.job", "gpu.render"}}
	for resource, covered := range map[string]bool{"gpu.job": true, "gpu.render": true, "llm.chat": false, "": false} {
		if got := gpu.Covers(resource); got != covered {
			t.Errorf("gpu.Covers(%q) = %v, want %v", resource, got, covered)
		}
	}
	if !(domain.PoolDef{Name: "anything"}).Covers("whatever") {
		t.Error("a pool with no restriction pays for every resource")
	}
	if !(domain.PoolDef{Name: domain.GeneralPool, AppliesTo: []string{"x"}}).Covers("y") {
		t.Error("general pays for everything, whatever a row says")
	}
}
