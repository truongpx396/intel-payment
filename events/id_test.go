package events_test

import (
	"bytes"
	"encoding/hex"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/events"
)

// A feed's cursor is an event id, so ids must sort in the order events became visible — across replicas whose clocks disagree.

func fixed(b ...byte) *bytes.Reader { return bytes.NewReader(append(b, make([]byte, 16)...)) }

func idInt(t testing.TB, id string) *big.Int {
	t.Helper()
	raw, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	if err != nil || len(raw) != 16 {
		t.Fatalf("%q is not a UUID", id)
	}
	return new(big.Int).SetBytes(raw)
}

func assertV7(t testing.TB, id string) {
	t.Helper()
	if len(id) != 36 || id[14] != '7' || !strings.ContainsRune("89ab", rune(id[19])) {
		t.Fatalf("%q is not a UUID v7 (version nibble, variant)", id)
	}
}

func TestAnIdIsAV7UUIDCarryingTheTimeInItsFirstFortyEightBits(t *testing.T) {
	t.Parallel()
	now := time.UnixMilli(1793872800123)
	id, err := events.NextID("", now, bytes.NewReader(bytes.Repeat([]byte{0xAB}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	assertV7(t, id)
	if got := idInt(t, id).Rsh(idInt(t, id), 80).Int64(); got != 1793872800123 {
		t.Fatalf("the timestamp is %d", got)
	}
	if !events.ValidID(id) || events.ValidID(strings.ToUpper(id)+"x") || events.ValidID("") || events.ValidID("not-a-uuid") {
		t.Fatal("ValidID")
	}
}

func TestAnIdSortsStrictlyAfterTheLastEvenWhenThisReplicasClockIsBehind(t *testing.T) {
	t.Parallel()
	now := time.UnixMilli(1793872800000)
	last, _ := events.NextID("", now, bytes.NewReader(bytes.Repeat([]byte{0xF0}, 16)))
	for name, at := range map[string]time.Time{
		"the same millisecond": now, "a clock a minute behind": now.Add(-time.Minute), "a clock at the epoch": time.UnixMilli(0), "a later clock": now.Add(time.Hour),
	} {
		next, err := events.NextID(last, at, bytes.NewReader(make([]byte, 16))) // the smallest entropy there is
		if err != nil {
			t.Fatal(err)
		}
		assertV7(t, next)
		if idInt(t, next).Cmp(idInt(t, last)) <= 0 {
			t.Errorf("%s: %s does not sort after %s", name, next, last)
		}
	}
}

func TestIdsIssuedOneAfterAnotherInTheSameMillisecondNeverRepeatOrReorder(t *testing.T) {
	t.Parallel()
	now := time.UnixMilli(1793872800000)
	last := ""
	for i := range 5000 {
		next, err := events.NextID(last, now, bytes.NewReader(make([]byte, 16)))
		if err != nil {
			t.Fatal(err)
		}
		if last != "" && idInt(t, next).Cmp(idInt(t, last)) <= 0 {
			t.Fatalf("id %d (%s) does not sort after %s", i, next, last)
		}
		assertV7(t, next)
		last = next
	}
}

func TestACarryRunsThroughTheVersionAndVariantBitsWithoutBreakingThem(t *testing.T) {
	t.Parallel()
	// All the free bits set: the next id must carry into the timestamp and still be a v7.
	last := "00000000-0001-7fff-bfff-ffffffffffff"
	next, err := events.NextID(last, time.UnixMilli(0), bytes.NewReader(make([]byte, 16)))
	if err != nil {
		t.Fatal(err)
	}
	if next != "00000000-0002-7000-8000-000000000000" {
		t.Fatalf("the carry gave %s", next)
	}
	// The top of the space has no successor.
	if _, err := events.NextID("ffffffff-ffff-7fff-bfff-ffffffffffff", time.UnixMilli(0), bytes.NewReader(make([]byte, 16))); err == nil {
		t.Fatal("an id after the last one that exists")
	}
}

func TestNextIDRefusesALastThatIsNotAUUIDAndEntropyThatRunsOut(t *testing.T) {
	t.Parallel()
	now := time.UnixMilli(1793872800000)
	for _, bad := range []string{"x", "0192f3c4-7b1e-7a2c-9d4e-5f6a7b8c9d0", "0192f3c47b1e7a2c9d4e5f6a7b8c9d0e0000", "0192f3c4-7b1e-7a2c-9d4e-5f6a7b8c9dzz"} {
		if _, err := events.NextID(bad, now, fixed()); err == nil {
			t.Errorf("last %q accepted", bad)
		}
	}
	if _, err := events.NextID("", now, bytes.NewReader([]byte{1, 2, 3})); err == nil {
		t.Error("short entropy accepted")
	}
}

// Held to math/big: whatever the last id is (made a valid v7), the next sorts strictly after it, is itself a v7, and is the candidate
// from now when that already sorts after.
func FuzzNextIDSortsStrictlyAfterTheLast(f *testing.F) {
	f.Add(make([]byte, 16), uint64(0), make([]byte, 16))
	f.Add(bytes.Repeat([]byte{0xFF}, 16), uint64(1793872800000), bytes.Repeat([]byte{0xFF}, 16))
	f.Add(bytes.Repeat([]byte{0x00}, 16), uint64(1<<47), bytes.Repeat([]byte{0x55}, 16))
	f.Fuzz(func(t *testing.T, lastRaw []byte, ms uint64, entropy []byte) {
		if len(lastRaw) < 16 || len(entropy) < 16 {
			t.Skip()
		}
		l := append([]byte(nil), lastRaw[:16]...)
		l[6] = 0x70 | l[6]&0x0F // a valid v7 layout
		l[8] = 0x80 | l[8]&0x3F
		hexs := hex.EncodeToString(l)
		last := hexs[0:8] + "-" + hexs[8:12] + "-" + hexs[12:16] + "-" + hexs[16:20] + "-" + hexs[20:]
		now := time.UnixMilli(int64(ms % (1 << 47)))
		next, err := events.NextID(last, now, bytes.NewReader(entropy[:16]))
		if err != nil {
			// The only refusal is the top of the id space.
			if idInt(t, last).Cmp(idInt(t, "ffffffff-ffff-7fff-bfff-ffffffffffff")) != 0 {
				t.Fatalf("refused after %s: %v", last, err)
			}
			return
		}
		assertV7(t, next)
		if idInt(t, next).Cmp(idInt(t, last)) <= 0 {
			t.Fatalf("%s does not sort after %s", next, last)
		}
		cand, err := events.NextID("", now, bytes.NewReader(entropy[:16]))
		if err != nil {
			t.Fatal(err)
		}
		if idInt(t, cand).Cmp(idInt(t, last)) > 0 && next != cand {
			t.Fatalf("the candidate %s already sorts after %s, and %s was issued instead", cand, last, next)
		}
	})
}
