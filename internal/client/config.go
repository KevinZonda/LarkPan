// Package client holds the larkpan client-side credential config,
// stored at ~/.config/kevinzonda.larkpan/token.
package client

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Config is the persisted client credential set.
type Config struct {
	Server string `json:"server"` // e.g. http://edge:8080
	Token  string `json:"token"`  // lp_<label>_<exp>_<sig>
}

// Path returns ~/.config/kevinzonda.larkpan/token.
func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "kevinzonda.larkpan", "token"), nil
}

// Save writes the config with 0600 permissions.
func (c *Config) Save() error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o600)
}

// Load reads the stored config.
func Load() (*Config, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	return &c, nil
}
