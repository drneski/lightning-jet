package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// isolate keeps the machine's own config and data out of a test.
func isolate(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
}

func TestRun(t *testing.T) {
	isolate(t)
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "no command", args: nil, wantCode: 2, wantStderr: "Usage:"},
		{name: "version", args: []string{"version"}, wantCode: 0, wantStdout: "jet dev\n"},
		{name: "version flag", args: []string{"--version"}, wantCode: 0, wantStdout: "jet dev\n"},
		{name: "help", args: []string{"help"}, wantCode: 0, wantStdout: "Usage:"},
		{name: "unknown command", args: []string{"rebalance"}, wantCode: 2, wantStderr: `unknown command "rebalance"`},
		{name: "run without credentials", args: []string{"run"}, wantCode: 2, wantStderr: "tls.cert and readonly.macaroon are required"},
		{name: "run in live mode", args: []string{"run", "--tls-cert", "c", "--macaroon", "m", "--fees-mode", "live"}, wantCode: 2, wantStderr: "live needs an executor"},
		{name: "run with a missing config", args: []string{"run", "--config", "/nonexistent/jet.conf"}, wantCode: 1, wantStderr: "no such file"},
		{name: "health without a database", args: []string{"health", "--data-dir", t.TempDir()}, wantCode: 1, wantStderr: "no database"},
		{name: "explain without a channel", args: []string{"explain"}, wantCode: 2, wantStderr: "expected one channel id"},
		{name: "explain a bad channel id", args: []string{"explain", "abc"}, wantCode: 2, wantStderr: "invalid channel id"},
		{name: "status without a database", args: []string{"status", "--data-dir", t.TempDir()}, wantCode: 1, wantStderr: "no database"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stderr %q)", code, tt.wantCode, stderr.String())
			}
			if !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.wantStdout)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

const (
	us   = "02aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	peer = "03bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// Synthetic channels, funded long before the forwards window.
var (
	fullBusy  = fmt.Sprint(uint64(800000)<<40 | 1)
	balanced  = fmt.Sprint(uint64(800000)<<40 | 2)
	fullQuiet = fmt.Sprint(uint64(800000)<<40 | 3)
)

// fakeLND serves a synthetic node over LND's REST API.
func fakeLND(t *testing.T) (address, cert, macaroon string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Grpc-Metadata-macaroon") == "" {
			http.Error(w, `{"message":"no macaroon"}`, http.StatusUnauthorized)
			return
		}
		hold := func() { // a stream stays open until the client goes away
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}
		switch {
		case r.URL.Path == "/v2/router/htlcevents":
			fmt.Fprintln(w, `{"result":{"event_type":"UNKNOWN","subscribed_event":{}}}`)
			fmt.Fprintf(w, `{"result":{"incoming_channel_id":%q,"outgoing_channel_id":%q,"timestamp_ns":"%d","event_type":"FORWARD",`+
				`"link_fail_event":{"info":{"outgoing_amt_msat":"300000000"},"wire_failure":"TEMPORARY_CHANNEL_FAILURE","failure_detail":"INSUFFICIENT_BALANCE"}}}`+"\n",
				balanced, fullBusy, time.Now().UnixNano())
			hold()
		case r.URL.Path == "/v1/channels/subscribe":
			hold()
		case strings.HasPrefix(r.URL.Path, "/v1/graph/node/"):
			fmt.Fprint(w, `{"channels":[]}`)
		case r.URL.Path == "/v1/getinfo":
			fmt.Fprintf(w, `{"identity_pubkey":%q,"block_height":900000,"synced_to_chain":true,"synced_to_graph":true}`, us)
		case r.URL.Path == "/v1/channels":
			ch := func(id, local, remote string) string {
				return fmt.Sprintf(`{"active":true,"remote_pubkey":%q,"chan_id":%q,"capacity":"5000000","local_balance":%q,"remote_balance":%q}`, peer, id, local, remote)
			}
			fmt.Fprintf(w, `{"channels":[%s,%s,%s]}`, ch(fullBusy, "4600000", "390000"), ch(balanced, "2500000", "2490000"), ch(fullQuiet, "4500000", "490000"))
		case strings.HasPrefix(r.URL.Path, "/v1/graph/edge/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/graph/edge/")
			p := func(ppm int) string {
				return fmt.Sprintf(`{"time_lock_delta":80,"fee_base_msat":"0","fee_rate_milli_msat":"%d"}`, ppm)
			}
			fmt.Fprintf(w, `{"channel_id":%q,"node1_pub":%q,"node2_pub":%q,"node1_policy":%s,"node2_policy":%s}`, id, us, peer, p(500), p(400))
		case r.URL.Path == "/v1/switch":
			var events []string
			add := func(out string, n int) {
				for i := 0; i < n; i++ {
					ts := time.Now().Add(-time.Duration(i+1) * time.Hour).UnixNano()
					events = append(events, fmt.Sprintf(`{"timestamp_ns":"%d","chan_id_in":%q,"chan_id_out":%q,"amt_in_msat":"100050000","amt_out_msat":"100000000","fee_msat":"50000"}`, ts, balanced, out))
				}
			}
			add(fullBusy, 12)
			add(fullQuiet, 3)
			fmt.Fprintf(w, `{"forwarding_events":[%s],"last_offset_index":%d}`, strings.Join(events, ","), len(events))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	cert = filepath.Join(dir, "tls.cert")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	macaroon = filepath.Join(dir, "readonly.macaroon")
	if err := os.WriteFile(macaroon, []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	return srv.Listener.Addr().String(), cert, macaroon
}

func TestRunStatusExplainEndToEnd(t *testing.T) {
	isolate(t)
	address, cert, macaroon := fakeLND(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	jet := func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), args, &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}

	if code, _, stderr := jet("run", "--once", "--lnd", address, "--tls-cert", cert, "--macaroon", macaroon, "--data-dir", dataDir); code != 0 {
		t.Fatalf("run --once: exit %d: %s", code, stderr)
	}

	code, stdout, stderr := jet("status", "--data-dir", dataDir)
	if code != 0 {
		t.Fatalf("status: exit %d: %s", code, stderr)
	}
	for _, want := range []string{"Channels     3", "Proposed     1 (1 approved, 0 rejected)", "Abstained    2 (insufficient evidence 1, no profitable action 1)",
		fullBusy + "     92%  proposed 500 to 450 ppm: approved", balanced + "     50%  abstained: no profitable action",
		fullQuiet + "     90%  abstained: insufficient evidence"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status is missing %q:\n%s", want, stdout)
		}
	}

	code, stdout, stderr = jet("explain", "--data-dir", dataDir, fullBusy)
	if code != 0 {
		t.Fatalf("explain: exit %d: %s", code, stderr)
	}
	for _, want := range []string{"Change the fee from 500 to 450 ppm", "Approved in shadow mode: 7 of 7 rules passed.", "Shadow mode: approved and recorded, not executed"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("explain is missing %q:\n%s", want, stdout)
		}
	}

	code, stdout, _ = jet("explain", "--json", "--data-dir", dataDir, fullQuiet)
	if code != 0 {
		t.Fatalf("explain --json: exit %d", code)
	}
	var x struct {
		ChanID   string `json:"chan_id"`
		Proposed struct {
			Abstention struct {
				Reason string `json:"reason"`
			} `json:"abstention"`
		} `json:"proposed"`
	}
	if err := json.Unmarshal([]byte(stdout), &x); err != nil {
		t.Fatalf("explain --json is not JSON: %v\n%s", err, stdout)
	}
	if x.ChanID != fullQuiet || x.Proposed.Abstention.Reason != "insufficient_evidence" {
		t.Errorf("explain --json = %+v", x)
	}

	if code, _, stderr := jet("explain", "--data-dir", dataDir, "42"); code != 1 || !strings.Contains(stderr, "no decision recorded") {
		t.Errorf("unknown channel: exit %d, %q", code, stderr)
	}
}

