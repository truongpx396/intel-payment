package app_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"testing"
	"unicode/utf8"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// An adjustment's request is stored in one canonical form, and that form is what decides whether a retry is the same operation (the database
// compares it as JSON) or a conflict, and what the worker later reads to move money. So it is held to an independent reference: a canonical
// string built by hand, and the bound on the amount taken by math/big. However the JSON is spelled — key order, whitespace — the verdict and
// the stored form are the same, and a request the reference calls valid is accepted and one it calls invalid is refused as invalid.
func FuzzAnAdjustmentIsReadTheSameWhateverItsJSONLooksLike(f *testing.F) {
	ceiling := big.NewInt(int64(domain.MaxOperationAmount))
	for _, s := range []struct {
		kind, id, pool string
		amount         int64
	}{
		{"workspace", "w1", "", 500}, {"workspace", "w1", "promo", -30}, {"workspace", "w1", "general", 1}, {"workspace", "w1", "", -1},
		{"workspace", "w1", "", int64(domain.MaxOperationAmount)}, {"workspace", "w1", "", -int64(domain.MaxOperationAmount)},
		{"workspace", "w1", "", int64(domain.MaxOperationAmount) + 1}, {"workspace", "w1", "", -int64(domain.MaxOperationAmount) - 1},
		{"workspace", "w1", "", 0}, {"workspace", "w1", "", math.MaxInt64}, {"workspace", "w1", "", math.MinInt64},
		{"Work Space", "w1", "", 5}, {"workspace", "", "", 5}, {"workspace", "w1", "Not A Pool", 5}, {"workspace", "w</>&1", "", 5},
	} {
		f.Add(s.kind, s.id, s.pool, s.amount)
	}
	f.Fuzz(func(t *testing.T, kind, id, pool string, amount int64) {
		if !utf8.ValidString(kind) || !utf8.ValidString(id) || !utf8.ValidString(pool) {
			t.Skip("JSON cannot spell invalid UTF-8 the way a Go string holds it")
		}
		q := func(v string) string { b, _ := json.Marshal(v); return string(b) }
		spellings := []string{
			fmt.Sprintf(`{"scope":{"kind":%s,"id":%s},"pool":%s,"amount":%d}`, q(kind), q(id), q(pool), amount),
			fmt.Sprintf(" {\n \"amount\" : %d , \"pool\":%s,\"scope\": {\"id\": %s ,\"kind\":%s} } ", amount, q(pool), q(id), q(kind)),
			fmt.Sprintf(`{"amount":%d,"scope":{"id":%s,"kind":%s},"pool":%s}`, amount, q(id), q(kind), q(pool)),
		}

		valid := (domain.Scope{Realm: "default", Kind: kind, ID: id}).Validate() == nil && domain.Pool(pool).Validate() == nil &&
			amount != 0 && new(big.Int).Abs(big.NewInt(amount)).Cmp(ceiling) <= 0
		canonicalPool := pool
		if canonicalPool == "" {
			canonicalPool = string(domain.GeneralPool)
		}
		want := fmt.Sprintf(`{"scope":{"kind":%s,"id":%s},"pool":%s,"amount":%d}`, q(kind), q(id), q(canonicalPool), amount)

		for i, sp := range spellings {
			r := newAdminRig(t)
			op, _, err := r.ops.Submit(t.Context(), req(ports.AdminAdjust, sp, fmt.Sprintf("k-%d", i)))
			switch {
			case valid && err != nil:
				t.Fatalf("a request the reference calls valid was refused: %s: %v", sp, err)
			case !valid && !errors.Is(err, domain.ErrInvalid):
				t.Fatalf("a request the reference calls invalid came to %v, want %v: %s", err, domain.ErrInvalid, sp)
			case valid && string(op.Params) != want:
				t.Fatalf("%s was stored as %s, want %s", sp, op.Params, want)
			}
		}
	})
}
