package system_test

import (
	"regexp"
	"sort"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/truongpx396/intel-payment/metering/adapters/driven/system"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

var uuidV7 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestIDsAreUUIDv7AndStrictlyIncreasing(t *testing.T) {
	t.Parallel()
	var g system.IDs
	prev := ""
	for i := 0; i < 5000; i++ { // far more than one per millisecond: the counter must carry the order
		id := g.NewID()
		if !uuidV7.MatchString(id) {
			t.Fatalf("%q is not a v7 uuid", id)
		}
		if id <= prev {
			t.Fatalf("ids must sort by issue order so index inserts stay local: %q after %q", id, prev)
		}
		prev = id
	}
}

func TestIDsAreUniqueAcrossGoroutines(t *testing.T) {
	t.Parallel()
	var g system.IDs
	var mu sync.Mutex
	var all []string
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]string, 0, 1000)
			for i := 0; i < 1000; i++ {
				local = append(local, g.NewID())
			}
			mu.Lock()
			all = append(all, local...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Strings(all)
	for i := 1; i < len(all); i++ {
		if all[i] == all[i-1] {
			t.Fatalf("duplicate id %q", all[i])
		}
	}
}

func TestClockIsTheWallClock(t *testing.T) {
	t.Parallel()
	before := time.Now()
	got := system.Clock{}.Now()
	if got.Before(before) || time.Since(got) > time.Second {
		t.Fatalf("%v", got)
	}
}
