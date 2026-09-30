package domain_test

import (
	"testing"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// A card assembled by hand (a test, a fixture) has no index and is scanned; it must answer exactly
// as the indexed card NewRateCard builds does, or a price would depend on how the card was made.
func TestRatesAnswerTheSameOnAHandBuiltCardAsOnAnIndexedOne(t *testing.T) {
	t.Parallel()
	entries := []domain.RateEntry{
		{RateKey: "gpt", Unit: "in", CreditsPerBlock: 150, BlockSize: 1000},
		{RateKey: "gpt", Unit: "out", CreditsPerBlock: 600, BlockSize: 1000},
		{RateKey: "img", Unit: "px", CreditsPerBlock: 1, BlockSize: 1},
	}
	indexed, err := domain.NewRateCard("t1", "v1", "table", entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	byHand := domain.RateCard{Realm: "t1", Version: "v1", Pricer: "table", Entries: entries}

	for name, card := range map[string]domain.RateCard{"indexed": indexed, "hand-built": byHand} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			gpt := card.Rates("gpt")
			if len(gpt) != 2 || gpt["in"].CreditsPerBlock != 150 || gpt["out"].CreditsPerBlock != 600 {
				t.Fatalf("gpt: %+v", gpt)
			}
			if img := card.Rates("img"); len(img) != 1 || img["px"].BlockSize != 1 {
				t.Fatalf("one key's entries must not leak into another's: %+v", img)
			}
			if got := card.Rates("no-such-model"); len(got) != 0 {
				t.Fatalf("an unknown rate key has no entries — pricing fails closed on it: %+v", got)
			}
		})
	}
}
