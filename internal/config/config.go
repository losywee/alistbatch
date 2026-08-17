package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config holds persisted Alist connection info.
// Password is stored in plaintext — file is created with 0600.
// If you prefer not to store password, set password to "" and only token will be saved.
type Config struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Password string `json:"password"`
	Token    string `json:"token"`
}

func (c *Config) Sanitized() *Config {
	cp := *c
	if cp.Password != "" {
		cp.Password = "***"
	}
	if cp.Token != "" && len(cp.Token) > 8 {
		cp.Token = cp.Token[:4] + "..." + cp.Token[len(cp.Token)-4:]
	} else if cp.Token != "" {
		cp.Token = "***"
	}
	return &cp
}

// Path returns config file path.
// Priority: $ALIST_CONFIG > UserConfigDir/alistbatch/config.json
func Path() string {
	if p := os.Getenv("ALIST_CONFIG"); p != "" {
		return p
	}
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, "alistbatch", "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "./alistbatch.json"
	}
	return filepath.Join(home, ".alistbatch.json")
}

func Load() (*Config, error) {
	p := Path()
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, err
	}
	if len(b) == 0 {
		return &Config{}, nil
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	c.Host = strings.TrimRight(strings.TrimSpace(c.Host), "/")
	return &c, nil
}

func Save(c *Config) error {
	p := Path()
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	// tighten perms if dir already existed with looser mode
	_ = os.Chmod(dir, 0700)
	c.Host = strings.TrimRight(strings.TrimSpace(c.Host), "/")
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	// ensure 0600 regardless of umask
	if err := os.Chmod(tmp, 0600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, p)
}

// Update merges non-empty fields into existing config and saves.
func Update(host, username, password, token string) error {
	c, err := Load()
	if err != nil {
		return err
	}
	if host != "" {
		c.Host = strings.TrimRight(strings.TrimSpace(host), "/")
	}
	if username != "" {
		c.Username = username
	}
	if password != "" {
		c.Password = password
	}
	if token != "" {
		c.Token = token
	}
	return Save(c)
}

func ClearToken() error {
	c, err := Load()
	if err != nil {
		return err
	}
	c.Token = ""
	return Save(c)
}

func ClearAll() error {
	p := Path()
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
