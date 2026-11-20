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
}

// DefaultLanguage is used when Language is empty or unknown.
const DefaultLanguage = "en"

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
