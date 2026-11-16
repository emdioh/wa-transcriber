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

// envFields maps config.json field names to the variable overriding them.
var envFields = map[string]string{
	"provider":          "WAT_PROVIDER",
	"api_key":           "WAT_API_KEY",
	"model":             "WAT_MODEL",
	"base_url":          "WAT_BASE_URL",
	"translation_model": "WAT_TRANSLATION_MODEL",
	"keep_languages":    "WAT_KEEP_LANGUAGES",
	"translate_to":      "WAT_TRANSLATE_TO",
	"timeout_seconds":   "WAT_TIMEOUT",
}

// EnvOverrides returns field -> variable name for the fields currently set
// by the environment (the web UI shows them read-only).
func EnvOverrides(getenv func(string) string) map[string]string {
	if getenv == nil {
		getenv = os.Getenv
	}
	out := map[string]string{}
	for field, env := range envFields {
		if strings.TrimSpace(getenv(env)) != "" {
			out[field] = env
		}
	}
	return out
}

// Defaults returns a provider's default base URL, model and translation
// model ("" for unknown providers).
func Defaults(provider string) (baseURL, model, translationModel string) {
	d := defaults[provider]
	return d.baseURL, d.model, d.translationModel
}
