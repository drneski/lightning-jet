package ids

import (
	"bytes"
	"regexp"
	"testing"
	"time"
)

var uuidV7 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestIDFormatAndTimestamp(t *testing.T) {
	at := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	g := &Generator{Now: func() time.Time { return at }, Rand: bytes.NewReader(bytes.Repeat([]byte{0xff}, 10))}
	id := g.ID()
	if !uuidV7.MatchString(id) {
		t.Fatalf("ID() = %q, not a UUIDv7", id)
	}
	// The first 48 bits are the Unix time in milliseconds: 1792972800000.
	if got, want := id[:13], "01a17581-c000"; got != want {
		t.Errorf("timestamp prefix = %q, want %q", got, want)
	}
	if got, want := id, "01a17581-c000-7fff-bfff-ffffffffffff"; got != want {
		t.Errorf("ID() = %q, want %q", got, want)
	}
}

func TestIDsAreOrderedByTime(t *testing.T) {
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	g := &Generator{Now: func() time.Time { return now }, Rand: bytes.NewReader(make([]byte, 20))}
	first := g.ID()
	now = now.Add(time.Millisecond)
	second := g.ID()
	if !(first < second) {
		t.Errorf("IDs not ordered by time: %q then %q", first, second)
	}
}

func TestIDPanicsWhenRandomnessRunsOut(t *testing.T) {
	g := &Generator{Now: time.Now, Rand: bytes.NewReader(nil)}
	defer func() {
		if recover() == nil {
			t.Error("ID() did not panic on a failing source of randomness")
		}
	}()
	g.ID()
}
