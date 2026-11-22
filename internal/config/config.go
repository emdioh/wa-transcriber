// Package config holds the persistent settings in /data/config.json.
//
// Reads return a deep copy; writes go through Update, which serialises
// mutations and replaces the file atomically (temp file + fsync + rename) so
// a power cut never leaves a half-written config on the SD card.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// Config is the whole persistent state besides the WhatsApp session.
type Config struct {
	Transcription Transcription `json:"transcription"`
	Web           Web           `json:"web"`
	// PublicURL is shown by !status when the network is not managed (Docker).
	PublicURL string `json:"public_url,omitempty"`
	// Language is the language of the bot's WhatsApp replies ("en", "it").
	Language string `json:"language,omitempty"`
	// UpdateCheck is the automatic binary update check: off or daily (M9).
	UpdateCheck string `json:"update_check,omitempty"`
	// UpdateAuto installs a found update without asking (still behind the
	// health gate and never while a note is queued or transcribing).
	UpdateAuto bool `json:"update_auto,omitempty"`
}

// DefaultLanguage is used when Language is empty or unknown.
const DefaultLanguage = "en"

// Update check intervals, in menu order. Empty/unknown means Off.
const (
	UpdateCheckOff   = "off"
	UpdateCheckDaily = "daily"
)

// UpdateChecks are the supported values of Config.UpdateCheck.
var UpdateChecks = []string{UpdateCheckOff, UpdateCheckDaily}

// Languages are the supported bot reply languages, in menu order.
var Languages = []string{"en", "it"}

// NormalizeLanguage returns lang when supported, DefaultLanguage otherwise.
func NormalizeLanguage(lang string) string {
	for _, l := range Languages {
		if lang == l {
			return lang
		}
	}
	return DefaultLanguage
}

// NormalizeUpdateCheck returns check when supported, UpdateCheckOff
// otherwise, so an empty field (files written before M9) means off.
func NormalizeUpdateCheck(check string) string {
	for _, c := range UpdateChecks {
		if check == c {
			return check
		}
	}
	return UpdateCheckOff
}

// Transcription settings.
type Transcription struct {
	// Global is the default for chats without an override.
	Global bool `json:"global"`
	// Chats maps a chat JID to its override.
	Chats map[string]bool `json:"chats,omitempty"`

	Provider string `json:"provider,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
	Model    string `json:"model,omitempty"`
	BaseURL  string `json:"base_url,omitempty"`

	// KeepLanguages are posted as transcribed (ISO 639-1 codes).
	KeepLanguages []string `json:"keep_languages,omitempty"`
	// TranslateTo is the target for every other language ("" = never translate).
	TranslateTo      string `json:"translate_to,omitempty"`
	TranslationModel string `json:"translation_model,omitempty"`
	// TimeoutSeconds of one provider request (0 = default).
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// Web UI settings.
type Web struct {
	PasswordHash string `json:"password_hash,omitempty"`
}

func (c Config) clone() Config {
	c.Transcription.Chats = maps.Clone(c.Transcription.Chats)
	c.Transcription.KeepLanguages = slices.Clone(c.Transcription.KeepLanguages)
	return c
}

// envFields maps config.json field names to the variable overriding them. An
// override is read where the value is used and never written back (D10).
var envFields = map[string]string{
	"update_check": "WAT_UPDATE_CHECK",
	"update_auto":  "WAT_UPDATE_AUTO",
}

// UpdateCheckMode returns the effective update check (WAT_UPDATE_CHECK, else
// config.json): UpdateCheckOff or UpdateCheckDaily.
func (c Config) UpdateCheckMode(getenv func(string) string) string {
	if v := strings.TrimSpace(envValue(getenv, "WAT_UPDATE_CHECK")); v != "" {
		return NormalizeUpdateCheck(strings.ToLower(v))
	}
	return NormalizeUpdateCheck(c.UpdateCheck)
}

// UpdateAutoOn returns the effective install-without-asking setting
// (WAT_UPDATE_AUTO=1/true, else config.json).
func (c Config) UpdateAutoOn(getenv func(string) string) bool {
	if v := strings.ToLower(strings.TrimSpace(envValue(getenv, "WAT_UPDATE_AUTO"))); v != "" {
		return v == "1" || v == "true"
	}
	return c.UpdateAuto
}

// UpdateSettings returns both effective update settings at once, as the web UI
// and the daily check read them: the form fields are one form, and the daily
// check must see a change made in it on its next tick (WAT_UPDATE_CHECK and
// WAT_UPDATE_AUTO still win over config.json).
func (c Config) UpdateSettings(getenv func(string) string) (checkMode string, auto bool) {
	return c.UpdateCheckMode(getenv), c.UpdateAutoOn(getenv)
}

// EnvOverrides returns field -> variable name for the update fields
// currently set by the environment (the web UI shows them read-only).
func EnvOverrides(getenv func(string) string) map[string]string {
	out := map[string]string{}
	for field, env := range envFields {
		if strings.TrimSpace(envValue(getenv, env)) != "" {
			out[field] = env
		}
	}
	return out
}

func envValue(getenv func(string) string, name string) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	return getenv(name)
}

// Effective tells whether auto-transcription is on for a chat. keys are the
// equivalent identifiers of the chat (phone-number and LID form): the first
// one with an override wins.
func (t Transcription) Effective(keys ...string) bool {
	if v, ok := t.Override(keys...); ok {
		return v
	}
	return t.Global
}

// Override returns the per-chat override, if any.
func (t Transcription) Override(keys ...string) (value, ok bool) {
	for _, k := range keys {
		if v, found := t.Chats[k]; found {
			return v, true
		}
	}
	return false, false
}

// Store is the concurrency-safe handle on config.json.
type Store struct {
	path string
	mu   sync.Mutex
	cur  Config
}

// Open loads path, or starts from defaults when it does not exist.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(data, &s.cur); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if s.cur.UpdateCheck != "" && NormalizeUpdateCheck(s.cur.UpdateCheck) != s.cur.UpdateCheck {
		return nil, fmt.Errorf("parse %s: unknown update_check %q (off or daily)", path, s.cur.UpdateCheck)
	}
	return s, nil
}

// Get returns a copy of the current config.
func (s *Store) Get() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur.clone()
}

// Update applies fn to a copy and persists it; on error nothing changes.
func (s *Store) Update(fn func(*Config)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cur.clone()
	fn(&next)
	if err := writeAtomic(s.path, next); err != nil {
		return err
	}
	s.cur = next
	return nil
}

// Reset deletes the file and returns to defaults.
func (s *Store) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.cur = Config{}
	return nil
}

func writeAtomic(path string, c Config) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
