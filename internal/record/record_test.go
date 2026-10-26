package record

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/lnd"
	"github.com/drneski/lightning-jet/v2/internal/lnd/lndtest"
	"github.com/drneski/lightning-jet/v2/internal/store"
)

func newRecorder(t *testing.T, f *lndtest.Fake) (*Recorder, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "jet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Recorder{LND: f, Store: st, Now: time.Now, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		MinBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond, AliveEvery: 10 * time.Millisecond}, st
}

// waitFor polls until cond holds or ten seconds pass.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func run(r *Recorder) (cancel func(), done chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	return cancel, done
}

func TestRecordsFailuresAndChannelEvents(t *testing.T) {
	now := time.Now().UTC()
	f := &lndtest.Fake{
		HtlcEvents: []lnd.HtlcEvent{
			{Kind: "subscribed"},
			{Time: now, Kind: "link_fail", EventType: "FORWARD", IncomingChanID: 2, OutgoingChanID: 1,
				Info: &lnd.HtlcInfo{OutgoingAmtMsat: 250_000_000}, WireFailure: "TEMPORARY_CHANNEL_FAILURE", FailureDetail: "INSUFFICIENT_BALANCE"},
			{Time: now, Kind: "forward", IncomingChanID: 2, OutgoingChanID: 1, Info: &lnd.HtlcInfo{OutgoingAmtMsat: 1}},
			{Time: now, Kind: "forward_fail", IncomingChanID: 2, OutgoingChanID: 1},
		},
		ChannelEvents: []lnd.ChannelEvent{{Type: "INACTIVE_CHANNEL", ChannelPoint: "ab:1"}},
	}
	r, st := newRecorder(t, f)
	ctx := context.Background()
	cancel, done := run(r)
	waitFor(t, "the failure to be recorded", func() bool {
		got, _ := st.BalanceFailuresSince(ctx, now.Add(-time.Hour))
		return got[1].Count == 1
	})
	waitFor(t, "both streams to be marked alive", func() bool {
		_, e1 := st.Meta(ctx, AliveKey(StreamHTLC))
		_, e2 := st.Meta(ctx, AliveKey(StreamChannels))
		return e1 == nil && e2 == nil
	})
	cancel()
	<-done

	got, _ := st.BalanceFailuresSince(ctx, now.Add(-time.Hour))
	if got[1].AmountMsat != 250_000_000 {
		t.Errorf("failures = %+v", got)
	}
	for _, stream := range []string{StreamHTLC, StreamChannels} {
		evs, err := st.StreamEvents(ctx, stream, now.Add(-time.Minute))
		if err != nil || len(evs) != 2 || evs[1].Event != store.StreamDisconnected || evs[1].Detail != "jet stopping" {
			t.Errorf("%s: events %+v, %v; want a connect, then a disconnect for the stop", stream, evs, err)
		}
		cov, _ := st.Coverage(ctx, stream, now.Add(-time.Minute), time.Now().UTC())
		if cov <= 0 {
			t.Errorf("%s: no coverage recorded", stream)
		}
	}
}

func TestReconnectsAfterTheStreamEnds(t *testing.T) {
	f := &lndtest.Fake{StreamEnd: errors.New("lnd restarted")}
	r, st := newRecorder(t, f)
	since := time.Now().Add(-time.Minute)
	cancel, done := run(r)
	waitFor(t, "three reconnects", func() bool {
		evs, _ := st.StreamEvents(context.Background(), StreamHTLC, since)
		return countEvents(evs, store.StreamConnected) >= 3
	})
	cancel()
	<-done
	evs, err := st.StreamEvents(context.Background(), StreamHTLC, since)
	if err != nil {
		t.Fatal(err)
	}
	// Every connect is followed by its disconnect, with the reason.
	for i := 0; i+1 < len(evs); i += 2 {
		if evs[i].Event != store.StreamConnected || evs[i+1].Event != store.StreamDisconnected {
			t.Fatalf("events out of order at %d: %+v", i, evs)
		}
	}
	if evs[1].Detail != "lnd restarted" {
		t.Errorf("first disconnect detail = %q", evs[1].Detail)
	}
	// Stopping during a session or between them leaves no session open.
	if last := evs[len(evs)-1]; last.Event != store.StreamDisconnected {
		t.Errorf("last event = %+v, want a disconnect", last)
	}
}

func countEvents(evs []store.StreamEvent, event string) int {
	n := 0
	for _, e := range evs {
		if e.Event == event {
			n++
		}
	}
	return n
}

func TestClosesASessionAPreviousRunLeftOpen(t *testing.T) {
	r, st := newRecorder(t, &lndtest.Fake{})
	ctx := context.Background()
	connected := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	alive := connected.Add(30 * time.Minute)
	if err := st.RecordStreamEvent(ctx, StreamHTLC, store.StreamConnected, connected, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMeta(ctx, AliveKey(StreamHTLC), strconv.FormatInt(alive.UnixMilli(), 10)); err != nil {
		t.Fatal(err)
	}
	cancel, done := run(r)
	waitFor(t, "a new session", func() bool {
		event, at, _ := st.LastStreamEvent(ctx, StreamHTLC)
		return event == store.StreamConnected && at.After(alive)
	})
	cancel()
	<-done
	// The old session is closed at its last sign of life: covered 30 minutes
	// of the hour before the new run, not the whole hour.
	cov, err := st.Coverage(ctx, StreamHTLC, connected, connected.Add(59*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if cov != 30*time.Minute {
		t.Errorf("coverage of the crashed session = %v, want 30m", cov)
	}
}
