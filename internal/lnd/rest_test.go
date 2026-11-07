package lnd

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The responses below are synthetic, in the shape of LND's REST API: 64-bit
// integers as strings, 32-bit ones as numbers, proto field names.
const (
	fakeMacaroon = "\x02\x01\x03lnd" // any bytes; the client sends them hex-encoded
	nodePub      = "02aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	peerPub      = "03bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	cert := filepath.Join(dir, "tls.cert")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	mac := filepath.Join(dir, "readonly.macaroon")
	if err := os.WriteFile(mac, []byte(fakeMacaroon), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(Config{Address: srv.Listener.Addr().String(), TLSCertPath: cert, MacaroonPath: mac})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func requireMacaroon(t *testing.T, r *http.Request) {
	t.Helper()
	if got, want := r.Header.Get("Grpc-Metadata-macaroon"), "0201036c6e64"; got != want {
		t.Errorf("macaroon header = %q, want %q", got, want)
	}
}

func TestGetInfoAndChannels(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requireMacaroon(t, r)
		switch r.URL.Path {
		case "/v1/getinfo":
			fmt.Fprintf(w, `{"identity_pubkey":%q,"alias":"synthetic","version":"0.18.0-beta","block_height":900000,"synced_to_chain":true,"synced_to_graph":false}`, nodePub)
		case "/v1/channels":
			fmt.Fprintf(w, `{"channels":[{"active":true,"remote_pubkey":%q,"channel_point":"aa:1","chan_id":"111111111111111111","capacity":"5000000","local_balance":"4500000","remote_balance":"480000","private":false}]}`, peerPub)
		default:
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()
	info, err := c.GetInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.IdentityPubkey != nodePub || info.BlockHeight != 900000 || !info.SyncedToChain || info.SyncedToGraph {
		t.Errorf("GetInfo() = %+v", info)
	}
	chans, err := c.ListChannels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(chans) != 1 || chans[0].ChanID != 111111111111111111 || chans[0].CapacitySat != 5000000 ||
		chans[0].LocalSat != 4500000 || chans[0].RemotePubkey != peerPub || !chans[0].Active {
		t.Errorf("ListChannels() = %+v", chans)
	}
}

func TestGetChanInfoWithAMissingPolicy(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/graph/edge/111111111111111111" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"channel_id":"111111111111111111","node1_pub":%q,"node2_pub":%q,
			"node1_policy":{"time_lock_delta":80,"min_htlc":"1000","fee_base_msat":"0","fee_rate_milli_msat":"500","disabled":false,"max_htlc_msat":"4950000000","last_update":1792900000},
			"node2_policy":null}`, nodePub, peerPub)
	})
	e, err := c.GetChanInfo(context.Background(), 111111111111111111)
	if err != nil {
		t.Fatal(err)
	}
	if e.Node1Policy == nil || e.Node1Policy.FeeRateMilliMsat != 500 || e.Node1Policy.TimeLockDelta != 80 ||
		e.Node1Policy.MaxHTLCMsat != 4950000000 || !e.Node1Policy.LastUpdate.Equal(time.Unix(1792900000, 0)) {
		t.Errorf("node1 policy = %+v", e.Node1Policy)
	}
	if e.Node2Policy != nil {
		t.Errorf("a null policy decoded as %+v", e.Node2Policy)
	}
}

func TestForwardingHistoryPages(t *testing.T) {
	start := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	end := start.Add(30 * 24 * time.Hour)
	var offsets []float64
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/switch" {
			http.NotFound(w, r)
			return
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req["start_time"] != fmt.Sprint(start.Unix()) || req["end_time"] != fmt.Sprint(end.Unix()) {
			t.Errorf("request = %v", req)
		}
		off := req["index_offset"].(float64)
		offsets = append(offsets, off)
		n := forwardsPageSize // a full first page forces a second request
		if off > 0 {
			n = 3
		}
		events := make([]string, n)
		for i := range events {
			ts := start.Add(time.Duration(int(off)+i) * time.Second).UnixNano()
			events[i] = fmt.Sprintf(`{"timestamp_ns":"%d","chan_id_in":"222","chan_id_out":"111111111111111111","amt_in_msat":"100500","amt_out_msat":"100000","fee_msat":"500"}`, ts)
		}
		fmt.Fprintf(w, `{"forwarding_events":[%s],"last_offset_index":%d}`, strings.Join(events, ","), int(off)+n)
	})
	fws, err := c.ForwardingHistory(context.Background(), start, end)
	if err != nil {
		t.Fatal(err)
	}
	if len(fws) != forwardsPageSize+3 {
		t.Errorf("got %d forwards, want %d", len(fws), forwardsPageSize+3)
	}
	if len(offsets) != 2 || offsets[1] != forwardsPageSize {
		t.Errorf("offsets requested = %v", offsets)
	}
	if f := fws[0]; f.ChanIDOut != 111111111111111111 || f.FeeMsat != 500 || f.AmtOutMsat != 100000 || !f.Time.Equal(start) {
		t.Errorf("first forward = %+v", f)
	}
}

func TestPaymentsPages(t *testing.T) {
	since := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	var offsets []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/v1/payments" || q.Get("include_incomplete") != "true" || q.Get("creation_date_start") != fmt.Sprint(since.Unix()) {
			http.NotFound(w, r)
			return
		}
		offsets = append(offsets, q.Get("index_offset"))
		n := paymentsPageSize // a full first page forces a second request
		if q.Get("index_offset") != "0" {
			n = 1
		}
		payment := `{"status":"FAILED","creation_time_ns":"%d","htlcs":[{"status":"FAILED","attempt_time_ns":"%d",` +
			`"route":{"total_amt_msat":"1001000","total_fees_msat":"1000","hops":[{"chan_id":"111","pub_key":"03bb"},{"chan_id":"222","pub_key":"02aa"}]},` +
			`"failure":{"code":"TEMPORARY_CHANNEL_FAILURE","failure_source_index":1}}]}`
		ps := make([]string, n)
		for i := range ps {
			ts := since.Add(time.Hour).UnixNano()
			ps[i] = fmt.Sprintf(payment, ts, ts+1000)
		}
		fmt.Fprintf(w, `{"payments":[%s],"last_index_offset":"%d"}`, strings.Join(ps, ","), len(offsets)*paymentsPageSize)
	})
	pays, err := c.Payments(context.Background(), since)
	if err != nil {
		t.Fatal(err)
	}
	if len(pays) != paymentsPageSize+1 || len(offsets) != 2 || offsets[1] != fmt.Sprint(paymentsPageSize) {
		t.Fatalf("got %d payments, offsets %v", len(pays), offsets)
	}
	a := pays[0].Attempts[0]
	if pays[0].Status != "FAILED" || a.TotalAmtMsat != 1_001_000 || a.TotalFeesMsat != 1000 || len(a.Hops) != 2 || a.Hops[1].ChanID != 222 ||
		a.Hops[1].PubKey != "02aa" || a.FailureCode != "TEMPORARY_CHANNEL_FAILURE" || a.FailureSourceIndex != 1 ||
		!a.At.Equal(since.Add(time.Hour+time.Microsecond)) {
		t.Errorf("first attempt = %+v", a)
	}
}

func TestErrorsCarryLNDsMessage(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"code":2,"message":"edge not found","details":[]}`)
	})
	_, err := c.GetChanInfo(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "edge not found") {
		t.Errorf("err = %v, want LND's message", err)
	}
}

