package config

import (
	"os"
	"path/filepath"
	"strings"
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

func TestUpdateDefaults(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	c := s.Get()
	if c.UpdateCheck != "" || c.UpdateAuto {
		t.Fatalf("stored defaults = %q/%v, want empty/false", c.UpdateCheck, c.UpdateAuto)
	}
	none := func(string) string { return "" }
	if got := c.UpdateCheckMode(none); got != UpdateCheckOff {
		t.Fatalf("effective update_check = %q, want %q", got, UpdateCheckOff)
	}
	if c.UpdateAutoOn(none) {
		t.Fatal("effective update_auto must default to false")
	}
}

func TestUpdateFieldsPersistAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s, _ := Open(path)
	if err := s.Update(func(c *Config) {
		c.UpdateCheck = UpdateCheckDaily
		c.UpdateAuto = true
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"update_check": "daily"`, `"update_auto": true`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("%s not persisted:\n%s", want, data)
		}
	}
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	c := again.Get()
	if c.UpdateCheck != UpdateCheckDaily || !c.UpdateAuto {
		t.Fatalf("reloaded = %q/%v", c.UpdateCheck, c.UpdateAuto)
	}
	// Reset erases them with everything else.
	if err := s.Reset(); err != nil {
		t.Fatal(err)
	}
	if c := s.Get(); c.UpdateCheck != "" || c.UpdateAuto {
		t.Fatalf("reset = %q/%v", c.UpdateCheck, c.UpdateAuto)
	}
}

func TestUpdateDefaultsAreOmitted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s, _ := Open(path)
	if err := s.Update(func(c *Config) { c.PublicURL = "http://x" }); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "update_") {
		t.Fatalf("defaults written to the file:\n%s", data)
	}
}

func TestUpdateEnvOverrides(t *testing.T) {
	c := Config{UpdateCheck: UpdateCheckOff, UpdateAuto: false}
	env := map[string]string{"WAT_UPDATE_CHECK": "Daily", "WAT_UPDATE_AUTO": "1"}
	getenv := func(k string) string { return env[k] }
	if got := c.UpdateCheckMode(getenv); got != UpdateCheckDaily {
		t.Fatalf("update_check = %q, want daily", got)
	}
	if !c.UpdateAutoOn(getenv) {
		t.Fatal("WAT_UPDATE_AUTO=1 ignored")
	}
	for _, v := range []string{"1", "true", "TRUE", " true "} {
		env["WAT_UPDATE_AUTO"] = v
		if !(Config{}).UpdateAutoOn(getenv) {
			t.Errorf("WAT_UPDATE_AUTO=%q not true", v)
		}
	}
	for _, v := range []string{"0", "false", "no"} {
		env["WAT_UPDATE_AUTO"] = v
		if (Config{UpdateAuto: true}).UpdateAutoOn(getenv) {
			t.Errorf("WAT_UPDATE_AUTO=%q must turn it off", v)
		}
	}
	// An unknown env value falls back to off instead of breaking the device.
	env["WAT_UPDATE_CHECK"] = "weekly"
	if got := (Config{UpdateCheck: UpdateCheckDaily}).UpdateCheckMode(getenv); got != UpdateCheckOff {
		t.Fatalf("unknown env update_check = %q, want off", got)
	}
	// Overrides are read-only: they never reach the file.
	path := filepath.Join(t.TempDir(), "config.json")
	s, _ := Open(path)
	if err := s.Update(func(c *Config) { c.UpdateCheck = UpdateCheckDaily }); err != nil {
		t.Fatal(err)
	}
	env = map[string]string{"WAT_UPDATE_CHECK": "off", "WAT_UPDATE_AUTO": "1"}
	if got := s.Get().UpdateCheckMode(getenv); got != UpdateCheckOff {
		t.Fatalf("env override not applied: %q", got)
	}
	if again, _ := Open(path); again.Get().UpdateCheck != UpdateCheckDaily || again.Get().UpdateAuto {
		t.Fatal("an env override was persisted to config.json")
	}
	if ov := EnvOverrides(getenv); ov["update_check"] != "WAT_UPDATE_CHECK" || ov["update_auto"] != "WAT_UPDATE_AUTO" {
		t.Fatalf("EnvOverrides = %v", ov)
	}
	if ov := EnvOverrides(func(string) string { return "" }); len(ov) != 0 {
		t.Fatalf("EnvOverrides with an empty environment = %v", ov)
	}
}

func TestUpdateCheckValidation(t *testing.T) {
	for _, valid := range []string{"", UpdateCheckOff, UpdateCheckDaily} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(`{"update_check":"`+valid+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path); err != nil {
			t.Fatalf("update_check=%q rejected: %v", valid, err)
		}
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"update_check":"hourly"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "update_check") {
		t.Fatalf("unknown update_check accepted: %v", err)
	}
}

func TestNormalizeUpdateCheck(t *testing.T) {
	for in, want := range map[string]string{"": UpdateCheckOff, "off": UpdateCheckOff, "daily": UpdateCheckDaily, "Daily": UpdateCheckOff} {
		if got := NormalizeUpdateCheck(in); got != want {
			t.Errorf("NormalizeUpdateCheck(%q) = %q, want %q", in, got, want)
		}
	}
}
