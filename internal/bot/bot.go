// Package bot decides what to do with each incoming WhatsApp message: run a
// !command or transcribe a voice note. It knows nothing about the WhatsApp
// connection itself; that is behind the Messenger interface.
package bot

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"golang.org/x/crypto/bcrypt"

	"github.com/emdioh/wa-transcriber/internal/config"
	"github.com/emdioh/wa-transcriber/internal/status"
)

const (
	// RecentWindow: older messages are replays (reconnection, offline queue).
	RecentWindow = 10 * time.Minute
	// StartMargin: messages from before start minus this are ignored.
	StartMargin = time.Minute
	// ResetConfirmWindow is how long "!reset confirm" is accepted.
	ResetConfirmWindow = time.Minute
	// MinPasswordLen for a user-chosen web UI password.
	MinPasswordLen = 8

	// TranscriptionPrefix starts every transcription reply.
	TranscriptionPrefix = "🎤 "
)

// Quote identifies a message a reply quotes.
type Quote struct {
	ID     string
	Sender types.JID
	Raw    *waE2E.Message
}

// Message is the part of an incoming WhatsApp message the bot cares about.
type Message struct {
	Chat types.JID
	// ChatAlt is the other addressing form (phone number <-> LID) of a direct
	// chat, when known. Settings are written under both.
	ChatAlt types.JID
	Sender  types.JID
	ID      string
	FromMe  bool
	IsGroup bool
	IsEdit  bool
	Time    time.Time
	Text    string
	// Voice is set for voice notes.
	Voice *waE2E.AudioMessage
	// Quoted is the message this one replies to, if any.
	Quoted *QuotedMessage
	Raw    *waE2E.Message
}

// QuotedMessage is the replied-to message as embedded in the reply.
type QuotedMessage struct {
	Quote
	Voice *waE2E.AudioMessage
}

// AsQuote returns the Quote that replies to m.
func (m *Message) AsQuote() Quote { return Quote{ID: m.ID, Sender: m.Sender, Raw: m.Raw} }

func (m *Message) chatKeys() []string {
	keys := []string{m.Chat.ToNonAD().String()}
	if !m.ChatAlt.IsEmpty() {
		keys = append(keys, m.ChatAlt.ToNonAD().String())
	}
	return keys
}

// Messenger is what the bot needs from the WhatsApp connection.
type Messenger interface {
	// Reply posts text in chat, quoting q.
	Reply(ctx context.Context, chat types.JID, q Quote, text string) error
	// Delete revokes one of the owner's messages for everyone.
	Delete(ctx context.Context, chat types.JID, id string) error
	// IsSelfChat tells whether the owner is the only member of chat.
	IsSelfChat(ctx context.Context, chat types.JID) bool
}

// Voice is a voice note to transcribe.
type Voice struct {
	Chat  types.JID
	Quote Quote
	Audio *waE2E.AudioMessage
}

// ErrNotConfigured is returned by a Transcriber without provider/API key.
var ErrNotConfigured = errors.New("transcription not configured")

// Transcriber turns a voice note into the reply text (without prefix).
type Transcriber interface {
	Transcribe(ctx context.Context, v Voice) (string, error)
	// Configured reports whether a provider and API key are set.
	Configured() bool
	// Describe is a one-line summary for !status ("openai / model, ...").
	Describe() string
}

// Bot handles messages. Handle may be called concurrently.
type Bot struct {
	WA          Messenger
	Config      *config.Store
	Status      *status.Tracker
	Transcriber Transcriber
	// Reset unlinks WhatsApp, erases everything and restarts the process.
	Reset func(ctx context.Context) error
	Addrs func() []status.Addr
	// WebPort is the web UI port ("" = web UI disabled).
	WebPort string
	Version string
	Started time.Time
	Now     func() time.Time
	Log     *slog.Logger

	mu           sync.Mutex
	pendingReset map[string]time.Time // chat -> deadline
	seen         map[string]struct{}
	seenOrder    []string
	warnedNoTr   bool
}

const maxSeen = 1000

func (b *Bot) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func (b *Bot) log() *slog.Logger {
	if b.Log != nil {
		return b.Log
	}
	return slog.Default()
}

func (b *Bot) recent(t time.Time) bool {
	now := b.now()
	return now.Sub(t) <= RecentWindow && !t.Before(b.Started.Add(-StartMargin))
}

// firstTime records id and reports whether it was new.
func (b *Bot) firstTime(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seen == nil {
		b.seen = make(map[string]struct{})
	}
	if _, ok := b.seen[id]; ok {
		return false
	}
	b.seen[id] = struct{}{}
	b.seenOrder = append(b.seenOrder, id)
	if len(b.seenOrder) > maxSeen {
		delete(b.seen, b.seenOrder[0])
		b.seenOrder = b.seenOrder[1:]
	}
	return true
}

