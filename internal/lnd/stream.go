package lnd

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
)

// Streamer subscribes to LND's event streams. Each call blocks, handing every
// event to fn, until ctx ends, the stream fails, or fn returns an error. It
// never returns nil: an ended stream is an error the caller reconnects from.
type Streamer interface {
	SubscribeHtlcEvents(ctx context.Context, fn func(HtlcEvent) error) error
	SubscribeChannelEvents(ctx context.Context, fn func(ChannelEvent) error) error
}

var _ Streamer = (*Client)(nil)

// HtlcEvent is one event from LND's HTLC stream. LND's events carry no payment
// hash: only its HTLC interceptor does, and that holds every forward until it
// is answered, so a read-only observer can't use it.
type HtlcEvent struct {
	Time           time.Time
	IncomingChanID contracts.ChanID
	OutgoingChanID contracts.ChanID
	IncomingHTLCID uint64
	OutgoingHTLCID uint64
	// EventType is LND's: FORWARD, SEND, RECEIVE or UNKNOWN.
	EventType string
	// Kind is which event it is: forward, forward_fail, settle, link_fail,
	// final_htlc or subscribed.
	Kind string
	// Info is set for forward and link_fail events.
	Info *HtlcInfo
	// The failure, for link_fail events.
	WireFailure   string
	FailureDetail string
	FailureString string
}

// HtlcInfo is an HTLC's amounts and timelocks on both sides.
type HtlcInfo struct {
	IncomingTimelock uint32
	OutgoingTimelock uint32
	IncomingAmtMsat  int64
	OutgoingAmtMsat  int64
}

// ChannelEvent is one event from LND's channel stream. LND's channel events
// carry no timestamp; whoever records one stamps it when it arrives.
type ChannelEvent struct {
	// Type is LND's: ACTIVE_CHANNEL, INACTIVE_CHANNEL, OPEN_CHANNEL,
	// CLOSED_CHANNEL, PENDING_OPEN_CHANNEL, FULLY_RESOLVED_CHANNEL and others.
	Type string
	// ChannelPoint is funding txid:output index, when the event names one.
	ChannelPoint string
	// ChanID is set when the event carries it (open and closed channels).
	ChanID contracts.ChanID
}

// stream reads a server-streaming REST response: newline-delimited JSON
// objects, each {"result": ...} or {"error": ...}.
func (c *Client) stream(ctx context.Context, path string, fn func(json.RawMessage) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Grpc-Metadata-macaroon", c.macaroon)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("lnd %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &e) == nil && e.Message != "" {
			return fmt.Errorf("lnd %s: %s (HTTP %d)", path, e.Message, resp.StatusCode)
		}
		return fmt.Errorf("lnd %s: HTTP %d", path, resp.StatusCode)
	}
	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct {
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("lnd %s: stream closed by LND", path)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("lnd %s: reading stream: %w", path, err)
		}
		if msg.Error != nil {
			return fmt.Errorf("lnd %s: %s", path, msg.Error.Message)
		}
		if err := fn(msg.Result); err != nil {
			return err
		}
	}
}

type wireHtlcInfo struct {
	IncomingTimelock uint32  `json:"incoming_timelock"`
	OutgoingTimelock uint32  `json:"outgoing_timelock"`
	IncomingAmtMsat  wireInt `json:"incoming_amt_msat"`
	OutgoingAmtMsat  wireInt `json:"outgoing_amt_msat"`
}

func (w *wireHtlcInfo) info() *HtlcInfo {
	if w == nil {
		return nil
	}
	return &HtlcInfo{IncomingTimelock: w.IncomingTimelock, OutgoingTimelock: w.OutgoingTimelock,
		IncomingAmtMsat: int64(w.IncomingAmtMsat), OutgoingAmtMsat: int64(w.OutgoingAmtMsat)}
}

