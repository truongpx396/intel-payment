package events

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"time"
)

// The bits of a UUID v7 that are not free: the version nibble and the variant.
const (
	fixedHi  = uint64(0xF000) // the version nibble, in the high half
	versionH = uint64(0x7000)
	fixedLo  = uint64(0xC000000000000000) // the variant, in the low half
	variantL = uint64(0x8000000000000000)
)

// NextID returns a UUID v7 for now that sorts strictly after last. last is the newest id in the log ("" for an empty one), read under
// the lock the log is appended under, so that two replicas whose clocks disagree still issue ids in the order events become visible —
// which is what lets a feed's cursor be an id and never skip an event. The ids of one millisecond are ordered too: when the candidate
// would not sort after last it becomes last plus one, counted over the bits a v7 leaves free.
func NextID(last string, now time.Time, entropy io.Reader) (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(entropy, b[:]); err != nil {
		return "", fmt.Errorf("events: no entropy for an id: %w", err)
	}
	ms := uint64(now.UnixMilli()) //nolint:gosec // a post-1970 clock is positive
	hi := (ms&0xFFFFFFFFFFFF)<<16 | versionH | uint64(binary.BigEndian.Uint16(b[6:8]))&^fixedHi
	lo := binary.BigEndian.Uint64(b[8:16])&^fixedLo | variantL
	if last != "" {
		lh, ll, err := parseUUID(last)
		if err != nil {
			return "", err
		}
		if hi < lh || hi == lh && lo <= ll {
			// last plus one over the free bits: set the fixed ones so the carry runs through them, add, and put them back.
			ll |= fixedLo
			lh |= fixedHi
			var carry uint64
			lo, carry = bits.Add64(ll, 1, 0)
			hi, carry = bits.Add64(lh, 0, carry)
			if carry != 0 {
				return "", errors.New("events: the id space is exhausted")
			}
			hi = hi&^fixedHi | versionH
			lo = lo&^fixedLo | variantL
		}
	}
	var out [16]byte
	binary.BigEndian.PutUint64(out[:8], hi)
	binary.BigEndian.PutUint64(out[8:], lo)
	h := hex.EncodeToString(out[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

func parseUUID(s string) (hi, lo uint64, err error) {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return 0, 0, fmt.Errorf("%w: %q is not a UUID", ErrInvalid, s)
	}
	raw, err := hex.DecodeString(s[0:8] + s[9:13] + s[14:18] + s[19:23] + s[24:])
	if err != nil {
		return 0, 0, fmt.Errorf("%w: %q is not a UUID", ErrInvalid, s)
	}
	return binary.BigEndian.Uint64(raw[:8]), binary.BigEndian.Uint64(raw[8:]), nil
}

// ValidID reports whether s is the text of a UUID, the only shape of cursor a feed accepts.
func ValidID(s string) bool {
	_, _, err := parseUUID(s)
	return err == nil
}