// Handle processes one incoming message.
func (b *Bot) Handle(ctx context.Context, m *Message) {
	if b.Status != nil {
		b.Status.MessageSeen(m.Time)
	}
	if m.IsEdit || !b.recent(m.Time) {
		return
	}
	if !b.firstTime(m.Chat.String() + "/" + m.ID) {
		return
	}
	switch {
	case m.Voice != nil:
		b.onVoice(ctx, m)
	case m.FromMe && strings.HasPrefix(strings.TrimSpace(m.Text), "!"):
		b.onCommand(ctx, m)
	}
}

func (b *Bot) reply(ctx context.Context, m *Message, text string) {
	if err := b.WA.Reply(ctx, m.Chat, m.AsQuote(), text); err != nil {
		b.log().Error("reply failed", "chat", m.Chat, "err", err)
	}
}

// -- voice notes ------------------------------------------------------------

func (b *Bot) onVoice(ctx context.Context, m *Message) {
	tr := b.Config.Get().Transcription
	if !tr.Effective(m.chatKeys()...) {
		b.log().Debug("voice note: auto-transcription off", "chat", m.Chat)
		return
	}
	b.transcribe(ctx, m.Chat, Voice{Chat: m.Chat, Quote: m.AsQuote(), Audio: m.Voice}, nil)
}

// transcribe runs the transcriber and posts the result quoting the voice
// note. When cmd is not nil (the !t command), problems are reported to it.
func (b *Bot) transcribe(ctx context.Context, chat types.JID, v Voice, cmd *Message) {
	var text string
	err := ErrNotConfigured
	if b.Transcriber != nil && b.Transcriber.Configured() {
		text, err = b.Transcriber.Transcribe(ctx, v)
	}
	if errors.Is(err, ErrNotConfigured) {
		b.mu.Lock()
		warn := !b.warnedNoTr
		b.warnedNoTr = true
		b.mu.Unlock()
		if warn {
			b.log().Warn("transcription not configured: voice notes are ignored")
		}
		if cmd != nil {
			b.reply(ctx, cmd, "⚠️ Transcription is not configured yet (open the web UI).")
		}
		return
	}
	if err != nil {
		b.log().Error("transcription failed", "chat", chat, "msg", v.Quote.ID, "err", err)
		if cmd != nil {
			b.reply(ctx, cmd, "⚠️ Transcription failed, try again later with !t.")
		}
		return
	}
	if strings.TrimSpace(text) == "" {
		text = "(no speech recognised)"
	}
	if err := b.WA.Reply(ctx, chat, v.Quote, TranscriptionPrefix+text); err != nil {
		b.log().Error("posting transcription failed", "chat", chat, "err", err)
		return
	}
	if b.Status != nil {
		b.Status.Transcribed(b.now())
	}
}

// -- commands ---------------------------------------------------------------

func (b *Bot) onCommand(ctx context.Context, m *Message) {
	fields := strings.Fields(strings.TrimSpace(m.Text))
	name := strings.ToLower(fields[0])
	args := fields[1:]
	switch name {
	case "!t":
		b.cmdT(ctx, m)
	case "!transcription":
		b.cmdTranscription(ctx, m, lower(args))
	case "!status", "!reset", "!password", "!help":
		if !b.WA.IsSelfChat(ctx, m.Chat) {
			b.log().Info("admin command ignored outside the self chat", "cmd", name, "chat", m.Chat)
			return
		}
		switch name {
		case "!status":
			b.reply(ctx, m, b.statusText())
		case "!reset":
			b.cmdReset(ctx, m, lower(args))
		case "!password":
			b.cmdPassword(ctx, m, args)
		case "!help":
			b.reply(ctx, m, helpText)
		}
	}
}

func lower(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = strings.ToLower(a)
	}
	return out
}

const transcriptionUsage = "ℹ️ Commands:\n" +
	"• !t (reply to a voice note): transcribe it\n" +
	"• !transcription: show the state\n" +
	"• !transcription on|off: auto-transcription in this chat\n" +
	"• !transcription global on|off: default for all chats"

const helpText = transcriptionUsage + "\n\n" +
	"Only in this chat:\n" +
	"• !status: device status\n" +
	"• !password [new]: set the web UI password (random if omitted)\n" +
	"• !reset: unlink WhatsApp and erase everything"

func onoff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

func (b *Bot) cmdT(ctx context.Context, m *Message) {
	if m.Quoted == nil || m.Quoted.Voice == nil {
		b.reply(ctx, m, "ℹ️ Reply to a voice note with !t to transcribe it.")
		return
	}
	b.transcribe(ctx, m.Chat, Voice{Chat: m.Chat, Quote: m.Quoted.Quote, Audio: m.Quoted.Voice}, m)
}

