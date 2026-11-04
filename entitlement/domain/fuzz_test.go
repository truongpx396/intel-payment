package domain_test

import (
	"math"
	"math/big"
	"testing"

	"github.com/truongpx396/intel-payment/entitlement/domain"
)

// R100, as properties. -1 is the value an `if limit > n` gets wrong, and int64 addition is the arithmetic that wraps, so
// each target is held to an INDEPENDENT reference (math/big, and a hand-written rule for unlimited), with the boundary cases
// as seeds. The seed corpus runs on every `go test`; `make fuzz` explores beyond it.

func FuzzQuotaMergeIsMaxWithUnlimitedAbove(f *testing.F) {
	for _, s := range [][2]int64{{-1, 0}, {0, -1}, {-1, -1}, {-1, math.MaxInt64}, {math.MaxInt64, -1}, {0, 0}, {25, 5}, {math.MaxInt64, math.MaxInt64 - 1}} {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, a, b int64) {
		if a < -1 || b < -1 {
			t.Skip("below -1 is not a quota")
		}
		got, err := quotaKey.Merge(domain.QuotaValue(a), domain.QuotaValue(b))
		if err != nil {
			t.Fatalf("merge(%d, %d): %v", a, b, err)
		}
		// The reference: unlimited if either side is, else the larger — compared as big integers.
		want := domain.QuotaValue(-1)
		if a != -1 && b != -1 {
			m := big.NewInt(a)
			if big.NewInt(b).Cmp(m) > 0 {
				m = big.NewInt(b)
			}
			want = domain.QuotaValue(m.Int64())
		}
		if got != want {
			t.Fatalf("merge(%d, %d) = %+v, want %+v", a, b, got, want)
		}
		if back, _ := quotaKey.Merge(domain.QuotaValue(b), domain.QuotaValue(a)); back != got {
			t.Fatalf("merge is not commutative on (%d, %d): %+v vs %+v", a, b, got, back)
		}
	})
}

func FuzzAdmitsMatchesBigIntegerArithmetic(f *testing.F) {
	m := int64(math.MaxInt64)
	for _, s := range [][3]int64{
		{25, 24, 1}, {25, 25, 1}, {0, 0, 0}, {0, 0, 1}, {-1, 0, 1}, {-1, m, m}, {m, m, 1}, {m, m - 1, 1},
		{m, 1, m}, {100, m, m}, {25, 30, 0}, {m, 0, m}, {m, m, 0},
	} {
		f.Add(s[0], s[1], s[2])
	}
	f.Fuzz(func(t *testing.T, limit, current, delta int64) {
		if limit < -1 || current < 0 || delta < 0 {
			t.Skip("not a quota measured against counts")
		}
		d, err := domain.QuotaValue(limit).Admits(current, delta)
		if err != nil {
			t.Fatalf("admits(limit %d, current %d, delta %d): %v", limit, current, delta, err)
		}
		if limit == -1 {
			if !d.Allowed || d.Limit != -1 || d.Remaining != -1 || d.Current != current {
				t.Fatalf("unlimited mishandled for current %d delta %d: %+v", current, delta, d)
			}
			return
		}
		sum := new(big.Int).Add(big.NewInt(current), big.NewInt(delta))
		fits := sum.Cmp(big.NewInt(limit)) <= 0
		if d.Allowed != fits {
			t.Fatalf("limit %d, current %d, delta %d: allowed = %v, the arithmetic says %v", limit, current, delta, d.Allowed, fits)
		}
		// Remaining is the room left once the delta is taken; a refusal leaves the room it found, never below zero.
		room := new(big.Int).Sub(big.NewInt(limit), big.NewInt(current))
		if fits {
			room.Sub(room, big.NewInt(delta))
		}
		if room.Sign() < 0 {
			room.SetInt64(0)
		}
		if d.Remaining != room.Int64() || d.Limit != limit || d.Current != current {
			t.Fatalf("limit %d, current %d, delta %d: %+v, want remaining %s", limit, current, delta, d, room)
		}
	})
}
