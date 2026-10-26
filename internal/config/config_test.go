package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
)

func TestParse(t *testing.T) {
	c, err := Parse(strings.NewReader(`
# Synthetic paths.
[lnd]
rest_address = localhost:8080
tls_cert = /var/lib/lnd/tls.cert
macaroon = /var/lib/lnd/readonly.macaroon

[jet]
data_dir = /var/lib/lightning-jet
interval = 30m

[fees]
mode = shadow

[limits]
fee_ppm_max = 2500
; a list: one channel per line
freeze = 111111111111111111
freeze = 222222222222222222

[observe]
gossip_refresh_hours = 0
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.TLSCert != "/var/lib/lnd/tls.cert" || c.Macaroon != "/var/lib/lnd/readonly.macaroon" || c.Interval != 30*time.Minute {
		t.Errorf("config = %+v", c)
	}
	if c.Limits.FeePPMMax != 2500 || c.Limits.FeePPMMin != 1 || len(c.Limits.Freeze) != 2 || c.Limits.Freeze[1] != 222222222222222222 {
		t.Errorf("limits = %+v", c.Limits)
	}
	if c.GossipRefreshHours != 0 || c.WindowDays != 30 || c.FeesMode != contracts.ModeShadow {
		t.Errorf("observe and fees = %+v", c)
	}
}

func TestParseRejects(t *testing.T) {
	for name, tc := range map[string]struct{ conf, want string }{
		"unknown key":     {"[limits]\nfee_ppm_maxx = 10", "line 2: unknown key fee_ppm_maxx in [limits]"},
		"unknown section": {"[fess]\nmode = shadow", "line 1: unknown section [fess]"},
		"set twice":       {"[jet]\ninterval = 1h\ninterval = 2h", "line 3: jet.interval is already set on line 2"},
		"no section":      {"interval = 1h", "line 1: interval is outside any [section]"},
		"not a number":    {"[limits]\ncooldown_hours = twelve", `line 2: limits.cooldown_hours: "twelve" is not a whole number`},
		"negative":        {"[limits]\ncooldown_hours = -1", "line 2: limits.cooldown_hours: -1 is negative"},
		"bad channel":     {"[limits]\nfreeze = 12x", `invalid channel id "12x"`},
		"bad duration":    {"[jet]\ninterval = soon", "line 2: jet.interval"},
		"live mode":       {"[fees]\nmode = live", "needs an executor"},
		"inverted bounds": {"[limits]\nfee_ppm_min = 500\nfee_ppm_max = 100", "fee_ppm_min 500 is above fee_ppm_max 100"},
		"short interval":  {"[jet]\ninterval = 10s", "under a minute"},
		"not key=value":   {"[jet]\ninterval", "line 2: expected key = value"},
		"trailing #":      {"[lnd]\nrest_address = localhost:8080  # the default", "line 2: a comment after a value"},
		"trailing ;":      {"[jet]\n\ninterval = 1h\t; hourly", "line 3: a comment after a value"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tc.conf))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestDefaultsWithoutAFile(t *testing.T) {
	c, err := Parse(strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	d := Default()
	if c.RESTAddress != d.RESTAddress || c.Interval != time.Hour || c.Limits.MaxStepPct != 15 {
		t.Errorf("empty config = %+v", c)
	}
}

func TestLoadNamesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jet.conf")
	if err := os.WriteFile(path, []byte("[nope]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("err = %v, want the path in it", err)
	}
}

func TestTheExampleConfigParses(t *testing.T) {
	c, err := Load("../../contrib/jet.conf.example")
	if err != nil {
		t.Fatalf("contrib/jet.conf.example: %v", err)
	}
	if c.Macaroon == "" || c.TLSCert == "" || c.Interval != time.Hour || c.GossipRefreshHours != 24 {
		t.Errorf("example config = %+v", c)
	}
}
