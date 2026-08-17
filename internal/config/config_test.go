package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func withTempConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	t.Setenv("ALIST_CONFIG", p)
	return p
}

func TestSaveLoad(t *testing.T) {
	p := withTempConfig(t)
	if err := Save(&Config{Host: "https://example.com/", Username: "admin", Password: "secret", Token: "tok123"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// host should be trimmed
	b, _ := os.ReadFile(p)
	var raw map[string]string
	json.Unmarshal(b, &raw)
	if raw["host"] != "https://example.com" {
		t.Errorf("host = %q", raw["host"])
	}
	// perms 0600
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0600 {
		t.Errorf("perm = %o want 600", fi.Mode().Perm())
	}
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Host != "https://example.com" || c.Username != "admin" || c.Password != "secret" || c.Token != "tok123" {
		t.Fatalf("Load = %+v", c)
	}
}

func TestUpdateMerges(t *testing.T) {
	withTempConfig(t)
	Save(&Config{Host: "https://a.com", Username: "u1", Password: "p1", Token: "t1"})
	if err := Update("", "", "", "t2"); err != nil {
		t.Fatalf("Update: %v", err)
	}
	c, _ := Load()
	if c.Token != "t2" || c.Host != "https://a.com" || c.Username != "u1" {
		t.Fatalf("after Update = %+v", c)
	}
	// empty fields should not overwrite
	Update("", "u2", "", "")
	c, _ = Load()
	if c.Username != "u2" || c.Password != "p1" {
		t.Fatalf("merge = %+v", c)
	}
}

func TestClearToken(t *testing.T) {
	withTempConfig(t)
	Save(&Config{Host: "https://a.com", Token: "tok"})
	ClearToken()
	c, _ := Load()
	if c.Token != "" {
		t.Fatalf("token not cleared: %q", c.Token)
	}
	if c.Host != "https://a.com" {
		t.Fatalf("host lost: %+v", c)
	}
}

func TestClearAll(t *testing.T) {
	p := withTempConfig(t)
	Save(&Config{Host: "https://a.com"})
	if err := ClearAll(); err != nil {
		t.Fatalf("ClearAll: %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("file still exists")
	}
}

func TestSanitized(t *testing.T) {
	c := &Config{Host: "https://a.com", Username: "u", Password: "secret", Token: "longtoken123456"}
	s := c.Sanitized()
	if s.Password != "***" {
		t.Errorf("password = %q", s.Password)
	}
	if s.Token == "longtoken123456" {
		t.Error("token not masked")
	}
	if c.Password != "secret" {
		t.Error("original mutated")
	}
}

func TestLoadMissing(t *testing.T) {
	withTempConfig(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Host != "" || c.Token != "" {
		t.Fatalf("expected empty, got %+v", c)
	}
}
