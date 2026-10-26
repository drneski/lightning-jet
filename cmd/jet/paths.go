package main

import (
	"errors"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/drneski/lightning-jet/v2/internal/config"
)

// common holds the flags every command shares: where the config file and the
// database are.
type common struct {
	config  string
	dataDir string
}

func addCommon(fs *flag.FlagSet) *common {
	c := &common{}
	fs.StringVar(&c.config, "config", "", "config file (default "+defaultConfigPath()+", if it exists)")
	fs.StringVar(&c.dataDir, "data-dir", "", "directory for Jet's database (default "+defaultDataDir()+")")
	return c
}

// load reads the config file, if any, and resolves the data directory. A file
// named with --config must exist; the default one is optional.
func (c *common) load() (config.Config, error) {
	cfg := config.Default()
	path, required := c.config, true
	if path == "" {
		path, required = defaultConfigPath(), false
	}
	loaded, err := config.Load(path)
	switch {
	case err == nil:
		cfg = loaded
	case !required && errors.Is(err, fs.ErrNotExist):
	default:
		return config.Config{}, err
	}
	if c.dataDir != "" {
		cfg.DataDir = c.dataDir
	}
	if cfg.DataDir == "" {
		cfg.DataDir = defaultDataDir()
	}
	return cfg, nil
}

func (c *common) dbPath(cfg config.Config) string { return filepath.Join(cfg.DataDir, "jet.db") }

// defaultDataDir is where Jet keeps its database: $XDG_DATA_HOME/lightning-jet,
// or the platform's usual place for application data.
func defaultDataDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "lightning-jet")
	}
	return filepath.Join(appDir(".local", "share"), "lightning-jet")
}

// defaultConfigPath is $XDG_CONFIG_HOME/lightning-jet/jet.conf, or the
// platform's usual place for configuration.
func defaultConfigPath() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "lightning-jet", "jet.conf")
	}
	return filepath.Join(appDir(".config"), "lightning-jet", "jet.conf")
}

// appDir is ~/<linux...> on Linux and ~/Library/Application Support on macOS.
func appDir(linux ...string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support")
	}
	return filepath.Join(append([]string{home}, linux...)...)
}
