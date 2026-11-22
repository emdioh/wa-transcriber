package transcribe

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"

	"github.com/emdioh/wa-transcriber/internal/bot"
	"github.com/emdioh/wa-transcriber/internal/config"
)

const audio = "OggS-fake-audio"

type fakeDL struct{ err error }

func (f fakeDL) DownloadAudio(context.Context, *waE2E.AudioMessage) ([]byte, error) {
	return []byte(audio), f.err
}

// fakeAPI records requests and answers like the providers.
type fakeAPI struct {
	t           *testing.T
	mu          sync.Mutex
	paths       []string
	transcript  string
	language    string // verbose_json language ("italian")
	chatAnswer  string // content of the chat completion
	geminiReply string
	status      int
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path)
	f.mu.Unlock()
	if f.status != 0 {
		w.WriteHeader(f.status)
		io.WriteString(w, `{"error":"bad key sk-secret"}`)
		return
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/audio/transcriptions"):
		if r.Header.Get("Authorization") != "Bearer sk-secret" {
			f.t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		file, hdr, err := r.FormFile("file")
		if err != nil {
			f.t.Fatal(err)
		}
		data, _ := io.ReadAll(file)
		if string(data) != audio || hdr.Filename != "voice.ogg" {
			f.t.Errorf("upload = %q %q", data, hdr.Filename)
		}
		resp := map[string]any{"text": " " + f.transcript + " "}
		if r.FormValue("response_format") == "verbose_json" {
			resp["language"] = f.language
		}
		json.NewEncoder(w).Encode(resp)
	case strings.HasSuffix(r.URL.Path, "/chat/completions"):
		var req chatRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Messages[1].Content != f.transcript {
			f.t.Errorf("chat user message = %q", req.Messages[1].Content)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": f.chatAnswer}}},
		})
	case strings.HasSuffix(r.URL.Path, ":generateContent"):
		if r.Header.Get("x-goog-api-key") != "sk-secret" {
			f.t.Errorf("gemini key header missing")
		}
		var req struct {
			Contents []struct {
				Parts []struct {
					InlineData struct {
						Data string `json:"data"`
					} `json:"inline_data"`
				} `json:"parts"`
			} `json:"contents"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		got, _ := base64.StdEncoding.DecodeString(req.Contents[0].Parts[1].InlineData.Data)
		if string(got) != audio {
			f.t.Errorf("gemini audio = %q", got)
		}
		json.NewEncoder(w).Encode(map[string]any{"candidates": []any{map[string]any{
			"content": map[string]any{"parts": []any{map[string]any{"text": f.geminiReply}}},
		}}})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeAPI) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

func newService(t *testing.T, api *fakeAPI, tr config.Transcription) *Service {
	t.Helper()
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	cfg, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	tr.APIKey = "sk-secret"
	tr.BaseURL = srv.URL
	if err := cfg.Update(func(c *config.Config) { c.Transcription = tr }); err != nil {
		t.Fatal(err)
	}
	s := New(cfg, fakeDL{}, nil)
	s.Getenv = func(string) string { return "" }
	return s
}

func voice() bot.Voice {
	return bot.Voice{Quote: bot.Quote{ID: "V"}, Audio: &waE2E.AudioMessage{FileLength: proto.Uint64(10)}}
}

func TestOpenAIPlainTranscription(t *testing.T) {
	api := &fakeAPI{t: t, transcript: "ciao a tutti"}
	s := newService(t, api, config.Transcription{Provider: OpenAI})
	got, err := s.Transcribe(context.Background(), voice())
	if err != nil || got != "ciao a tutti" {
		t.Fatalf("got %q, %v", got, err)
	}
	if c := api.calls(); len(c) != 1 {
		t.Fatalf("no translation configured, calls = %v", c)
	}
}

func TestWhisperKnownKeptLanguageSkipsChat(t *testing.T) {
	api := &fakeAPI{t: t, transcript: "ciao", language: "italian"}
	s := newService(t, api, config.Transcription{
		Provider: OpenAI, Model: "whisper-1", KeepLanguages: []string{"it", "en"}, TranslateTo: "it",
	})
	got, err := s.Transcribe(context.Background(), voice())
	if err != nil || got != "ciao" {
		t.Fatalf("got %q, %v", got, err)
	}
	if c := api.calls(); len(c) != 1 {
		t.Fatalf("language known and kept: no chat call expected, calls = %v", c)
	}
}

func TestTranslationOfOtherLanguage(t *testing.T) {
	api := &fakeAPI{t: t, transcript: "Hallo zusammen",
		chatAnswer: "```json\n{\"language\":\"de\",\"translation\":\"Ciao a tutti\"}\n```"}
	s := newService(t, api, config.Transcription{
		Provider: OpenRouter, KeepLanguages: []string{"en"}, TranslateTo: "Italian",
	})
	got, err := s.Transcribe(context.Background(), voice())
	if err != nil {
		t.Fatal(err)
	}
	if got != "[de→it] Ciao a tutti\n\n🗣 Hallo zusammen" {
		t.Fatalf("got %q", got)
	}
}

func TestModelTranslatingAKeptLanguageIsIgnored(t *testing.T) {
	api := &fakeAPI{t: t, transcript: "hello",
		chatAnswer: `{"language":"en","translation":"ciao"}`}
	s := newService(t, api, config.Transcription{
		Provider: OpenAI, KeepLanguages: []string{"en"}, TranslateTo: "it",
	})
	got, _ := s.Transcribe(context.Background(), voice())
	if got != "hello" {
		t.Fatalf("kept language must not be translated, got %q", got)
	}
}

func TestTranslationFailureStillPostsTranscript(t *testing.T) {
	api := &fakeAPI{t: t, transcript: "Hallo", chatAnswer: "not json"}
	s := newService(t, api, config.Transcription{Provider: OpenAI, TranslateTo: "it"})
	got, err := s.Transcribe(context.Background(), voice())
	if err != nil || got != "Hallo" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestGeminiSingleCall(t *testing.T) {
	api := &fakeAPI{t: t,
		geminiReply: `{"language":"es","transcript":"Hola","translation":"Ciao"}`}
	s := newService(t, api, config.Transcription{
		Provider: Gemini, KeepLanguages: []string{"it", "en"}, TranslateTo: "it",
	})
	got, err := s.Transcribe(context.Background(), voice())
	if err != nil || got != "[es→it] Ciao\n\n🗣 Hola" {
		t.Fatalf("got %q, %v", got, err)
	}
	if c := api.calls(); len(c) != 1 || !strings.HasSuffix(c[0], "/models/gemini-flash-latest:generateContent") {
		t.Fatalf("calls = %v", c)
	}
}

func TestHTTPErrorRedactsKey(t *testing.T) {
	api := &fakeAPI{t: t, status: http.StatusUnauthorized}
	s := newService(t, api, config.Transcription{Provider: OpenAI})
	_, err := s.Transcribe(context.Background(), voice())
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") || strings.Contains(err.Error(), "sk-secret") {
		t.Fatalf("err = %v", err)
	}
}

func TestNotConfigured(t *testing.T) {
	cfg, _ := config.Open(filepath.Join(t.TempDir(), "config.json"))
	s := New(cfg, fakeDL{}, nil)
	s.Getenv = func(string) string { return "" }
	if s.Configured() {
		t.Fatal("no key: not configured")
	}
	if _, err := s.Transcribe(context.Background(), voice()); !errors.Is(err, bot.ErrNotConfigured) {
		t.Fatalf("err = %v", err)
	}
}

func TestDownloadErrorAndSizeLimit(t *testing.T) {
	api := &fakeAPI{t: t, transcript: "x"}
	s := newService(t, api, config.Transcription{Provider: OpenAI})
	s.Download = fakeDL{err: errors.New("media gone")}
	if _, err := s.Transcribe(context.Background(), voice()); err == nil || !strings.Contains(err.Error(), "media gone") {
		t.Fatalf("err = %v", err)
	}
	big := voice()
	big.Audio.FileLength = proto.Uint64(MaxAudioBytes + 1)
	if _, err := s.Transcribe(context.Background(), big); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v", err)
	}
	if len(api.calls()) != 0 {
		t.Fatal("nothing should reach the API")
	}
}

