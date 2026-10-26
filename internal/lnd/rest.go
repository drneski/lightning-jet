package lnd

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
)

// Config is how to reach LND's REST API.
type Config struct {
	// Address is LND's REST listener, host:port (LND's default is
	// localhost:8080).
	Address string
	// TLSCertPath is LND's tls.cert. The client trusts that certificate only.
	TLSCertPath string
	// MacaroonPath is the macaroon to authenticate with. Give it LND's
	// readonly.macaroon.
	MacaroonPath string
	// Timeout bounds each call whose context has no deadline. Zero means 30s.
	Timeout time.Duration
}

// Client reads from LND's REST API. It implements Reader.
type Client struct {
	base     string
	http     *http.Client
	macaroon string
	timeout  time.Duration
}

var _ Reader = (*Client)(nil)

const (
	maxResponseBytes = 64 << 20
	forwardsPageSize = 10000
)

// NewClient returns a client for cfg. It reads the certificate and macaroon
// once, at creation.
func NewClient(cfg Config) (*Client, error) {
	if cfg.Address == "" {
		return nil, errors.New("lnd: missing address")
	}
	pem, err := os.ReadFile(cfg.TLSCertPath)
	if err != nil {
		return nil, fmt.Errorf("lnd: reading TLS certificate: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("lnd: no certificate found in %s", cfg.TLSCertPath)
	}
	mac, err := os.ReadFile(cfg.MacaroonPath)
	if err != nil {
		return nil, fmt.Errorf("lnd: reading macaroon: %w", err)
	}
	if len(mac) == 0 {
		return nil, fmt.Errorf("lnd: macaroon %s is empty", cfg.MacaroonPath)
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		Proxy:           nil, // never route LND traffic through a proxy
	}
	return &Client{
		base: "https://" + cfg.Address,
		http: &http.Client{
			Transport: transport,
			// LND never redirects; refusing keeps the macaroon on this host.
			CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("lnd: refusing redirect") },
		},
		macaroon: hex.EncodeToString(mac),
		timeout:  timeout,
	}, nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Grpc-Metadata-macaroon", c.macaroon)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("lnd %s: %w", path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("lnd %s: reading response: %w", path, err)
	}
	if len(data) > maxResponseBytes {
		return fmt.Errorf("lnd %s: response larger than %d bytes", path, maxResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &e) == nil && e.Message != "" {
			return fmt.Errorf("lnd %s: %s (HTTP %d)", path, e.Message, resp.StatusCode)
		}
		return fmt.Errorf("lnd %s: HTTP %d", path, resp.StatusCode)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("lnd %s: decoding response: %w", path, err)
	}
	return nil
}

// GetInfo implements Reader.
func (c *Client) GetInfo(ctx context.Context) (Info, error) {
	var r struct {
		IdentityPubkey string `json:"identity_pubkey"`
		Alias          string `json:"alias"`
		Version        string `json:"version"`
		BlockHeight    uint32 `json:"block_height"`
		SyncedToChain  bool   `json:"synced_to_chain"`
		SyncedToGraph  bool   `json:"synced_to_graph"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/getinfo", nil, &r); err != nil {
		return Info{}, err
	}
	return Info{
		IdentityPubkey: r.IdentityPubkey, Alias: r.Alias, Version: r.Version,
		BlockHeight: r.BlockHeight, SyncedToChain: r.SyncedToChain, SyncedToGraph: r.SyncedToGraph,
	}, nil
}

// ListChannels implements Reader.
func (c *Client) ListChannels(ctx context.Context) ([]Channel, error) {
	var r struct {
		Channels []struct {
			Active        bool     `json:"active"`
			RemotePubkey  string   `json:"remote_pubkey"`
			ChannelPoint  string   `json:"channel_point"`
			ChanID        wireUint `json:"chan_id"`
			Capacity      wireInt  `json:"capacity"`
			LocalBalance  wireInt  `json:"local_balance"`
			RemoteBalance wireInt  `json:"remote_balance"`
			Private       bool     `json:"private"`
		} `json:"channels"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/channels", nil, &r); err != nil {
		return nil, err
	}
	out := make([]Channel, 0, len(r.Channels))
	for _, ch := range r.Channels {
		out = append(out, Channel{
			ChanID: contracts.ChanID(ch.ChanID), RemotePubkey: ch.RemotePubkey, ChannelPoint: ch.ChannelPoint,
			Active: ch.Active, Private: ch.Private,
			CapacitySat: int64(ch.Capacity), LocalSat: int64(ch.LocalBalance), RemoteSat: int64(ch.RemoteBalance),
		})
	}
	return out, nil
}

type wirePolicy struct {
	TimeLockDelta    uint32  `json:"time_lock_delta"`
	MinHTLC          wireInt `json:"min_htlc"`
	FeeBaseMsat      wireInt `json:"fee_base_msat"`
	FeeRateMilliMsat wireInt `json:"fee_rate_milli_msat"`
	Disabled         bool    `json:"disabled"`
	MaxHTLCMsat      wireInt `json:"max_htlc_msat"`
	LastUpdate       int64   `json:"last_update"`
}

func (p *wirePolicy) policy() *RoutingPolicy {
	if p == nil {
		return nil
	}
	return &RoutingPolicy{
		TimeLockDelta: p.TimeLockDelta, MinHTLCMsat: int64(p.MinHTLC), MaxHTLCMsat: int64(p.MaxHTLCMsat),
		FeeBaseMsat: int64(p.FeeBaseMsat), FeeRateMilliMsat: int64(p.FeeRateMilliMsat),
		Disabled: p.Disabled, LastUpdate: time.Unix(p.LastUpdate, 0).UTC(),
	}
}