// SubscribeHtlcEvents implements Streamer.
func (c *Client) SubscribeHtlcEvents(ctx context.Context, fn func(HtlcEvent) error) error {
	return c.stream(ctx, "/v2/router/htlcevents", func(raw json.RawMessage) error {
		var w struct {
			IncomingChannelID wireUint `json:"incoming_channel_id"`
			OutgoingChannelID wireUint `json:"outgoing_channel_id"`
			IncomingHTLCID    wireUint `json:"incoming_htlc_id"`
			OutgoingHTLCID    wireUint `json:"outgoing_htlc_id"`
			TimestampNs       wireInt  `json:"timestamp_ns"`
			EventType         string   `json:"event_type"`
			ForwardEvent      *struct {
				Info *wireHtlcInfo `json:"info"`
			} `json:"forward_event"`
			ForwardFailEvent json.RawMessage `json:"forward_fail_event"`
			SettleEvent      json.RawMessage `json:"settle_event"`
			FinalHtlcEvent   json.RawMessage `json:"final_htlc_event"`
			SubscribedEvent  json.RawMessage `json:"subscribed_event"`
			LinkFailEvent    *struct {
				Info          *wireHtlcInfo `json:"info"`
				WireFailure   string        `json:"wire_failure"`
				FailureDetail string        `json:"failure_detail"`
				FailureString string        `json:"failure_string"`
			} `json:"link_fail_event"`
		}
		if err := json.Unmarshal(raw, &w); err != nil {
			return fmt.Errorf("lnd htlc event: %w", err)
		}
		ev := HtlcEvent{
			Time:           time.Unix(0, int64(w.TimestampNs)).UTC(),
			IncomingChanID: contracts.ChanID(w.IncomingChannelID), OutgoingChanID: contracts.ChanID(w.OutgoingChannelID),
			IncomingHTLCID: uint64(w.IncomingHTLCID), OutgoingHTLCID: uint64(w.OutgoingHTLCID),
			EventType: w.EventType,
		}
		switch {
		case w.LinkFailEvent != nil:
			ev.Kind, ev.Info = "link_fail", w.LinkFailEvent.Info.info()
			ev.WireFailure, ev.FailureDetail, ev.FailureString = w.LinkFailEvent.WireFailure, w.LinkFailEvent.FailureDetail, w.LinkFailEvent.FailureString
		case w.ForwardEvent != nil:
			ev.Kind, ev.Info = "forward", w.ForwardEvent.Info.info()
		case present(w.ForwardFailEvent):
			ev.Kind = "forward_fail"
		case present(w.SettleEvent):
			ev.Kind = "settle"
		case present(w.FinalHtlcEvent):
			ev.Kind = "final_htlc"
		case present(w.SubscribedEvent):
			ev.Kind = "subscribed"
		default:
			ev.Kind = "unknown"
		}
		return fn(ev)
	})
}

func present(raw json.RawMessage) bool { return len(raw) > 0 && string(raw) != "null" }

type wireChannelPoint struct {
	FundingTxidBytes string `json:"funding_txid_bytes"`
	FundingTxidStr   string `json:"funding_txid_str"`
	OutputIndex      uint32 `json:"output_index"`
}

// point returns txid:index. LND sends the txid as base64 bytes in the
// reverse of the order a txid is written in.
func (p *wireChannelPoint) point() (string, error) {
	if p == nil {
		return "", nil
	}
	txid := p.FundingTxidStr
	if txid == "" {
		b, err := base64.StdEncoding.DecodeString(p.FundingTxidBytes)
		if err != nil {
			return "", fmt.Errorf("lnd channel point: %w", err)
		}
		for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
			b[i], b[j] = b[j], b[i]
		}
		txid = hex.EncodeToString(b)
	}
	return txid + ":" + strconv.FormatUint(uint64(p.OutputIndex), 10), nil
}

// SubscribeChannelEvents implements Streamer.
func (c *Client) SubscribeChannelEvents(ctx context.Context, fn func(ChannelEvent) error) error {
	return c.stream(ctx, "/v1/channels/subscribe", func(raw json.RawMessage) error {
		var w struct {
			Type                 string            `json:"type"`
			ActiveChannel        *wireChannelPoint `json:"active_channel"`
			InactiveChannel      *wireChannelPoint `json:"inactive_channel"`
			FullyResolvedChannel *wireChannelPoint `json:"fully_resolved_channel"`
			OpenChannel          *struct {
				ChannelPoint string   `json:"channel_point"`
				ChanID       wireUint `json:"chan_id"`
			} `json:"open_channel"`
			ClosedChannel *struct {
				ChannelPoint string   `json:"channel_point"`
				ChanID       wireUint `json:"chan_id"`
			} `json:"closed_channel"`
			PendingOpenChannel *struct {
				Txid        string `json:"txid"`
				OutputIndex uint32 `json:"output_index"`
			} `json:"pending_open_channel"`
		}
		if err := json.Unmarshal(raw, &w); err != nil {
			return fmt.Errorf("lnd channel event: %w", err)
		}
		ev := ChannelEvent{Type: w.Type}
		var err error
		switch {
		case w.ActiveChannel != nil:
			ev.ChannelPoint, err = w.ActiveChannel.point()
		case w.InactiveChannel != nil:
			ev.ChannelPoint, err = w.InactiveChannel.point()
		case w.FullyResolvedChannel != nil:
			ev.ChannelPoint, err = w.FullyResolvedChannel.point()
		case w.OpenChannel != nil:
			ev.ChannelPoint, ev.ChanID = w.OpenChannel.ChannelPoint, contracts.ChanID(w.OpenChannel.ChanID)
		case w.ClosedChannel != nil:
			ev.ChannelPoint, ev.ChanID = w.ClosedChannel.ChannelPoint, contracts.ChanID(w.ClosedChannel.ChanID)
		case w.PendingOpenChannel != nil:
			p := &wireChannelPoint{FundingTxidBytes: w.PendingOpenChannel.Txid, OutputIndex: w.PendingOpenChannel.OutputIndex}
			ev.ChannelPoint, err = p.point()
		}
		if err != nil {
			return err
		}
		return fn(ev)
	})
}