// blockingDL holds downloads until released, to fill the queue.
type blockingDL struct{ release chan struct{} }

func (b blockingDL) DownloadAudio(ctx context.Context, _ *waE2E.AudioMessage) ([]byte, error) {
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return []byte(audio), nil
}

func TestQueueIsBounded(t *testing.T) {
	api := &fakeAPI{t: t, transcript: "x"}
	s := newService(t, api, config.Transcription{Provider: OpenAI})
	dl := blockingDL{release: make(chan struct{})}
	s.Download = dl
	var wg sync.WaitGroup
	for range MaxWaiting + 1 {
		wg.Add(1)
		go func() { defer wg.Done(); s.Transcribe(context.Background(), voice()) }()
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(s.queue) < MaxWaiting+1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := s.Transcribe(context.Background(), voice()); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	close(dl.release)
	wg.Wait()
}

func TestBusy(t *testing.T) {
	api := &fakeAPI{t: t, transcript: "x"}
	s := newService(t, api, config.Transcription{Provider: OpenAI})
	if s.Busy() {
		t.Fatal("a fresh service is busy")
	}
	dl := blockingDL{release: make(chan struct{})}
	s.Download = dl
	done := make(chan error, 1)
	go func() { _, err := s.Transcribe(context.Background(), voice()); done <- err }()
	// The note is accepted as soon as the queue slot is taken, before it
	// starts downloading.
	deadline := time.Now().Add(2 * time.Second)
	for !s.Busy() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !s.Busy() {
		t.Fatal("a queued note does not count as busy")
	}
	close(dl.release)
	if err := <-done; err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	if s.Busy() {
		t.Fatal("still busy after the note was transcribed")
	}
	// A refused job must not leave the counter behind.
	big := voice()
	big.Audio.FileLength = proto.Uint64(MaxAudioBytes + 1)
	if _, err := s.Transcribe(context.Background(), big); err == nil {
		t.Fatal("oversized note accepted")
	}
	if s.Busy() {
		t.Fatal("a refused note leaves the service busy")
	}
}

func TestResolveEnvAndDefaults(t *testing.T) {
	env := map[string]string{"WAT_PROVIDER": "Gemini", "WAT_API_KEY": "k", "WAT_KEEP_LANGUAGES": "Italian, en-US"}
	s := Resolve(config.Transcription{Provider: OpenAI, APIKey: "cfg", TranslateTo: "IT"},
		func(k string) string { return env[k] })
	if s.Provider != Gemini || s.APIKey != "k" || s.Model != "gemini-flash-latest" ||
		!strings.HasPrefix(s.BaseURL, "https://generativelanguage") {
		t.Fatalf("settings = %+v", s)
	}
	if strings.Join(s.Keep, ",") != "it,en" || s.TranslateTo != "it" || s.Timeout != DefaultTimeout {
		t.Fatalf("languages = %v %q %v", s.Keep, s.TranslateTo, s.Timeout)
	}
	if !s.Configured() {
		t.Fatal("should be configured")
	}
	if Resolve(config.Transcription{Provider: "nope", APIKey: "k"}, func(string) string { return "" }).Configured() {
		t.Fatal("unknown provider must not count as configured")
	}
}

func TestNormalizeLanguage(t *testing.T) {
	for in, want := range map[string]string{
		"it": "it", "IT": "it", "pt-BR": "pt", "English": "en", "italiano": "it", "klingon": "", "": "",
	} {
		if got := NormalizeLanguage(in); got != want {
			t.Errorf("NormalizeLanguage(%q) = %q, want %q", in, got, want)
		}
	}
	if languageLabel("it") != "Italian (it)" {
		t.Fatalf("label = %q", languageLabel("it"))
	}
}