type wireEdge struct {
	ChannelID   wireUint    `json:"channel_id"`
	Capacity    wireInt     `json:"capacity"`
	Node1Pub    string      `json:"node1_pub"`
	Node2Pub    string      `json:"node2_pub"`
	Node1Policy *wirePolicy `json:"node1_policy"`
	Node2Policy *wirePolicy `json:"node2_policy"`
}

func (w wireEdge) edge() Edge {
	return Edge{
		ChanID: contracts.ChanID(w.ChannelID), CapacitySat: int64(w.Capacity), Node1Pub: w.Node1Pub, Node2Pub: w.Node2Pub,
		Node1Policy: w.Node1Policy.policy(), Node2Policy: w.Node2Policy.policy(),
	}
}

// GetChanInfo implements Reader.
func (c *Client) GetChanInfo(ctx context.Context, id contracts.ChanID) (Edge, error) {
	var r wireEdge
	if err := c.do(ctx, http.MethodGet, "/v1/graph/edge/"+id.String(), nil, &r); err != nil {
		return Edge{}, err
	}
	return r.edge(), nil
}

// NodeChannels implements Reader.
func (c *Client) NodeChannels(ctx context.Context, pubkey string) ([]Edge, error) {
	if !isHexPubkey(pubkey) {
		return nil, fmt.Errorf("lnd: %q is not a node public key", pubkey)
	}
	var r struct {
		Channels []wireEdge `json:"channels"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/graph/node/"+pubkey+"?include_channels=true", nil, &r); err != nil {
		return nil, err
	}
	out := make([]Edge, 0, len(r.Channels))
	for _, e := range r.Channels {
		out = append(out, e.edge())
	}
	return out, nil
}

// isHexPubkey reports whether s is a compressed public key in hex, so it is
// safe to put in a URL path.
func isHexPubkey(s string) bool {
	if len(s) != 66 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// ForwardingHistory implements Reader.
func (c *Client) ForwardingHistory(ctx context.Context, start, end time.Time) ([]Forward, error) {
	var out []Forward
	var offset uint32
	for {
		req := map[string]any{
			"start_time":     strconv.FormatInt(start.Unix(), 10),
			"end_time":       strconv.FormatInt(end.Unix(), 10),
			"index_offset":   offset,
			"num_max_events": forwardsPageSize,
		}
		var r struct {
			Events []struct {
				Timestamp   wireInt  `json:"timestamp"`
				TimestampNs wireInt  `json:"timestamp_ns"`
				ChanIDIn    wireUint `json:"chan_id_in"`
				ChanIDOut   wireUint `json:"chan_id_out"`
				AmtInMsat   wireInt  `json:"amt_in_msat"`
				AmtOutMsat  wireInt  `json:"amt_out_msat"`
				FeeMsat     wireInt  `json:"fee_msat"`
			} `json:"forwarding_events"`
			LastOffsetIndex uint32 `json:"last_offset_index"`
		}
		if err := c.do(ctx, http.MethodPost, "/v1/switch", req, &r); err != nil {
			return nil, err
		}
		for _, e := range r.Events {
			t := time.Unix(0, int64(e.TimestampNs)).UTC()
			if e.TimestampNs == 0 {
				t = time.Unix(int64(e.Timestamp), 0).UTC()
			}
			if t.Before(start) || !t.Before(end) {
				continue
			}
			out = append(out, Forward{
				Time: t, ChanIDIn: contracts.ChanID(e.ChanIDIn), ChanIDOut: contracts.ChanID(e.ChanIDOut),
				AmtInMsat: int64(e.AmtInMsat), AmtOutMsat: int64(e.AmtOutMsat), FeeMsat: int64(e.FeeMsat),
			})
		}
		if len(r.Events) < forwardsPageSize {
			return out, nil
		}
		if r.LastOffsetIndex <= offset {
			return nil, fmt.Errorf("lnd /v1/switch: paging did not advance past offset %d", offset)
		}
		offset = r.LastOffsetIndex
	}
}

// wireInt is an int64 as LND's REST API writes it: a JSON string, or a number
// for older fields.
type wireInt int64

func (w *wireInt) UnmarshalJSON(b []byte) error {
	v, err := parseWireNumber(b, func(s string) (any, error) { return strconv.ParseInt(s, 10, 64) })
	if err != nil {
		return err
	}
	*w = wireInt(v.(int64))
	return nil
}

// wireUint is a uint64 as LND's REST API writes it.
type wireUint uint64

func (w *wireUint) UnmarshalJSON(b []byte) error {
	v, err := parseWireNumber(b, func(s string) (any, error) { return strconv.ParseUint(s, 10, 64) })
	if err != nil {
		return err
	}
	*w = wireUint(v.(uint64))
	return nil
}

func parseWireNumber(b []byte, parse func(string) (any, error)) (any, error) {
	s := string(b)
	if s == "null" || s == `""` {
		return parse("0")
	}
	if len(s) >= 2 && s[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return nil, err
		}
		s = str
	}
	v, err := parse(s)
	if err != nil {
		return nil, fmt.Errorf("invalid integer %s", b)
	}
	return v, nil
}
