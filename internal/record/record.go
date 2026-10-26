// Package record writes LND's event streams to the store as they happen:
// failed HTLCs and channel events, which LND won't report again later. It
// reconnects when a stream drops, and records every connect and disconnect,
// so a gap in the record is known rather than silent.
package record

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
	"github.com/drneski/lightning-jet/v2/internal/lnd"
	"github.com/drneski/lightning-jet/v2/internal/store"
)

// The streams, as named in the store.
const (
	StreamHTLC     = "htlc"
	StreamChannels = "channels"
)

// AliveKey is the meta key holding when a stream was last known to be
// connected, as Unix milliseconds.
func AliveKey(stream string) string { return "stream." + stream + ".alive_at" }

// Recorder records LND's streams.
type Recorder struct {
	LND   lnd.Streamer
	Store *store.Store
	Now   func() time.Time
	Log   *slog.Logger
	// MinBackoff and MaxBackoff bound the wait before reconnecting.
	MinBackoff, MaxBackoff time.Duration
	// AliveEvery is how often a connected stream marks itself alive.
	AliveEvery time.Duration
}

// Run records both streams until ctx ends.
func (r *Recorder) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		r.run(ctx, StreamHTLC, func(c context.Context) error {
			return r.LND.SubscribeHtlcEvents(c, func(ev lnd.HtlcEvent) error { return r.htlc(c, ev) })
		})
	}()
	go func() {
		defer wg.Done()
		r.run(ctx, StreamChannels, func(c context.Context) error {
			return r.LND.SubscribeChannelEvents(c, func(ev lnd.ChannelEvent) error { return r.channel(c, ev) })
		})
	}()
	wg.Wait()
}

func (r *Recorder) htlc(ctx context.Context, ev lnd.HtlcEvent) error {
	if ev.Kind != "link_fail" && ev.Kind != "forward_fail" {
		return nil // successful forwards come from LND's forwarding history
	}
	now := r.Now().UTC()
	f := store.HTLCFailure{
		At: ev.Time, Kind: ev.Kind, EventType: ev.EventType,
		IncomingChanID: ev.IncomingChanID, OutgoingChanID: ev.OutgoingChanID,
		IncomingHTLCID: ev.IncomingHTLCID, OutgoingHTLCID: ev.OutgoingHTLCID,
		WireFailure: ev.WireFailure, FailureDetail: ev.FailureDetail, FailureString: ev.FailureString,
		RecordedAt: now,
	}
	if f.At.IsZero() || f.At.Unix() <= 0 {
		f.At = now
	}
	if i := ev.Info; i != nil {
		f.IncomingAmtMsat, f.OutgoingAmtMsat = msat(i.IncomingAmtMsat), msat(i.OutgoingAmtMsat)
		f.IncomingTimelock, f.OutgoingTimelock = i.IncomingTimelock, i.OutgoingTimelock
	}
	return r.Store.SaveHTLCFailure(ctx, f)
}

func msat(v int64) contracts.MilliSat { return contracts.MilliSat(v) }

func (r *Recorder) channel(ctx context.Context, ev lnd.ChannelEvent) error {
	return r.Store.SaveChannelEvent(ctx, store.ChannelEvent{At: r.Now().UTC(), Type: ev.Type, ChannelPoint: ev.ChannelPoint, ChanID: ev.ChanID})
}

func (r *Recorder) run(ctx context.Context, stream string, subscribe func(context.Context) error) {
	log := r.Log.With("stream", stream)
	// Recording must outlive cancellation, so a stop is written down too.
	bg := context.WithoutCancel(ctx)
	r.closeUnended(bg, stream, log)
	backoff := r.MinBackoff
	for ctx.Err() == nil {
		began := r.Now().UTC()
		if err := r.Store.RecordStreamEvent(bg, stream, store.StreamConnected, began, ""); err != nil {
			log.Error("recording a connect", "err", err)
		}
		session, stop := context.WithCancel(ctx)
		go r.markAlive(session, stream)
		err := subscribe(session)
		stop()
		ended := r.Now().UTC()
		detail := "stream ended"
		if err != nil {
			detail = err.Error()
		}
		if ctx.Err() != nil {
			detail = "jet stopping"
		}
		if rerr := r.Store.RecordStreamEvent(bg, stream, store.StreamDisconnected, ended, detail); rerr != nil {
			log.Error("recording a disconnect", "err", rerr)
		}
		if ctx.Err() != nil {
			return
		}
		if ended.Sub(began) > r.MaxBackoff {
			backoff = r.MinBackoff // it had been healthy, so start over
		}
		log.Warn("stream disconnected; reconnecting", "err", detail, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, r.MaxBackoff)
	}
}

// closeUnended records the disconnect a previous run never got to write, at
// the last time the stream was known to be alive.
func (r *Recorder) closeUnended(ctx context.Context, stream string, log *slog.Logger) {
	event, connectedAt, err := r.Store.LastStreamEvent(ctx, stream)
	if err != nil || event != store.StreamConnected {
		return
	}
	at := connectedAt
	if v, err := r.Store.Meta(ctx, AliveKey(stream)); err == nil {
		if msAlive, err := strconv.ParseInt(v, 10, 64); err == nil && time.UnixMilli(msAlive).After(at) {
			at = time.UnixMilli(msAlive).UTC()
		}
	}
	if err := r.Store.RecordStreamEvent(ctx, stream, store.StreamDisconnected, at, "jet stopped without closing the stream; recorded on the next start"); err != nil {
		log.Error("closing the previous session", "err", err)
	}
}

func (r *Recorder) markAlive(ctx context.Context, stream string) {
	t := time.NewTicker(r.AliveEvery)
	defer t.Stop()
	for {
		if err := r.Store.SetMeta(context.WithoutCancel(ctx), AliveKey(stream), strconv.FormatInt(r.Now().UnixMilli(), 10)); err != nil {
			r.Log.Error("marking the stream alive", "stream", stream, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
