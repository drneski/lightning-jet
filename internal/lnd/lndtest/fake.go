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

// Fake is an in-memory Reader. A non-nil entry in Errors makes that method
// fail with it.
type Fake struct {
	Info     lnd.Info
	Channels []lnd.Channel
	Edges    map[contracts.ChanID]lnd.Edge
	Forwards []lnd.Forward
	Errors   map[string]error
}

var _ lnd.Reader = (*Fake)(nil)

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
