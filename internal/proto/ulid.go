package proto

import (
	"crypto/rand"
	"time"
)

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewULID returns a 26-character ULID (48-bit millisecond timestamp,
// 80 random bits, Crockford base32). Client-generated per the at-most-once
// admission invariant.
func NewULID() string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	if _, err := rand.Read(b[6:]); err != nil {
		panic(err)
	}
	// 128 bits -> 26 base32 chars, reading 5 bits at a time from the top
	// (the leading char covers the 2 zero pad bits + top 3 timestamp bits).
	out := make([]byte, 26)
	for i := 0; i < 26; i++ {
		bitPos := i*5 - 2 // first char consumes only 3 data bits
		var v byte
		for j := 0; j < 5; j++ {
			bit := bitPos + j
			v <<= 1
			if bit >= 0 && bit < 128 && b[bit/8]&(1<<(7-bit%8)) != 0 {
				v |= 1
			}
		}
		out[i] = crockford[v]
	}
	return string(out)
}

// ValidULID reports whether s looks like a ULID (length and alphabet only).
func ValidULID(s string) bool {
	if len(s) != 26 {
		return false
	}
	for _, c := range s {
		ok := false
		for _, a := range crockford {
			if c == a {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}
