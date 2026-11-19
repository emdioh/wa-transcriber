package bot

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"golang.org/x/crypto/bcrypt"

	"github.com/emdioh/wa-transcriber/internal/config"
	"github.com/emdioh/wa-transcriber/internal/status"
)

var (
	t0      = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	me      = types.NewJID("393330000000", types.DefaultUserServer)
	friend  = types.NewJID("393331111111", types.DefaultUserServer)
	friendL = types.NewJID("123456789", types.HiddenUserServer)
)

type sent struct {
	chat  types.JID
	quote string
	text  string
}

type fakeWA struct {
	mu      sync.Mutex
	replies []sent
	tagged  int
	deleted []string
	self    map[types.JID]bool
}

func (f *fakeWA) Reply(_ context.Context, chat types.JID, q Quote, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = append(f.replies, sent{chat, q.ID, text})
	return nil
}

// ReplyTagged records the reply and the tag it would carry on the wire.
func (f *fakeWA) ReplyTagged(_ context.Context, chat types.JID, q Quote, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tagged++
	f.replies = append(f.replies, sent{chat, q.ID, text})
	return nil
}

func (f *fakeWA) Delete(_ context.Context, _ types.JID, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeWA) IsSelfChat(_ context.Context, chat types.JID) bool { return f.self[chat] }

func (f *fakeWA) last(t *testing.T) sent {
	t.Helper()
	if len(f.replies) == 0 {
		t.Fatal("no reply sent")
	}
	return f.replies[len(f.replies)-1]
}

type fakeTr struct {
	text string
	err  error
	got  []Voice
}

func (f *fakeTr) Configured() bool { return true }
func (f *fakeTr) Describe() string { return "fake / model" }

func (f *fakeTr) Transcribe(_ context.Context, v Voice) (string, error) {
	f.got = append(f.got, v)
	return f.text, f.err
}

type env struct {
	bot   *Bot
	wa    *fakeWA
	clock time.Time
	ids   int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	store, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{wa: &fakeWA{self: map[types.JID]bool{me: true}}, clock: t0}
	e.bot = &Bot{
		WA:      e.wa,
		Config:  store,
		Status:  status.New(t0),
		Started: t0,
		Now:     func() time.Time { return e.clock },
		Version: "test",
		WebPort: "8080",
		Addrs:   func() []status.Addr { return []status.Addr{{Interface: "eth0", IP: "192.168.1.40"}} },
	}
	return e
}

func (e *env) msg(chat types.JID, fromMe bool, text string) *Message {
	e.ids++
	sender := friend
	if fromMe {
		sender = me
	}
	return &Message{
		Chat: chat, Sender: sender, FromMe: fromMe, Text: text,
		ID: "ID" + string(rune('A'+e.ids)), Time: e.clock,
		Raw: &waE2E.Message{Conversation: &text},
	}
}

func (e *env) voice(chat types.JID, fromMe bool) *Message {
	m := e.msg(chat, fromMe, "")
	m.Voice = &waE2E.AudioMessage{PTT: new(true)}
	return m
}

func (e *env) send(m *Message) { e.bot.Handle(context.Background(), m) }

func TestCommandsFromOthersAreIgnored(t *testing.T) {
	e := newEnv(t)
	e.send(e.msg(friend, false, "!transcription global on"))
	if len(e.wa.replies) != 0 || e.bot.Config.Get().Transcription.Global {
		t.Fatal("a contact must not be able to run commands")
	}
}

func TestTranscriptionPerChatWritesBothAddressForms(t *testing.T) {
	e := newEnv(t)
	m := e.msg(friend, true, "!Transcription ON")
	m.ChatAlt = friendL
	e.send(m)
	chats := e.bot.Config.Get().Transcription.Chats
	if !chats[friend.String()] || !chats[friendL.String()] {
		t.Fatalf("chats = %v", chats)
	}
	if r := e.wa.last(t); r.chat != friend || r.quote != m.ID || !strings.Contains(r.text, "on in this chat") {
		t.Fatalf("reply = %+v", r)
	}
}

