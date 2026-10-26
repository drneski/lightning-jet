package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBalanceFailuresSince(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	fail := func(at time.Time, out uint64, detail string, amt int64) {
		t.Helper()
		if err := s.SaveHTLCFailure(ctx, HTLCFailure{At: at, Kind: "link_fail", EventType: "FORWARD", IncomingChanID: 2, OutgoingChanID: chanID(out),
			OutgoingAmtMsat: msat(amt), WireFailure: "TEMPORARY_CHANNEL_FAILURE", FailureDetail: detail, RecordedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	fail(t0.Add(-time.Hour), 1, "INSUFFICIENT_BALANCE", 250_000_000)
	fail(t0.Add(-2*time.Hour), 1, "INSUFFICIENT_BALANCE", 100_000_000)
	fail(t0.Add(-3*time.Hour), 1, "HTLC_EXCEEDS_MAX", 9_000_000_000)         // not a balance failure
	fail(t0.Add(-40*24*time.Hour), 1, "INSUFFICIENT_BALANCE", 1_000_000_000) // before the window
	if err := s.SaveHTLCFailure(ctx, HTLCFailure{At: t0, Kind: "forward_fail", EventType: "FORWARD", IncomingChanID: 2, OutgoingChanID: 1, RecordedAt: t0}); err != nil {
		t.Fatal(err)
	}

	got, err := s.BalanceFailuresSince(ctx, t0.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if f := got[1]; f.Count != 2 || f.AmountMsat != 350_000_000 || len(got) != 1 {
		t.Errorf("balance failures = %+v", got)
	}
}

func TestCoverageCountsOnlyRecordedTime(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	ev := func(event string, at time.Time) {
		t.Helper()
		if err := s.RecordStreamEvent(ctx, "htlc", event, at, ""); err != nil {
			t.Fatal(err)
		}
	}
	since := t0
	ev(StreamConnected, t0.Add(-time.Hour)) // started before the window
	ev(StreamDisconnected, t0.Add(2*time.Hour))
	ev(StreamConnected, t0.Add(3*time.Hour))
	ev(StreamConnected, t0.Add(4*time.Hour)) // restarted without a disconnect: 3h-4h counts
	ev(StreamDisconnected, t0.Add(5*time.Hour))
	ev(StreamConnected, t0.Add(6*time.Hour)) // still connected at now
	now := t0.Add(8 * time.Hour)

	got, err := s.Coverage(ctx, "htlc", since, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := 2*time.Hour + time.Hour + time.Hour + 2*time.Hour; got != want {
		t.Errorf("coverage = %v, want %v", got, want)
	}
	if none, _ := s.Coverage(ctx, "channels", since, now); none != 0 {
		t.Errorf("a stream never recorded has coverage %v", none)
	}

	event, at, err := s.LastStreamEvent(ctx, "htlc")
	if err != nil || event != StreamConnected || !at.Equal(t0.Add(6*time.Hour)) {
		t.Errorf("last event = %s at %v, %v", event, at, err)
	}
	if _, _, err := s.LastStreamEvent(ctx, "channels"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown stream: err = %v", err)
	}
}

func TestChannelEventsAndMeta(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	if err := s.SaveChannelEvent(ctx, ChannelEvent{At: t0, Type: "INACTIVE_CHANNEL", ChannelPoint: "ab:1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Meta(ctx, "stream.htlc.alive_at"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unset meta: err = %v", err)
	}
	for _, v := range []string{"1", "2"} {
		if err := s.SetMeta(ctx, "stream.htlc.alive_at", v); err != nil {
			t.Fatal(err)
		}
	}
	if v, err := s.Meta(ctx, "stream.htlc.alive_at"); err != nil || v != "2" {
		t.Errorf("meta = %q, %v", v, err)
	}
}