func TestDaemonRecordsStreamsAndReportsHealth(t *testing.T) {
	isolate(t)
	address, cert, macaroon := fakeLND(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	jet := func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), args, &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}
	if code, out, _ := jet("health", "--data-dir", dataDir); code != 1 || out != "" {
		t.Errorf("health before any run: exit %d, %q", code, out)
	}

	// A config file supplies the LND settings; the data directory comes from a flag.
	conf := filepath.Join(t.TempDir(), "jet.conf")
	if err := os.WriteFile(conf, []byte(fmt.Sprintf("[lnd]\nrest_address = %s\ntls_cert = %s\nmacaroon = %s\n[jet]\ninterval = 1h\n", address, cert, macaroon)), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int)
	var stderr bytes.Buffer
	go func() {
		done <- run(ctx, []string{"run", "--config", conf, "--data-dir", dataDir}, io.Discard, &stderr)
	}()

	deadline := time.Now().Add(15 * time.Second)
	for {
		code, out, _ := jet("health", "--data-dir", dataDir)
		if code == 0 {
			for _, want := range []string{"ok    cycle", "ok    htlc stream", "ok    channels stream"} {
				if !strings.Contains(out, want) {
					t.Errorf("health is missing %q:\n%s", want, out)
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Jet never became healthy:\n%s\nlog:\n%s", out, stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The failure arrives on the stream after the first cycle; wait for it.
	for {
		_, out, _ := jet("status", "--data-dir", dataDir)
		if strings.Contains(out, "1 forwards failed for lack of balance") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the streamed failure was never recorded:\n%s", out)
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("run exited %d:\n%s", code, stderr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not stop when canceled")
	}

	code, out, _ := jet("status", "--data-dir", dataDir)
	if code != 0 || !strings.Contains(out, "Recording    htlc stream:") || !strings.Contains(out, "Recording    channels stream:") {
		t.Errorf("status after the run: exit %d\n%s", code, out)
	}
}
