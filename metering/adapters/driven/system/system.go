// Package system holds the reference implementations of the two collaborators the core must not
// build for itself: the wall clock and the identifier source. The core reads no clock (a core that
// does is not replayable), so the real one lives here, in an adapter.
package system

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/truongpx396/intel-payment/metering/ports"
)

// Clock is the system clock.
type Clock struct{}

// Now returns the current time.
func (Clock) Now() time.Time { return time.Now() }

// IDs issues UUID v7 identifiers: a millisecond timestamp then random bits, so ids sort by time and
// index inserts stay local. Within one millisecond they are made strictly increasing.
type IDs struct {
	mu   sync.Mutex
	last uint64 // the last (ms << 12 | counter) issued
}

// NewID returns a new UUID v7.
func (g *IDs) NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("system: no entropy for an id: %v", err)) // a broken RNG must not yield colliding ids
	}
	g.mu.Lock()
	ms := uint64(time.Now().UnixMilli()) //nolint:gosec // a post-1970 clock is positive
	next := ms << 12
	if next <= g.last {
		next = g.last + 1 // same millisecond: bump the 12-bit counter so ids are strictly increasing
	}
	g.last = next
	ts, seq := next>>12, next&0xfff
	g.mu.Unlock()

	// Big-endian 48-bit timestamp, then the version nibble and the 12-bit counter. The masks make each
	// conversion provably lossless.
	b[0], b[1], b[2] = byte(ts>>40&0xff), byte(ts>>32&0xff), byte(ts>>24&0xff)
	b[3], b[4], b[5] = byte(ts>>16&0xff), byte(ts>>8&0xff), byte(ts&0xff)
	b[6] = 0x70 | byte(seq>>8&0x0f) // version 7
	b[7] = byte(seq & 0xff)
	b[8] = 0x80 | b[8]&0x3f // RFC 4122 variant
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

var (
	_ ports.Clock    = Clock{}
	_ ports.IDSource = (*IDs)(nil)
)
