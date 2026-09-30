package redis_test

import (
	"os"
	"testing"

	hotredis "github.com/truongpx396/intel-payment/metering/adapters/driven/redis"
)

// The adapter loads the reference hot path UNCHANGED (T050). go:embed cannot reach outside the
// package, so the adapter carries a copy — and this test is what makes "unchanged" true: the copy
// and the contract's reference file must be byte-identical.
func TestEmbeddedLibraryIsTheReferenceFile(t *testing.T) {
	t.Parallel()
	ref, err := os.ReadFile("../../../../specs/001-metering-billing-core/contracts/reference/hot_path.lua")
	if err != nil {
		t.Fatal(err)
	}
	if string(ref) != hotredis.Library {
		t.Fatal("lua/hot_path.lua differs from specs/…/contracts/reference/hot_path.lua — edit the reference file, " +
			"copy it to metering/adapters/driven/redis/lua/hot_path.lua, and re-run scripts/verify-hot-path.sh")
	}
}
