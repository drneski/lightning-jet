package lnd

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// streamHandler writes each line as one chunk, as LND's REST streams do, then
// ends the response.
func streamHandler(path string, lines ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		for _, l := range lines {
			fmt.Fprintln(w, l)
			w.(http.Flusher).Flush()
		}
	}
}

func TestHtlcEvents(t *testing.T) {
	c := newTestClient(t, streamHandler("/v2/router/htlcevents",
		`{"result":{"incoming_channel_id":"0","outgoing_channel_id":"0","incoming_htlc_id":"0","outgoing_htlc_id":"0","timestamp_ns":"0","event_type":"UNKNOWN","subscribed_event":{}}}`,
		`{"result":{"incoming_channel_id":"222","outgoing_channel_id":"111","incoming_htlc_id":"7","outgoing_htlc_id":"0","timestamp_ns":"1792972800000000000","event_type":"FORWARD",
			"link_fail_event":{"info":{"incoming_timelock":900100,"outgoing_timelock":900060,"incoming_amt_msat":"250500000","outgoing_amt_msat":"250000000"},
			"wire_failure":"TEMPORARY_CHANNEL_FAILURE","failure_detail":"INSUFFICIENT_BALANCE","failure_string":"insufficient bandwidth to route htlc"}}}`,
		`{"result":{"incoming_channel_id":"222","outgoing_channel_id":"111","incoming_htlc_id":"8","outgoing_htlc_id":"3","timestamp_ns":"1792972801000000000","event_type":"FORWARD","forward_fail_event":{}}}`,
		`{"result":{"incoming_channel_id":"222","outgoing_channel_id":"111","incoming_htlc_id":"9","outgoing_htlc_id":"4","timestamp_ns":"1792972802000000000","event_type":"FORWARD","settle_event":{"preimage":"AA=="}}}`,
	))
	var got []HtlcEvent
	err := c.SubscribeHtlcEvents(context.Background(), func(ev HtlcEvent) error { got = append(got, ev); return nil })
	if err == nil || !strings.Contains(err.Error(), "stream closed") {
		t.Errorf("err = %v, want the stream closed", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d events", len(got))
	}
	kinds := []string{got[0].Kind, got[1].Kind, got[2].Kind, got[3].Kind}
	if strings.Join(kinds, ",") != "subscribed,link_fail,forward_fail,settle" {
		t.Errorf("kinds = %v", kinds)
	}
	f := got[1]
	if f.OutgoingChanID != 111 || f.IncomingChanID != 222 || f.IncomingHTLCID != 7 || f.EventType != "FORWARD" ||
		f.WireFailure != "TEMPORARY_CHANNEL_FAILURE" || f.FailureDetail != "INSUFFICIENT_BALANCE" ||
		f.Info == nil || f.Info.OutgoingAmtMsat != 250000000 || f.Info.IncomingTimelock != 900100 ||
		!f.Time.Equal(time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("link fail = %+v, info %+v", f, f.Info)
	}
}

func TestChannelEventsDecodeChannelPoints(t *testing.T) {
	// The txid 0102...20, sent as base64 of its bytes in reverse.
	rev := make([]byte, 32)
	for i := range rev {
		rev[i] = byte(32 - i)
	}
	b64 := base64.StdEncoding.EncodeToString(rev)
	c := newTestClient(t, streamHandler("/v1/channels/subscribe",
		fmt.Sprintf(`{"result":{"type":"INACTIVE_CHANNEL","inactive_channel":{"funding_txid_bytes":%q,"output_index":1}}}`, b64),
		`{"result":{"type":"CLOSED_CHANNEL","closed_channel":{"channel_point":"ab:0","chan_id":"111"}}}`,
	))
	var got []ChannelEvent
	_ = c.SubscribeChannelEvents(context.Background(), func(ev ChannelEvent) error { got = append(got, ev); return nil })
	if len(got) != 2 {
		t.Fatalf("got %d events", len(got))
	}
	want := "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20:1"
	if got[0].Type != "INACTIVE_CHANNEL" || got[0].ChannelPoint != want {
		t.Errorf("inactive = %+v, want point %s", got[0], want)
	}
	if got[1].Type != "CLOSED_CHANNEL" || got[1].ChannelPoint != "ab:0" || got[1].ChanID != 111 {
		t.Errorf("closed = %+v", got[1])
	}
}

func TestStreamErrorsAndCallbackErrors(t *testing.T) {
	c := newTestClient(t, streamHandler("/v1/channels/subscribe", `{"error":{"code":2,"message":"server shutting down"}}`))
	err := c.SubscribeChannelEvents(context.Background(), func(ChannelEvent) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "server shutting down") {
		t.Errorf("err = %v, want LND's message", err)
	}

	c = newTestClient(t, streamHandler("/v1/channels/subscribe", `{"result":{"type":"ACTIVE_CHANNEL","active_channel":{"funding_txid_str":"ab","output_index":0}}}`))
	stop := fmt.Errorf("enough")
	if err := c.SubscribeChannelEvents(context.Background(), func(ChannelEvent) error { return stop }); err != stop {
		t.Errorf("err = %v, want the callback's error", err)
	}
}

func TestStreamEndsWithItsContext(t *testing.T) {
	release := make(chan struct{})
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"result":{"type":"ACTIVE_CHANNEL","active_channel":{"funding_txid_str":"ab","output_index":0}}}`)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() {
		done <- c.SubscribeChannelEvents(ctx, func(ChannelEvent) error { cancel(); return nil })
	}()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stream outlived its context")
	}
}
