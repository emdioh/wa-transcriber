package bot

import (
	"strings"
	"testing"

	"github.com/emdioh/wa-transcriber/internal/config"
)

// TestMessagesComplete fails when a language is missing a key (or has a
// stale one), so a new reply cannot be added without every translation.
func TestMessagesComplete(t *testing.T) {
	for _, lang := range config.Languages {
		table, ok := messages[lang]
		if !ok {
			t.Errorf("language %q: no messages", lang)
			continue
		}
		for k := msgKey(0); k < msgKeyCount; k++ {
			if _, ok := table[k]; !ok {
				t.Errorf("language %q: missing message %d", lang, int(k))
			}
		}
		if len(table) != int(msgKeyCount) {
			t.Errorf("language %q: %d messages, want %d", lang, len(table), msgKeyCount)
		}
	}
	if _, ok := messages[config.DefaultLanguage]; !ok {
		t.Fatalf("no messages for the default language %q", config.DefaultLanguage)
	}
}

func TestLanguageFallsBackToEnglish(t *testing.T) {
	e := newEnv(t)
	for _, lang := range []string{"", "xx", "EN"} {
		if err := e.bot.Config.Update(func(c *config.Config) { c.Language = lang }); err != nil {
			t.Fatal(err)
		}
		if got, want := e.bot.t(msgReplyToVoice), messages["en"][msgReplyToVoice]; got != want {
			t.Errorf("language %q: %q, want %q", lang, got, want)
		}
	}
}

func TestItalianReplies(t *testing.T) {
	e := newEnv(t)
	if err := e.bot.Config.Update(func(c *config.Config) { c.Language = "it" }); err != nil {
		t.Fatal(err)
	}

	e.send(e.msg(me, true, "!help"))
	if got, want := e.wa.last(t).text, messages["it"][msgTranscriptionUsage]+"\n\n"+messages["it"][msgHelpAdmin]; got != want {
		t.Fatalf("!help = %q", got)
	}

	e.send(e.msg(friend, true, "!t"))
	if got, want := e.wa.last(t).text, messages["it"][msgReplyToVoice]; got != want {
		t.Fatalf("!t = %q, want %q", got, want)
	}

	e.send(e.msg(friend, true, "!transcription"))
	got := e.wa.last(t).text
	for _, want := range []string{"Trascrizione automatica", "Globale: disattiva", "Questa chat: non impostata"} {
		if !strings.Contains(got, want) {
			t.Fatalf("!transcription = %q, missing %q", got, want)
		}
	}

	e.send(e.msg(me, true, "!status"))
	got = e.wa.last(t).text
	for _, want := range []string{"Ultimo messaggio visto", "Trascrizione:", "Attivo da:", "Web UI:"} {
		if !strings.Contains(got, want) {
			t.Fatalf("!status = %q, missing %q", got, want)
		}
	}
}

func TestUnconfiguredTranscriptionReplyIsTranslated(t *testing.T) {
	e := newEnv(t)
	if err := e.bot.Config.Update(func(c *config.Config) { c.Language = "it" }); err != nil {
		t.Fatal(err)
	}
	e.bot.Transcriber = nil
	v := e.voice(friend, false)
	e.send(v) // not a command: the error is not reported
	cmd := e.msg(friend, true, "!t")
	cmd.Quoted = &QuotedMessage{Quote: v.AsQuote(), Voice: v.Voice}
	e.send(cmd)
	if got, want := e.wa.last(t).text, messages["it"][msgTranscriptionNotConfigured]; got != want {
		t.Fatalf("reply = %q, want %q", got, want)
	}
}
