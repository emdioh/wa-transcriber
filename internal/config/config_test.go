package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenMissingGivesDefaults(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if c := s.Get(); c.Transcription.Global || len(c.Transcription.Chats) != 0 {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestUpdatePersistsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s, _ := Open(path)
	err := s.Update(func(c *Config) {
		c.Transcription.Global = true
		c.Transcription.Chats = map[string]bool{"a@s.whatsapp.net": false}
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600 (the file holds an API key)", info.Mode().Perm())
	}
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	c := again.Get()
	if !c.Transcription.Global || c.Transcription.Chats["a@s.whatsapp.net"] {
		t.Fatalf("reloaded = %+v", c)
	}
}

func TestGetReturnsCopy(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "config.json"))
	_ = s.Update(func(c *Config) { c.Transcription.Chats = map[string]bool{"x": true} })
	c := s.Get()
	c.Transcription.Chats["x"] = false
	if !s.Get().Transcription.Chats["x"] {
		t.Fatal("mutating a Get() copy changed the store")
	}
}

func TestEffective(t *testing.T) {
	tr := Transcription{Global: true, Chats: map[string]bool{"lid": false}}
	if tr.Effective("pn", "lid") {
		t.Fatal("override on the alternate key must win over global")
	}
	if !tr.Effective("other") {
		t.Fatal("no override: global applies")
	}
}

func TestReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s, _ := Open(path)
	_ = s.Update(func(c *Config) { c.Web.PasswordHash = "h" })
	if err := s.Reset(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("config file still exists")
	}
	if s.Get().Web.PasswordHash != "" {
		t.Fatal("in-memory config not reset")
	}
	if err := s.Reset(); err != nil {
		t.Fatal("second reset must be a no-op:", err)
	}
}
