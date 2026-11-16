package transcribe

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/emdioh/wa-transcriber/internal/config"
)

// Provider names.
const (
	OpenAI     = "openai"
	OpenRouter = "openrouter"
	Gemini     = "gemini"
)

// Providers lists the supported providers (UI order).
var Providers = []string{OpenAI, OpenRouter, Gemini}

type providerDefaults struct {
	baseURL, model, translationModel string
}

var defaults = map[string]providerDefaults{
	OpenAI:     {"https://api.openai.com/v1", "gpt-4o-mini-transcribe", "gpt-4o-mini"},
	OpenRouter: {"https://openrouter.ai/api/v1", "openai/whisper-large-v3", "openai/gpt-4o-mini"},
	// Gemini transcribes, detects and translates in one call.
	Gemini: {"https://generativelanguage.googleapis.com/v1beta", "gemini-flash-latest", ""},
}

// DefaultTimeout of one provider request.
const DefaultTimeout = 120 * time.Second

// Settings are the effective transcription settings: config.json, then
// environment overrides, then provider defaults.
type Settings struct {
	Provider         string
	APIKey           string
	Model            string
	BaseURL          string
	TranslationModel string
	Keep             []string // normalised ISO 639-1 codes
	TranslateTo      string   // normalised, "" = never translate
	Timeout          time.Duration
}

// EnvVars maps environment variables to the setting they override. They are
// meant for Docker and first boot; they are never written to config.json.
var EnvVars = []string{
	"WAT_PROVIDER", "WAT_API_KEY", "WAT_MODEL", "WAT_BASE_URL",
	"WAT_TRANSLATION_MODEL", "WAT_KEEP_LANGUAGES", "WAT_TRANSLATE_TO", "WAT_TIMEOUT",
}

// Resolve computes the effective settings. getenv is os.Getenv in production.
func Resolve(t config.Transcription, getenv func(string) string) Settings {
	if getenv == nil {
		getenv = os.Getenv
	}
	pick := func(env, cfg string) string {
		if v := strings.TrimSpace(getenv(env)); v != "" {
			return v
		}
		return strings.TrimSpace(cfg)
	}
	s := Settings{
		Provider:         strings.ToLower(pick("WAT_PROVIDER", t.Provider)),
		APIKey:           pick("WAT_API_KEY", t.APIKey),
		Model:            pick("WAT_MODEL", t.Model),
		BaseURL:          strings.TrimRight(pick("WAT_BASE_URL", t.BaseURL), "/"),
		TranslationModel: pick("WAT_TRANSLATION_MODEL", t.TranslationModel),
		TranslateTo:      NormalizeLanguage(pick("WAT_TRANSLATE_TO", t.TranslateTo)),
		Timeout:          DefaultTimeout,
	}
	keep := t.KeepLanguages
	if v := getenv("WAT_KEEP_LANGUAGES"); strings.TrimSpace(v) != "" {
		keep = strings.Split(v, ",")
	}
	for _, k := range keep {
		if code := NormalizeLanguage(k); code != "" {
			s.Keep = append(s.Keep, code)
		}
	}
	secs := t.TimeoutSeconds
	if v, err := strconv.Atoi(getenv("WAT_TIMEOUT")); err == nil {
		secs = v
	}
	if secs > 0 {
		s.Timeout = time.Duration(secs) * time.Second
	}
	if s.Provider == "" {
		s.Provider = OpenAI
	}
	if d, ok := defaults[s.Provider]; ok {
		if s.Model == "" {
			s.Model = d.model
		}
		if s.BaseURL == "" {
			s.BaseURL = d.baseURL
		}
		if s.TranslationModel == "" {
			s.TranslationModel = d.translationModel
		}
	}
	return s
}

// Configured reports whether a known provider has an API key.
func (s Settings) Configured() bool {
	_, ok := defaults[s.Provider]
	return ok && s.APIKey != ""
}

// wantsTranslation tells whether text in lang must be translated. An unknown
// language ("") must be detected first.
func (s Settings) wantsTranslation(lang string) bool {
	if s.TranslateTo == "" || lang == s.TranslateTo {
		return false
	}
	for _, k := range s.Keep {
		if k == lang {
			return false
		}
	}
	return true
}
