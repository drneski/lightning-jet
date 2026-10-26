// Package lndtest provides a fake LND for tests: a Reader that serves
// synthetic data from memory. Never put real node data in it.
package lndtest

import (
	"context"
	"fmt"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
	"github.com/drneski/lightning-jet/v2/internal/lnd"
)

// Fake is an in-memory Reader and Streamer. A non-nil entry in Errors makes
// that method fail with it.
//
// Each subscription delivers its events in order, then fails with
// StreamEnd if set, or else waits for its context to end.
type Fake struct {
	Info          lnd.Info
	Channels      []lnd.Channel
	Edges         map[contracts.ChanID]lnd.Edge
	Forwards      []lnd.Forward
	HtlcEvents    []lnd.HtlcEvent
	ChannelEvents []lnd.ChannelEvent
	StreamEnd     error
	Errors        map[string]error
}

var (
	_ lnd.Reader   = (*Fake)(nil)
	_ lnd.Streamer = (*Fake)(nil)
)

func (f *Fake) fail(method string) error {
	if err := f.Errors[method]; err != nil {
		return err
	}
	return nil
}

// GetInfo implements lnd.Reader.
func (f *Fake) GetInfo(ctx context.Context) (lnd.Info, error) {
	if err := f.fail("GetInfo"); err != nil {
		return lnd.Info{}, err
	}
	return f.Info, ctx.Err()
}

// ListChannels implements lnd.Reader.
func (f *Fake) ListChannels(ctx context.Context) ([]lnd.Channel, error) {
	if err := f.fail("ListChannels"); err != nil {
		return nil, err
	}
	return append([]lnd.Channel(nil), f.Channels...), ctx.Err()
}

// GetChanInfo implements lnd.Reader.
func (f *Fake) GetChanInfo(ctx context.Context, id contracts.ChanID) (lnd.Edge, error) {
	if err := f.fail("GetChanInfo"); err != nil {
		return lnd.Edge{}, err
	}
	e, ok := f.Edges[id]
	if !ok {
		return lnd.Edge{}, fmt.Errorf("edge %s not found", id)
	}
	return e, ctx.Err()
}

// ForwardingHistory implements lnd.Reader.
func (f *Fake) ForwardingHistory(ctx context.Context, start, end time.Time) ([]lnd.Forward, error) {
	if err := f.fail("ForwardingHistory"); err != nil {
		return nil, err
	}
	var out []lnd.Forward
	for _, fw := range f.Forwards {
		if !fw.Time.Before(start) && fw.Time.Before(end) {
			out = append(out, fw)
		}
	}
	return out, ctx.Err()
}

// SubscribeHtlcEvents implements lnd.Streamer.
func (f *Fake) SubscribeHtlcEvents(ctx context.Context, fn func(lnd.HtlcEvent) error) error {
	if err := f.fail("SubscribeHtlcEvents"); err != nil {
		return err
	}
	for _, ev := range f.HtlcEvents {
		if err := fn(ev); err != nil {
			return err
		}
	}
	return f.end(ctx)
}

// SubscribeChannelEvents implements lnd.Streamer.
func (f *Fake) SubscribeChannelEvents(ctx context.Context, fn func(lnd.ChannelEvent) error) error {
	if err := f.fail("SubscribeChannelEvents"); err != nil {
		return err
	}
	for _, ev := range f.ChannelEvents {
		if err := fn(ev); err != nil {
			return err
		}
	}
	return f.end(ctx)
}

func (f *Fake) end(ctx context.Context) error {
	if f.StreamEnd != nil {
		return f.StreamEnd
	}
	<-ctx.Done()
	return ctx.Err()
}
