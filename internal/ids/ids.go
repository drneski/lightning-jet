// Package ids makes the identifiers Jet gives its records: UUIDv7 (RFC 9562),
// which are unique and ordered by the millisecond they were made.
package ids

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"time"
)

// Generator makes UUIDv7 strings. Its clock and source of randomness are
// injectable, so tests can make deterministic identifiers.
type Generator struct {
	Now  func() time.Time
	Rand io.Reader
}

// New returns a generator on the system clock and crypto/rand.
func New() *Generator {
	return &Generator{Now: time.Now, Rand: rand.Reader}
}

// ID returns a new UUIDv7 in its canonical text form. Identifiers made in the
// same millisecond are unique but not ordered among themselves. It panics if
// the source of randomness fails, which crypto/rand never does.
func (g *Generator) ID() string {
	var b [16]byte
	if _, err := io.ReadFull(g.Rand, b[6:]); err != nil {
		panic("ids: reading randomness: " + err.Error())
	}
	ms := uint64(g.Now().UnixMilli())
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (40 - 8*i))
	}
	b[6] = 0x70 | b[6]&0x0f // version 7
	b[8] = 0x80 | b[8]&0x3f // variant 10

	var s [36]byte
	hex.Encode(s[0:8], b[0:4])
	s[8] = '-'
	hex.Encode(s[9:13], b[4:6])
	s[13] = '-'
	hex.Encode(s[14:18], b[6:8])
	s[18] = '-'
	hex.Encode(s[19:23], b[8:10])
	s[23] = '-'
	hex.Encode(s[24:36], b[10:16])
	return string(s[:])
}
