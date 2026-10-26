// Package config reads Jet's configuration file. The format is the one LND
// and Bitcoin Core operators know from lnd.conf: [sections], key = value lines,
// comment lines starting with # or ;, and a key repeated to make a list.
// Unknown keys, a setting given twice, a comment after a value, and bad values
// are errors with their line, never ignored.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
	"github.com/drneski/lightning-jet/v2/internal/limits"
)

// Config is everything jet run can be told.
type Config struct {
	RESTAddress string
	TLSCert     string
	Macaroon    string

	DataDir  string
	Interval time.Duration

	FeesMode contracts.Mode
	Limits   limits.Config

	WindowDays         int
	GossipRefreshHours int
}

// Default is the configuration without a file.
func Default() Config {
	return Config{
		RESTAddress: "localhost:8080",
		Interval:    time.Hour,
		FeesMode:    contracts.ModeShadow,
		Limits:      limits.DefaultConfig,
		WindowDays:  30, GossipRefreshHours: 24,
	}
}

// Load reads path over the defaults.
func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	c, err := Parse(f)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Parse reads a configuration over the defaults.
func Parse(r io.Reader) (Config, error) {
	c := Default()
	c.Limits.Freeze = nil
	seen := map[string]int{}
	section := ""
	sc := bufio.NewScanner(r)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			if !strings.HasSuffix(line, "]") {
				return Config{}, fmt.Errorf("line %d: unclosed section header", n)
			}
			section = strings.TrimSpace(line[1 : len(line)-1])
			if _, ok := setters[section]; !ok {
				return Config{}, fmt.Errorf("line %d: unknown section [%s]", n, section)
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return Config{}, fmt.Errorf("line %d: expected key = value", n)
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if trailingComment.MatchString(value) {
			// lnd.conf has no trailing comments either: the text would become
			// part of the value, and a path or address would be quietly wrong.
			return Config{}, fmt.Errorf("line %d: a comment after a value; comments go on their own line", n)
		}
		if section == "" {
			return Config{}, fmt.Errorf("line %d: %s is outside any [section]", n, key)
		}
		set, ok := setters[section][key]
		if !ok {
			return Config{}, fmt.Errorf("line %d: unknown key %s in [%s]", n, key, section)
		}
		name := section + "." + key
		if prev, dup := seen[name]; dup && !lists[name] {
			return Config{}, fmt.Errorf("line %d: %s is already set on line %d", n, name, prev)
		}
		seen[name] = n
		if err := set(&c, value); err != nil {
			return Config{}, fmt.Errorf("line %d: %s: %w", n, name, err)
		}
	}
	if err := sc.Err(); err != nil {
		return Config{}, err
	}
	return c, c.Validate()
}

// trailingComment is # or ; after whitespace, which in a value can only be a
// comment that was meant to end the line.
var trailingComment = regexp.MustCompile(`\s[#;]`)

// lists are the keys that may repeat.
var lists = map[string]bool{"limits.freeze": true}

var setters = map[string]map[string]func(*Config, string) error{
	"lnd": {
		"rest_address": func(c *Config, v string) error { c.RESTAddress = v; return nil },
		"tls_cert":     func(c *Config, v string) error { c.TLSCert = v; return nil },
		"macaroon":     func(c *Config, v string) error { c.Macaroon = v; return nil },
	},
	"jet": {
		"data_dir": func(c *Config, v string) error { c.DataDir = v; return nil },
		"interval": func(c *Config, v string) (err error) { c.Interval, err = time.ParseDuration(v); return err },
	},
	"fees": {
		"mode": func(c *Config, v string) error { c.FeesMode = contracts.Mode(v); return nil },
	},
	"limits": {
		"fee_ppm_min":             intTo(func(c *Config, v int64) { c.Limits.FeePPMMin = v }),
		"fee_ppm_max":             intTo(func(c *Config, v int64) { c.Limits.FeePPMMax = v }),
		"max_step_pct":            intTo(func(c *Config, v int64) { c.Limits.MaxStepPct = v }),
		"cooldown_hours":          intTo(func(c *Config, v int64) { c.Limits.CooldownHours = v }),
		"max_fee_changes_per_day": intTo(func(c *Config, v int64) { c.Limits.MaxFeeChangesPerDay = int(v) }),
		"freeze": func(c *Config, v string) error {
			id, err := contracts.ParseChanID(v)
			if err == nil {
				c.Limits.Freeze = append(c.Limits.Freeze, id)
			}
			return err
		},
	},
	"observe": {
		"window_days":          intTo(func(c *Config, v int64) { c.WindowDays = int(v) }),
		"gossip_refresh_hours": intTo(func(c *Config, v int64) { c.GossipRefreshHours = int(v) }),
	},
}

func intTo(set func(*Config, int64)) func(*Config, string) error {
	return func(c *Config, v string) error {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("%q is not a whole number", v)
		}
		if n < 0 {
			return fmt.Errorf("%d is negative", n)
		}
		set(c, n)
		return nil
	}
}

// ErrLive is returned for fees mode live, which needs an executor this version
// of Jet doesn't have.
var ErrLive = errors.New("fees mode live needs an executor, which this version of Jet does not have; use shadow")

// Validate checks values that are wrong together or out of range.
func (c Config) Validate() error {
	var errs []error
	switch c.FeesMode {
	case contracts.ModeShadow, contracts.ModeOff:
	case contracts.ModeLive:
		errs = append(errs, ErrLive)
	default:
		errs = append(errs, fmt.Errorf("fees mode must be shadow or off, not %q", c.FeesMode))
	}
	if c.Interval < time.Minute {
		errs = append(errs, fmt.Errorf("interval %s is under a minute", c.Interval))
	}
	if c.Limits.FeePPMMin > c.Limits.FeePPMMax {
		errs = append(errs, fmt.Errorf("fee_ppm_min %d is above fee_ppm_max %d", c.Limits.FeePPMMin, c.Limits.FeePPMMax))
	}
	if c.WindowDays < 1 || c.WindowDays > 365 {
		errs = append(errs, fmt.Errorf("window_days %d is outside 1-365", c.WindowDays))
	}
	return errors.Join(errs...)
}