func TestRedirectsAreRefused(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.com/steal", http.StatusFound)
	})
	if _, err := c.GetInfo(context.Background()); err == nil || !strings.Contains(err.Error(), "refusing redirect") {
		t.Errorf("err = %v, want a refused redirect", err)
	}
}

func TestCallsRespectTheDeadline(t *testing.T) {
	release := make(chan struct{})
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { <-release })
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	began := time.Now()
	if _, err := c.GetInfo(ctx); err == nil {
		t.Fatal("a call past its deadline succeeded")
	}
	if waited := time.Since(began); waited > 5*time.Second {
		t.Errorf("call returned after %v, not at its deadline", waited)
	}
}

func TestNewClientRejectsBadInputs(t *testing.T) {
	dir := t.TempDir()
	notPEM := filepath.Join(dir, "tls.cert")
	if err := os.WriteFile(notPEM, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewClient(Config{Address: "localhost:8080", TLSCertPath: notPEM, MacaroonPath: notPEM}); err == nil {
		t.Error("a file without a certificate was accepted")
	}
	if _, err := NewClient(Config{TLSCertPath: notPEM, MacaroonPath: notPEM}); err == nil {
		t.Error("a missing address was accepted")
	}
}

func TestGraphNode(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/graph/node/"+peerPub || r.URL.Query().Get("include_channels") != "true" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"node":{"pub_key":%q,"alias":"synthetic peer"},"num_channels":1,"channels":[{"channel_id":"333","capacity":"2000000","node1_pub":%q,"node2_pub":%q,
			"node1_policy":{"fee_base_msat":"1000","fee_rate_milli_msat":"250"},"node2_policy":null}]}`, peerPub, nodePub, peerPub)
	})
	n, err := c.GraphNode(context.Background(), peerPub)
	if err != nil {
		t.Fatal(err)
	}
	edges := n.Channels
	if n.Alias != "synthetic peer" || len(edges) != 1 || edges[0].ChanID != 333 || edges[0].CapacitySat != 2000000 || edges[0].Node1Policy.FeeRateMilliMsat != 250 {
		t.Errorf("node = %+v", n)
	}
	if _, err := c.GraphNode(context.Background(), "../getinfo"); err == nil {
		t.Error("a path that is not a pubkey was requested")
	}
}