func (b *Bot) cmdTranscription(ctx context.Context, m *Message, args []string) {
	switch {
	case len(args) == 0:
		tr := b.Config.Get().Transcription
		chat := "not set"
		if v, ok := tr.Override(m.chatKeys()...); ok {
			chat = onoff(v)
		}
		b.reply(ctx, m, fmt.Sprintf(
			"ℹ️ Auto-transcription\n• Global: %s\n• This chat: %s\n• Effective: %s\n\n%s",
			onoff(tr.Global), chat, onoff(tr.Effective(m.chatKeys()...)), transcriptionUsage))
	case len(args) == 1 && (args[0] == "on" || args[0] == "off"):
		on := args[0] == "on"
		err := b.Config.Update(func(c *config.Config) {
			if c.Transcription.Chats == nil {
				c.Transcription.Chats = map[string]bool{}
			}
			for _, k := range m.chatKeys() {
				c.Transcription.Chats[k] = on
			}
		})
		b.replySaved(ctx, m, err, fmt.Sprintf("✅ Auto-transcription %s in this chat.", onoff(on)))
	case len(args) == 2 && args[0] == "global" && (args[1] == "on" || args[1] == "off"):
		on := args[1] == "on"
		err := b.Config.Update(func(c *config.Config) { c.Transcription.Global = on })
		b.replySaved(ctx, m, err, fmt.Sprintf("✅ Auto-transcription %s by default.", onoff(on)))
	default:
		b.reply(ctx, m, "⚠️ Invalid arguments.\n\n"+transcriptionUsage)
	}
}

func (b *Bot) replySaved(ctx context.Context, m *Message, err error, ok string) {
	if err != nil {
		b.log().Error("saving config failed", "err", err)
		b.reply(ctx, m, "⚠️ Could not save the setting.")
		return
	}
	b.reply(ctx, m, ok)
}

func (b *Bot) cmdReset(ctx context.Context, m *Message, args []string) {
	key := m.Chat.String()
	now := b.now()
	if len(args) == 0 {
		b.mu.Lock()
		if b.pendingReset == nil {
			b.pendingReset = map[string]time.Time{}
		}
		b.pendingReset[key] = now.Add(ResetConfirmWindow)
		b.mu.Unlock()
		b.reply(ctx, m, "⚠️ This unlinks WhatsApp and erases all settings, the web password "+
			"and the WiFi networks.\nSend \"!reset confirm\" within 60 seconds to proceed.")
		return
	}
	if len(args) != 1 || args[0] != "confirm" {
		b.reply(ctx, m, "ℹ️ Usage: !reset, then !reset confirm")
		return
	}
	b.mu.Lock()
	deadline, ok := b.pendingReset[key]
	delete(b.pendingReset, key)
	b.mu.Unlock()
	if !ok || now.After(deadline) {
		b.reply(ctx, m, "ℹ️ Nothing to confirm: send !reset first.")
		return
	}
	b.reply(ctx, m, "♻️ Resetting. The device restarts in setup mode; this chat will "+
		"no longer receive replies.")
	b.log().Warn("reset requested from WhatsApp")
	if b.Reset == nil {
		return
	}
	if err := b.Reset(ctx); err != nil {
		b.log().Error("reset failed", "err", err)
	}
}

func (b *Bot) cmdPassword(ctx context.Context, m *Message, args []string) {
	var pw string
	generated := false
	switch len(args) {
	case 0:
		pw = RandomPassword()
		generated = true
	case 1:
		pw = args[0]
		if len([]rune(pw)) < MinPasswordLen {
			b.reply(ctx, m, fmt.Sprintf("⚠️ The password needs at least %d characters.", MinPasswordLen))
			b.deleteCommand(ctx, m)
			return
		}
	default:
		b.reply(ctx, m, "ℹ️ Usage: !password [new password, no spaces]")
		b.deleteCommand(ctx, m)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err == nil {
		err = b.Config.Update(func(c *config.Config) { c.Web.PasswordHash = string(hash) })
	}
	if err != nil {
		b.log().Error("setting the web password failed", "err", err)
		b.reply(ctx, m, "⚠️ Could not save the password.")
		return
	}
	if generated {
		b.reply(ctx, m, "🔑 New web UI password: "+pw)
		return
	}
	// The command carries the password in clear: remove it from the history.
	// Reply first, while the quoted message still exists.
	b.reply(ctx, m, "🔑 Web UI password updated.")
	b.deleteCommand(ctx, m)
}

func (b *Bot) deleteCommand(ctx context.Context, m *Message) {
	if err := b.WA.Delete(ctx, m.Chat, m.ID); err != nil {
		b.log().Error("deleting the command message failed", "err", err)
	}
}

// RandomPassword returns 12 characters without look-alikes (0/O, 1/l/I).
func RandomPassword() string {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	return randomFrom(alphabet, 12)
}

// randomFrom draws n characters uniformly (rejection sampling, no modulo bias).
func randomFrom(alphabet string, n int) string {
	limit := 256 - 256%len(alphabet)
	out := make([]byte, 0, n)
	var buf [32]byte
	for len(out) < n {
		_, _ = rand.Read(buf[:])
		for _, c := range buf {
			if int(c) < limit && len(out) < n {
				out = append(out, alphabet[int(c)%len(alphabet)])
			}
		}
	}
	return string(out)
}
