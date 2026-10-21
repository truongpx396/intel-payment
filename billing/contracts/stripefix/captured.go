package stripefix

import (
	"embed"
	"testing"
)

//go:embed captured/*.json
var captured embed.FS

// Captured is a delivery body exactly as the Stripe sandbox sent it to a real endpoint, with identities, links and keys scrubbed and the
// sandbox's own price ids replaced by the contract world's. These bodies were captured, not built from documentation: they
// are what the adapter has to read in production.
func Captured(t testing.TB, name string) []byte {
	t.Helper()
	b, err := captured.ReadFile("captured/" + name + ".json")
	if err != nil {
		t.Fatalf("no captured delivery %q: %v", name, err)
	}
	return b
}
