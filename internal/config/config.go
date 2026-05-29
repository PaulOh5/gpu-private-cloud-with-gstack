// Package config persists the small amount of state the flex CLI needs between
// invocations: the coordinator address (the bootstrap decision — set explicitly
// via `flex join <addr>`, MagicDNS name recommended) and this machine's node id.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Config is the on-disk CLI configuration.
type Config struct {
	Coordinator string `json:"coordinator"` // e.g. http://office-coord.tailnet:7070
	Node        string `json:"node"`        // this machine's node id (defaults to hostname)
}

// Path returns the config file location, honoring FLEX_CONFIG for tests/overrides.
func Path() (string, error) {
	if p := os.Getenv("FLEX_CONFIG"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".flex", "config.json"), nil
}

// Load reads the config. A missing file yields a zero Config and nil error so
// callers can give their own "run `flex join` first" message.
func Load() (Config, error) {
	p, err := Path()
	if err != nil {
		return Config{}, err
	}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", p, err)
	}
	return c, nil
}

// Save writes the config, creating the directory if needed.
func Save(c Config) error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}
