// Package transcribe turns voice notes into text with a cloud provider and,
// when configured, translates languages the user does not want to read as-is.
package transcribe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"go.mau.fi/whatsmeow/proto/waE2E"

	"github.com/emdioh/wa-transcriber/internal/bot"
	"github.com/emdioh/wa-transcriber/internal/config"
)

const (
	// MaxAudioBytes: OpenAI's upload limit; Gemini's inline limit is ~20 MB
	// after base64. A minute of voice note is ~100 KB, so this is hours.
	MaxAudioBytes = 15 << 20
	// MaxWaiting jobs queue behind the running one; more are refused.
	MaxWaiting = 8
)

// ErrBusy is returned when too many voice notes are already queued.
var ErrBusy = errors.New("too many voice notes queued")

// Downloader fetches and decrypts a voice note's audio.
type Downloader interface {
	DownloadAudio(ctx context.Context, a *waE2E.AudioMessage) ([]byte, error)
}

// Result of one voice note.
type Result struct {
	Text        string
	Language    string // ISO 639-1, "" if unknown
	Translation string // "" when not translated
	Target      string // translation language
}

// Format renders the reply body (the bot adds the 🎤 prefix).
func Format(r Result) string {
	if r.Translation == "" {
		return r.Text
	}
	from := r.Language
	if from == "" {
		from = "?"
	}
	return fmt.Sprintf("[%s→%s] %s\n\n🗣 %s", from, r.Target, r.Translation, r.Text)
}

// Service implements bot.Transcriber. Settings are read at every job, so
// changes from the web UI apply immediately. One job runs at a time.
type Service struct {
	Config   *config.Store
	Download Downloader
	HTTP     *http.Client // nil: a default client
	Log      *slog.Logger
	Getenv   func(string) string // nil: os.Getenv
	queue    chan struct{}       // MaxWaiting+1 slots
	running  chan struct{}       // 1 slot
}

// New returns a ready Service.
func New(cfg *config.Store, dl Downloader, log *slog.Logger) *Service {
	return &Service{
		Config:   cfg,
		Download: dl,
		Log:      log,
		queue:    make(chan struct{}, MaxWaiting+1),
		running:  make(chan struct{}, 1),
	}
}

// Settings returns the effective settings.
func (s *Service) Settings() Settings { return Resolve(s.Config.Get().Transcription, s.Getenv) }

// Configured implements bot.Transcriber.
func (s *Service) Configured() bool { return s.Settings().Configured() }

// Describe implements bot.Transcriber: "provider / model" for !status.
func (s *Service) Describe() string {
	st := s.Settings()
	d := st.Provider + " / " + st.Model
	if st.TranslateTo != "" {
		d += ", translate to " + st.TranslateTo
		if len(st.Keep) > 0 {
			d += " (keep " + strings.Join(st.Keep, ", ") + ")"
		}
	}
	return d
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Transcribe implements bot.Transcriber.
func (s *Service) Transcribe(ctx context.Context, v bot.Voice) (string, error) {
	st := s.Settings()
	if !st.Configured() {
		return "", bot.ErrNotConfigured
	}
	if n := v.Audio.GetFileLength(); n > MaxAudioBytes {
		return "", fmt.Errorf("voice note too large (%d bytes)", n)
	}

	select {
	case s.queue <- struct{}{}:
	default:
		return "", ErrBusy
	}
	defer func() { <-s.queue }()
	select {
	case s.running <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-s.running }()

	audio, err := s.Download.DownloadAudio(ctx, v.Audio)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	if len(audio) > MaxAudioBytes {
		return "", fmt.Errorf("voice note too large (%d bytes)", len(audio))
	}
	ctx, cancel := context.WithTimeout(ctx, st.Timeout)
	defer cancel()
	r, err := s.run(ctx, st, audio)
	if err != nil {
		return "", err
	}
	s.log().Info("voice note transcribed", "chat", v.Chat, "msg", v.Quote.ID,
		"bytes", len(audio), "chars", len(r.Text), "lang", r.Language, "translated", r.Translation != "")
	return Format(r), nil
}

func (s *Service) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return http.DefaultClient
}

func (s *Service) run(ctx context.Context, st Settings, audio []byte) (Result, error) {
	if st.Provider == Gemini {
		r, err := gemini{st, s.client()}.process(ctx, audio)
		if err != nil {
			return Result{}, err
		}
		return s.finish(st, r), nil
	}

	o := openAI{st, s.client()}
	text, lang, err := o.transcribe(ctx, audio)
	if err != nil {
		return Result{}, err
	}
	r := Result{Text: text, Language: lang}
	// Translate when a target is set, unless the language is already known
	// to be one to keep. An unknown language is detected by the same call.
	if text != "" && st.TranslateTo != "" && (lang == "" || st.wantsTranslation(lang)) {
		detected, tr, err := o.translate(ctx, text)
		if err != nil {
			// The transcript is still worth posting.
			s.log().Warn("translation failed, posting the transcript only", "err", err)
		} else {
			if detected != "" {
				r.Language = detected
			}
			r.Translation = tr
		}
	}
	return s.finish(st, r), nil
}

// finish drops translations the settings do not ask for (models do not
// always follow the instructions) and fills the target.
func (s *Service) finish(st Settings, r Result) Result {
	if r.Text == "" || !st.wantsTranslation(r.Language) || strings.EqualFold(r.Translation, r.Text) {
		r.Translation = ""
	}
	if r.Translation != "" {
		r.Target = st.TranslateTo
	}
	return r
}