func TestTranscriptionGlobalAndState(t *testing.T) {
	e := newEnv(t)
	e.send(e.msg(friend, true, "!transcription global on"))
	if !e.bot.Config.Get().Transcription.Global {
		t.Fatal("global not set")
	}
	e.send(e.msg(friend, true, "!transcription"))
	r := e.wa.last(t).text
	if !strings.Contains(r, "Global: on") || !strings.Contains(r, "This chat: not set") ||
		!strings.Contains(r, "Effective: on") {
		t.Fatalf("state reply = %q", r)
	}
	e.send(e.msg(friend, true, "!transcription bogus"))
	if !strings.Contains(e.wa.last(t).text, "Invalid arguments") {
		t.Fatal("bad args not reported")
	}
}

func TestUnknownCommandIgnored(t *testing.T) {
	e := newEnv(t)
	e.send(e.msg(friend, true, "!whatever"))
	e.send(e.msg(friend, true, "not a command"))
	if len(e.wa.replies) != 0 {
		t.Fatalf("replies = %v", e.wa.replies)
	}
}

func TestOldAndDuplicateMessagesIgnored(t *testing.T) {
	e := newEnv(t)
	old := e.msg(friend, true, "!transcription global on")
	old.Time = t0.Add(-2 * time.Minute) // before start minus margin
	e.send(old)
	if len(e.wa.replies) != 0 {
		t.Fatal("message from before start handled")
	}
	m := e.msg(friend, true, "!transcription")
	e.send(m)
	e.send(m)
	if len(e.wa.replies) != 1 {
		t.Fatalf("duplicate handled: %d replies", len(e.wa.replies))
	}
	e.clock = t0.Add(time.Hour)
	stale := e.msg(friend, true, "!transcription")
	stale.Time = t0.Add(time.Hour - RecentWindow - time.Second)
	e.send(stale)
	if len(e.wa.replies) != 1 {
		t.Fatal("stale message handled")
	}
}

func TestEditsIgnored(t *testing.T) {
	e := newEnv(t)
	m := e.msg(friend, true, "!transcription")
	m.IsEdit = true
	e.send(m)
	if len(e.wa.replies) != 0 {
		t.Fatal("edit handled as a command")
	}
}

func TestAdminCommandsOnlyInSelfChat(t *testing.T) {
	e := newEnv(t)
	for _, c := range []string{"!status", "!reset", "!password", "!help"} {
		e.send(e.msg(friend, true, c))
	}
	if len(e.wa.replies) != 0 {
		t.Fatalf("admin command answered in a shared chat: %v", e.wa.replies)
	}
	e.send(e.msg(me, true, "!status"))
	r := e.wa.last(t)
	if r.chat != me || !strings.Contains(r.text, "eth0:\n`http://192.168.1.40:8080/`") ||
		!strings.Contains(r.text, "WhatsApp: offline") {
		t.Fatalf("status = %+v", r)
	}
}

func TestStatusURLPort(t *testing.T) {
	e := newEnv(t)
	e.bot.WebPort = "80"
	e.send(e.msg(me, true, "!status"))
	if r := e.wa.last(t).text; !strings.Contains(r, "eth0:\n`http://192.168.1.40/`") {
		t.Fatalf("port 80 must be omitted: %q", r)
	}
	e.bot.WebPort = ""
	e.send(e.msg(me, true, "!status"))
	if r := e.wa.last(t).text; !strings.Contains(r, "Web UI: disabled") {
		t.Fatalf("disabled web UI not reported: %q", r)
	}
}

func TestResetNeedsConfirmationInTime(t *testing.T) {
	e := newEnv(t)
	resets := 0
	e.bot.Reset = func(context.Context) error { resets++; return nil }

	e.send(e.msg(me, true, "!reset confirm"))
	if resets != 0 || !strings.Contains(e.wa.last(t).text, "Nothing to confirm") {
		t.Fatal("confirm without request")
	}
	e.send(e.msg(me, true, "!reset"))
	e.clock = e.clock.Add(ResetConfirmWindow + time.Second)
	e.send(e.msg(me, true, "!reset confirm"))
	if resets != 0 {
		t.Fatal("late confirmation accepted")
	}
	e.send(e.msg(me, true, "!reset"))
	e.clock = e.clock.Add(30 * time.Second)
	e.send(e.msg(me, true, "!reset confirm"))
	if resets != 1 {
		t.Fatalf("resets = %d", resets)
	}
}

