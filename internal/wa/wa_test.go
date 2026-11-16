package wa

import (
	"testing"
	"time"

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
