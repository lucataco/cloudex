// Package config stores the CLI's settings in ~/.config/cloudex (or
// $CLOUDEX_CONFIG_DIR): which deployment to talk to and the current sandbox.
package config

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Config is written by `cloudex deploy` and read by every other command.
type Config struct {
	APIURL      string `json:"apiUrl,omitempty"`
	APIKey      string `json:"apiKey,omitempty"`
	AccountID   string `json:"accountId,omitempty"`
	PreviewZone string `json:"previewZone,omitempty"`
	// Current is the sandbox commands use when none is named.
	Current string `json:"current,omitempty"`
}

// Dir returns the configuration directory, creating it if needed.
func Dir() (string, error) {
	dir := os.Getenv("CLOUDEX_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config", "cloudex")
	}
	return dir, os.MkdirAll(dir, 0o700)
}

func path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Load reads the config. A missing file is an empty config.
func Load() (*Config, error) {
	p, err := path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Save writes the config readable only by the current user; it holds the API key.
func (c *Config) Save() error {
	p, err := path()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
