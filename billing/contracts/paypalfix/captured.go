package paypalfix

import (
	"bytes"
	"embed"
	"testing"
)

//go:embed captured/*.json
var captured embed.FS

// Captured is a delivery body as the PayPal sandbox's webhook simulator sent it to a real endpoint, with identities and links
// scrubbed. These bodies were captured, not built from documentation — but they are the simulator's, and the simulator sends
// the legacy v1 samples for some types (a refund is `PAYMENT.CAPTURE.REFUNDED` as a sale, not as the v2 refund the adapter
// reads), so they prove the envelope, the verification round trip and which types are read, not every field of a real payment.
// The file's closing newline is storage, not part of the delivery: PayPal's signature covers the bytes sent.
func Captured(t testing.TB, name string) []byte {
	t.Helper()
	b, err := captured.ReadFile("captured/" + name + ".json")
	if err != nil {
		t.Fatalf("no captured delivery %q: %v", name, err)
	}
	return bytes.TrimSpace(b)
}
