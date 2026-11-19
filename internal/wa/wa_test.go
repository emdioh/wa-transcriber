package wa

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
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