func TestPasswordSetAndCommandDeleted(t *testing.T) {
	e := newEnv(t)
	m := e.msg(me, true, "!password Sup3rSecret")
	e.send(m)
	hash := e.bot.Config.Get().Web.PasswordHash
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte("Sup3rSecret")) != nil {
		t.Fatal("hash does not match")
	}
	if strings.Contains(e.wa.last(t).text, "Sup3rSecret") {
		t.Fatal("password echoed back")
	}
	if len(e.wa.deleted) != 1 || e.wa.deleted[0] != m.ID {
		t.Fatalf("command not deleted: %v", e.wa.deleted)
	}
}

func TestPasswordTooShortRejectedAndDeleted(t *testing.T) {
	e := newEnv(t)
	e.send(e.msg(me, true, "!password short"))
	if e.bot.Config.Get().Web.PasswordHash != "" {
		t.Fatal("short password accepted")
	}
	if len(e.wa.deleted) != 1 {
		t.Fatal("rejected password still visible in the chat")
	}
}

func TestPasswordGenerated(t *testing.T) {
	e := newEnv(t)
	e.send(e.msg(me, true, "!password"))
	text := e.wa.last(t).text
	pw := strings.TrimPrefix(text, "🔑 New web UI password: ")
	if len(pw) != 12 {
		t.Fatalf("reply = %q", text)
	}
	if bcrypt.CompareHashAndPassword([]byte(e.bot.Config.Get().Web.PasswordHash), []byte(pw)) != nil {
		t.Fatal("generated password does not match the hash")
	}
	if len(e.wa.deleted) != 0 {
		t.Fatal("nothing secret in the command, it should stay")
	}
}

func TestVoiceAutoTranscription(t *testing.T) {
	e := newEnv(t)
	tr := &fakeTr{text: "ciao"}
	e.bot.Transcriber = tr

	e.send(e.voice(friend, false))
	if len(tr.got) != 0 {
		t.Fatal("transcribed while off")
	}
	_ = e.bot.Config.Update(func(c *config.Config) { c.Transcription.Global = true })
	v := e.voice(friend, false)
	e.send(v)
	r := e.wa.last(t)
	if r.quote != v.ID || r.text != TranscriptionPrefix+"ciao" {
		t.Fatalf("reply = %+v", r)
	}
	if e.bot.Status.Snapshot().LastTranscription.IsZero() {
		t.Fatal("last transcription not tracked")
	}
}

func TestTCommand(t *testing.T) {
	e := newEnv(t)
	e.send(e.msg(friend, true, "!t"))
	if !strings.Contains(e.wa.last(t).text, "Reply to a voice note") {
		t.Fatal("hint missing")
	}

	cmd := e.msg(friend, true, "!t")
	cmd.Quoted = &QuotedMessage{Quote: Quote{ID: "VOICE", Sender: friend}, Voice: &waE2E.AudioMessage{}}
	e.send(cmd)
	if !strings.Contains(e.wa.last(t).text, "not configured") {
		t.Fatal("unconfigured transcription not reported")
	}

	tr := &fakeTr{err: errors.New("boom")}
	e.bot.Transcriber = tr
	cmd = e.msg(friend, true, "!t")
	cmd.Quoted = &QuotedMessage{Quote: Quote{ID: "VOICE", Sender: friend}, Voice: &waE2E.AudioMessage{}}
	e.send(cmd)
	if !strings.Contains(e.wa.last(t).text, "failed") {
		t.Fatal("failure not reported")
	}

	tr.err, tr.text = nil, "hello"
	cmd = e.msg(friend, true, "!t")
	cmd.Quoted = &QuotedMessage{Quote: Quote{ID: "VOICE", Sender: friend}, Voice: &waE2E.AudioMessage{}}
	e.send(cmd)
	if r := e.wa.last(t); r.quote != "VOICE" || r.text != TranscriptionPrefix+"hello" {
		t.Fatalf("transcription must quote the voice note: %+v", r)
	}
}

func TestRandomPassword(t *testing.T) {
	a, b := RandomPassword(), RandomPassword()
	if len(a) != 12 || a == b || strings.ContainsAny(a, "0O1lI") {
		t.Fatalf("%q %q", a, b)
	}
}
