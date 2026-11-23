package wa

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWa6"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

var (
	me     = types.NewJID("393330000000", types.DefaultUserServer)
	friend = types.NewJID("393331111111", types.DefaultUserServer)
	lid    = types.NewJID("98765", types.HiddenUserServer)
)

func TestConvertCommandReplyingToVoice(t *testing.T) {
	voice := &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true)}}
	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: friend, Sender: me, IsFromMe: true, RecipientAlt: lid},
			ID:            "CMD", Timestamp: time.Unix(1, 0),
		},
		Message: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text:        proto.String("!t"),
			ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("VOICE"), QuotedMessage: voice},
		}},
	}
	m := convert(evt)
	if m.Text != "!t" || !m.FromMe || m.ChatAlt != lid {
		t.Fatalf("m = %+v", m)
	}
	if m.Quoted == nil || m.Quoted.ID != "VOICE" || m.Quoted.Voice == nil {
		t.Fatalf("quoted = %+v", m.Quoted)
	}
	if m.Quoted.Sender != friend {
		t.Fatalf("DM quote without participant must fall back to the chat, got %v", m.Quoted.Sender)
	}
}

func TestConvertIgnoresStatusBroadcast(t *testing.T) {
	evt := &events.Message{
		Info:    types.MessageInfo{MessageSource: types.MessageSource{Chat: types.StatusBroadcastJID, Sender: friend}},
		Message: &waE2E.Message{Conversation: proto.String("hi")},
	}
	if convert(evt) != nil {
		t.Fatal("status updates must be ignored")
	}
}

func TestIsVoice(t *testing.T) {
	cases := []struct {
		a    *waE2E.AudioMessage
		want bool
	}{
		{nil, false},
		{&waE2E.AudioMessage{PTT: proto.Bool(true)}, true},
		{&waE2E.AudioMessage{Mimetype: proto.String("audio/ogg; codecs=opus")}, true},
		{&waE2E.AudioMessage{Mimetype: proto.String("audio/mpeg")}, false},
	}
	for _, c := range cases {
		if got := isVoice(c.a); got != c.want {
			t.Errorf("isVoice(%v) = %v", c.a, got)
		}
	}
}

func TestConvertClaim(t *testing.T) {
	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: friend, Sender: friend},
			ID:            "CLAIM", Timestamp: time.Unix(1, 0),
		},
		Message: &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
			Key:         &waCommon.MessageKey{RemoteJID: proto.String(friend.String()), ID: proto.String("VOICE1"), FromMe: proto.Bool(false)},
			Text:        proto.String(""),
			GroupingKey: proto.String(ClaimGroupingKey),
		}},
	}
	m := convert(evt)
	if m == nil || m.Claim == nil {
		t.Fatalf("claim not recognised: %+v", m)
	}
	if m.Claim.VoiceID != "VOICE1" || m.Claim.Chat != friend {
		t.Fatalf("claim = %+v", m.Claim)
	}
	if m.Text != "" || m.TranscriptionOf != "" {
		t.Fatalf("a claim must not look like text: %+v", m)
	}
}

func TestConvertIgnoresOrdinaryReactions(t *testing.T) {
	evt := &events.Message{
		Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: friend, Sender: friend}},
		Message: &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
			Key:  &waCommon.MessageKey{RemoteJID: proto.String(friend.String()), ID: proto.String("VOICE1")},
			Text: proto.String("👍"),
		}},
	}
	if m := convert(evt); m == nil || m.Claim != nil {
		t.Fatalf("ordinary reaction treated as a claim: %+v", m)
	}
}

func TestConvertTaggedTranscription(t *testing.T) {
	evt := &events.Message{
		Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: friend, Sender: friend}},
		Message: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String("🎤 hello"),
			ContextInfo: &waE2E.ContextInfo{
				StanzaID:         proto.String("VOICE2"),
				ConversionSource: proto.String(TagSource),
			},
		}},
	}
	m := convert(evt)
	if m == nil || m.TranscriptionOf != "VOICE2" {
		t.Fatalf("tag not recognised: %+v", m)
	}
}

func TestConvertLegacyMegachatTranscription(t *testing.T) {
	evt := &events.Message{
		Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: friend, Sender: friend}},
		Message: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text:        proto.String(LegacyPrefix + " ciao a tutti"),
			ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("VOICE3")},
		}},
	}
	m := convert(evt)
	if m == nil || m.TranscriptionOf != "VOICE3" {
		t.Fatalf("legacy prefix not recognised: %+v", m)
	}
}

func TestConvertPlainReplyIsNotATranscription(t *testing.T) {
	evt := &events.Message{
		Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: friend, Sender: friend}},
		Message: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text:        proto.String("just a reply"),
			ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("VOICE4")},
		}},
	}
	if m := convert(evt); m == nil || m.TranscriptionOf != "" {
		t.Fatalf("plain reply treated as a transcription: %+v", m)
	}
}

// The phone's "Linked devices" list shows "<browser> (<os>)": a plain
// WhatsApp Web session in Chrome on Ubuntu is what this device claims to be,
// and the library's current app version travels with it.
func TestDevicePresentsAsChromeOnUbuntu(t *testing.T) {
	dp := store.DeviceProps
	if got := dp.GetPlatformType(); got != waCompanionReg.DeviceProps_CHROME {
		t.Errorf("PlatformType = %v, want CHROME", got)
	}
	if got := dp.GetOs(); got != "Ubuntu" {
		t.Errorf("Os = %q, want Ubuntu", got)
	}
	if got := dp.GetOs(); got == DeviceName {
		t.Errorf("Os still names the tool: %q", got)
	}
	v := store.GetWAVersion()
	av := dp.GetVersion()
	if av.GetPrimary() != v[0] || av.GetSecondary() != v[1] || av.GetTertiary() != v[2] {
		t.Errorf("AppVersion = %d.%d.%d, want %s", av.GetPrimary(), av.GetSecondary(), av.GetTertiary(), v)
	}
	if dp.GetRequireFullSync() {
		t.Error("RequireFullSync is on: the phone would push the whole history")
	}
	// The request for as little history as possible must survive the change.
	if h := dp.GetHistorySyncConfig(); h.GetFullSyncDaysLimit() != 1 ||
		h.GetRecentSyncDaysLimit() != 1 || h.GetInitialSyncMaxMessagesPerChat() != 1 {
		t.Errorf("history limits = %d/%d/%d", h.GetFullSyncDaysLimit(), h.GetRecentSyncDaysLimit(),
			h.GetInitialSyncMaxMessagesPerChat())
	}
	// The web-client user agent is what makes the server treat the connection
	// as a browser session at all; it must not be overridden to something else.
	ua := store.BaseClientPayload.GetUserAgent()
	if ua.GetPlatform() != waWa6.ClientPayload_UserAgent_WEB {
		t.Errorf("UserAgent platform = %v, want WEB", ua.GetPlatform())
	}
	if got := store.BaseClientPayload.GetWebInfo().GetWebSubPlatform(); got != waWa6.ClientPayload_WebInfo_WEB_BROWSER {
		t.Errorf("WebSubPlatform = %v, want WEB_BROWSER", got)
	}
	if got := ua.GetOsVersion(); got != v.String() {
		t.Errorf("UserAgent OsVersion = %q, want %q", got, v.String())
	}
}
