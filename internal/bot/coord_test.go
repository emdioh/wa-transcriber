package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"

	"github.com/emdioh/wa-transcriber/internal/config"
)

// fakeCoord records what the bot reports and decides through Lease.
type fakeCoord struct {
	lease     bool // grant or refuse the lease
	stillMine bool
	claims    []string
	transcr   []string
	acquired  int
}

func (f *fakeCoord) Acquire(context.Context, Voice) Lease {
	f.acquired++
	if !f.lease {
		return nil
	}
	return fakeLease{f.stillMine}
}

func (f *fakeCoord) ObserveClaim(_ types.JID, voiceID string, _ types.JID, _ time.Time) {
	f.claims = append(f.claims, voiceID)
}

func (f *fakeCoord) ObserveTranscription(_ types.JID, voiceID string, _ types.JID) {
	f.transcr = append(f.transcr, voiceID)
}

func (f *fakeCoord) Counters() (uint64, uint64, uint64, string) { return 1, 2, 3, "observe-only" }

type fakeLease struct{ mine bool }

func (l fakeLease) StillMine() bool { return l.mine }

func coordEnv(t *testing.T) (*env, *fakeCoord, *fakeTr) {
	e := newEnv(t)
	co := &fakeCoord{lease: true, stillMine: true}
	tr := &fakeTr{text: "hello"}
	e.bot.Coord = co
	e.bot.Transcriber = tr
	return e, co, tr
}

func TestVoiceNotePostsTaggedWhenLeaseHolds(t *testing.T) {
	e, co, tr := coordEnv(t)
	_ = e.bot.Config.Update(func(c *config.Config) { c.Transcription.Global = true })
	e.bot.Handle(context.Background(), e.voice(me, false))
	if co.acquired != 1 || len(tr.got) != 1 {
		t.Fatalf("acquired=%d transcribed=%d", co.acquired, len(tr.got))
	}
	if e.wa.tagged != 1 {
		t.Fatalf("transcription not tagged: %+v", e.wa.replies)
	}
	if got := e.wa.last(t); !strings.HasPrefix(got.text, TranscriptionPrefix) {
		t.Fatalf("reply = %q", got.text)
	}
}

func TestVoiceNoteSkippedWhenPeerOwnsIt(t *testing.T) {
	e, co, tr := coordEnv(t)
	_ = e.bot.Config.Update(func(c *config.Config) { c.Transcription.Global = true })
	co.lease = false
	e.bot.Handle(context.Background(), e.voice(me, false))
	if len(tr.got) != 0 || len(e.wa.replies) != 0 {
		t.Fatalf("transcribed a peer's note: %+v", e.wa.replies)
	}
}

func TestVoiceNoteDroppedWhenLeaseLostMidway(t *testing.T) {
	e, co, tr := coordEnv(t)
	_ = e.bot.Config.Update(func(c *config.Config) { c.Transcription.Global = true })
	co.stillMine = false
	e.bot.Handle(context.Background(), e.voice(me, false))
	if len(tr.got) != 1 {
		t.Fatalf("expected one transcription attempt, got %d", len(tr.got))
	}
	if len(e.wa.replies) != 0 {
		t.Fatalf("posted despite losing the lease: %+v", e.wa.replies)
	}
}

func TestClaimsAndTaggedRepliesAreRoutedNotTreatedAsCommands(t *testing.T) {
	e, co, _ := coordEnv(t)
	_ = e.bot.Config.Update(func(c *config.Config) { c.Transcription.Global = true })

	claim := e.msg(friend, false, "")
	claim.Claim = &ClaimInfo{VoiceID: "VOICE1", Chat: friend}
	e.bot.Handle(context.Background(), claim)

	reply := e.msg(friend, false, "🎤 text")
	reply.TranscriptionOf = "VOICE2"
	e.bot.Handle(context.Background(), reply)

	if len(co.claims) != 1 || co.claims[0] != "VOICE1" {
		t.Fatalf("claims = %v", co.claims)
	}
	if len(co.transcr) != 1 || co.transcr[0] != "VOICE2" {
		t.Fatalf("transcriptions = %v", co.transcr)
	}
	if e.wa.tagged != 0 || len(e.wa.replies) != 0 {
		t.Fatalf("a coordination signal produced a reply: %+v", e.wa.replies)
	}
}

func TestOwnCoordinationMessagesIgnored(t *testing.T) {
	e, co, _ := coordEnv(t)
	claim := e.msg(friend, true, "")
	claim.Claim = &ClaimInfo{VoiceID: "VOICE3", Chat: friend}
	e.bot.Handle(context.Background(), claim)
	reply := e.msg(friend, true, "🎤 text")
	reply.TranscriptionOf = "VOICE4"
	e.bot.Handle(context.Background(), reply)
	if len(co.claims) != 0 || len(co.transcr) != 0 {
		t.Fatalf("own signals fed back: %v %v", co.claims, co.transcr)
	}
}

func TestCommandTranscribeBypassesCoordinator(t *testing.T) {
	e, co, tr := coordEnv(t)
	_ = e.bot.Config.Update(func(c *config.Config) { c.Transcription.Global = true })
	cmd := e.msg(me, true, "!t")
	cmd.Quoted = &QuotedMessage{Quote: Quote{ID: "VOICE5", Sender: friend}, Voice: e.voice(me, false).Voice}
	e.bot.Handle(context.Background(), cmd)
	if co.acquired != 0 {
		t.Fatal("!t must bypass coordination")
	}
	if len(tr.got) != 1 {
		t.Fatal("!t did not transcribe")
	}
}

func TestStatusShowsCoordinationCounters(t *testing.T) {
	e, _, _ := coordEnv(t)
	got := e.bot.statusText()
	if !strings.Contains(got, "Coordination: observe-only, claims 1, stood down 2, took over 3") {
		t.Fatalf("status = %s", got)
	}
}
