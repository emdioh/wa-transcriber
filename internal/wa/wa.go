// Package wa is the WhatsApp connection: a whatsmeow companion device whose
// session lives in a pure-Go SQLite file. It translates whatsmeow events into
// bot.Message values and implements bot.Messenger.
package wa

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/emdioh/wa-transcriber/internal/bot"
	"github.com/emdioh/wa-transcriber/internal/status"
)

// DeviceName is shown in the phone's "Linked devices" list.
const DeviceName = "WA Transcriber"

// Options configures Open.
type Options struct {
	DBPath string
	Log    *slog.Logger
	Status *status.Tracker
	// QROut receives a terminal rendering of pairing QR codes (nil: none).
	QROut io.Writer
}

// Client wraps a whatsmeow client.
type Client struct {
	opts      Options
	container *sqlstore.Container
	cli       *whatsmeow.Client
	log       *slog.Logger

	// OnMessage is called (in its own goroutine) for every incoming message.
	OnMessage func(*bot.Message)
	// OnLoggedOut is called when the phone unlinks this device.
	OnLoggedOut func()

	groupMu    sync.Mutex
	groupCache map[types.JID]groupEntry

	pairMu       sync.Mutex
	pairPhone    string // pair with a code for this number instead of the QR
	pairCodeSent bool   // a code is on screen for the current attempt
	pairReady    bool   // connected and waiting to be paired
}

type groupEntry struct {
	selfOnly bool
	name     string
	expires  time.Time
}

const groupCacheTTL = 2 * time.Minute

func init() {
	// Only new messages matter: ask the phone for as little history as possible.
	store.DeviceProps.Os = proto.String(DeviceName)
	store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_DESKTOP.Enum()
	store.DeviceProps.RequireFullSync = proto.Bool(false)
	if h := store.DeviceProps.HistorySyncConfig; h != nil {
		h.FullSyncDaysLimit = proto.Uint32(1)
		h.RecentSyncDaysLimit = proto.Uint32(1)
		h.InitialSyncMaxMessagesPerChat = proto.Uint32(1)
	}
}

// Open opens (or creates) the session database.
func Open(ctx context.Context, opts Options) (*Client, error) {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	dsn := "file:" + opts.DBPath +
		"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	container, err := sqlstore.New(ctx, "sqlite", dsn, newLogger(log.With("module", "db"), slog.LevelWarn))
	if err != nil {
		return nil, fmt.Errorf("open session db: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		container.Close()
		return nil, fmt.Errorf("load device: %w", err)
	}
	c := &Client{
		opts:       opts,
		container:  container,
		log:        log,
		groupCache: map[types.JID]groupEntry{},
	}
	c.cli = whatsmeow.NewClient(device, newLogger(log.With("module", "whatsmeow"), slog.LevelInfo))
	c.cli.EnableAutoReconnect = true
	c.cli.ManualHistorySyncDownload = true // never download history blobs
	c.cli.AddEventHandler(c.onEvent)
	return c, nil
}

// Paired reports whether the device is linked to an account.
func (c *Client) Paired() bool { return c.cli.Store.ID != nil }

// Run connects, pairing first if needed (QR codes, or a pairing code when
// pairPhone is set), and returns once connected. Pairing attempts repeat
// until ctx ends: a headless device must never give up.
func (c *Client) Run(ctx context.Context, pairPhone string) error {
	c.pairMu.Lock()
	c.pairPhone = pairPhone
	c.pairMu.Unlock()
	for !c.Paired() {
		if err := c.pairOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			c.log.Warn("pairing attempt ended", "err", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
	}
	if c.cli.IsConnected() {
		return nil
	}
	return c.cli.ConnectContext(ctx)
}

var errPairTimeout = errors.New("pairing codes expired")

// ErrNotReady: the pairing connection is not up yet (or already paired).
var ErrNotReady = errors.New("WhatsApp is not waiting for pairing right now, try again in a few seconds")

func (c *Client) pairOnce(ctx context.Context) error {
	qrCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := c.cli.GetQRChannel(qrCtx)
	if err != nil {
		return err
	}
	if err := c.cli.ConnectContext(ctx); err != nil {
		return err
	}
	c.pairMu.Lock()
	c.pairCodeSent = false
	phone := c.pairPhone
	c.pairMu.Unlock()
	defer func() {
		c.pairMu.Lock()
		c.pairReady = false
		c.pairMu.Unlock()
		c.setPairing("")
	}()
	first := true
	for item := range ch {
		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			if first {
				first = false
				c.pairMu.Lock()
				c.pairReady = true
				c.pairMu.Unlock()
				if phone != "" {
					if err := c.requestCode(ctx, phone); err != nil {
						c.log.Error("pairing code request failed", "err", err)
					}
				}
			}
			c.pairMu.Lock()
			codeOnScreen := c.pairCodeSent
			c.pairMu.Unlock()
			if !codeOnScreen {
				c.setPairing("qr:" + item.Code)
				c.log.Info("scan the QR code: WhatsApp → Linked devices → Link a device", "expires_in", item.Timeout)
				if c.opts.QROut != nil {
					qrterminal.GenerateHalfBlock(item.Code, qrterminal.L, c.opts.QROut)
				}
			}
		case whatsmeow.QRChannelSuccess.Event:
			c.log.Info("paired", "jid", c.cli.Store.ID)
			return nil
		case whatsmeow.QRChannelTimeout.Event:
			return errPairTimeout
		case whatsmeow.QRChannelEventError:
			return fmt.Errorf("pairing error: %w", item.Error)
		default:
			c.log.Info("pairing event", "event", item.Event)
		}
	}
	if c.Paired() {
		return nil
	}
	return errors.New("pairing channel closed")
}

func (c *Client) requestCode(ctx context.Context, phone string) error {
	code, err := c.cli.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
	if err != nil {
		return err
	}
	c.pairMu.Lock()
	c.pairCodeSent = true
	c.pairMu.Unlock()
	c.log.Info("pairing code: on the phone open Linked devices → Link with phone number", "code", code)
	c.setPairing("code:" + code)
	return nil
}

// RequestPairingCode switches pairing to "link with phone number" for phone
// (with country code). Later attempts keep using it until UseQR is called.
func (c *Client) RequestPairingCode(ctx context.Context, phone string) error {
	c.pairMu.Lock()
	ready := c.pairReady && !c.Paired()
	if ready {
		c.pairPhone = phone
	}
	c.pairMu.Unlock()
	if !ready {
		return ErrNotReady
	}
	return c.requestCode(ctx, phone)
}

// UseQR goes back to QR pairing (from the next QR code on).
func (c *Client) UseQR() {
	c.pairMu.Lock()
	c.pairPhone = ""
	c.pairCodeSent = false
	c.pairMu.Unlock()
}

func (c *Client) setPairing(code string) {
	if c.opts.Status != nil {
		c.opts.Status.SetPairingCode(code)
	}
}

// Close disconnects and closes the database.
func (c *Client) Close() {
	c.cli.Disconnect()
	if err := c.container.Close(); err != nil {
		c.log.Warn("closing session db", "err", err)
	}
}

// Logout unlinks the device (best effort) and removes the session files.
func (c *Client) Logout(ctx context.Context) {
	if c.Paired() {
		if err := c.cli.Logout(ctx); err != nil {
			c.log.Warn("logout failed, deleting the session locally", "err", err)
		}
	}
	c.Close()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(c.opts.DBPath + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			c.log.Warn("removing session file", "file", c.opts.DBPath+suffix, "err", err)
		}
	}
}

// -- events -----------------------------------------------------------------

func (c *Client) onEvent(evt any) {
	st := c.opts.Status
	switch v := evt.(type) {
	case *events.Message:
		m := convert(v)
		if m == nil || c.OnMessage == nil {
			return
		}
		go c.OnMessage(m)
	case *events.Connected:
		c.log.Info("connected to WhatsApp")
		if st != nil {
			st.SetConnected(true)
			if id := c.cli.Store.ID; id != nil {
				st.SetAccount(id.User)
			}
		}
	case *events.Disconnected:
		c.log.Warn("disconnected from WhatsApp")
		if st != nil {
			st.SetConnected(false)
		}
	case *events.LoggedOut:
		c.log.Warn("logged out by the phone", "reason", v.Reason)
		if st != nil {
			st.SetConnected(false)
			st.SetAccount("")
		}
		if c.OnLoggedOut != nil {
			go c.OnLoggedOut()
		}
	case *events.StreamReplaced:
		c.log.Error("another client with this session connected: stream replaced")
	case *events.TemporaryBan:
		c.log.Error("temporary ban", "ban", v.String())
	case *events.HistorySync:
		// Ignored on purpose (ManualHistorySyncDownload): nothing is downloaded.
	}
}

// convert extracts the fields the bot needs; nil for messages to ignore.
func convert(v *events.Message) *bot.Message {
	info := v.Info
	if info.Chat.Server == types.BroadcastServer || info.Chat.Server == types.NewsletterServer {
		return nil // status updates, broadcast lists, channels
	}
	msg := v.Message
	if msg == nil {
		return nil
	}
	m := &bot.Message{
		Chat:    info.Chat,
		Sender:  info.Sender,
		ID:      info.ID,
		FromMe:  info.IsFromMe,
		IsGroup: info.IsGroup,
		IsEdit:  v.IsEdit,
		Time:    info.Timestamp,
		Raw:     msg,
	}
	if !info.IsGroup {
		if info.IsFromMe {
			m.ChatAlt = info.RecipientAlt
		} else {
			m.ChatAlt = info.SenderAlt
		}
	}
	switch {
	case msg.GetConversation() != "":
		m.Text = msg.GetConversation()
	case msg.GetExtendedTextMessage() != nil:
		ext := msg.GetExtendedTextMessage()
		m.Text = ext.GetText()
		m.Quoted = quoted(info, ext.GetContextInfo())
	}
	if a := msg.GetAudioMessage(); isVoice(a) {
		m.Voice = a
	}
	return m
}

func quoted(info types.MessageInfo, ci *waE2E.ContextInfo) *bot.QuotedMessage {
	if ci.GetStanzaID() == "" {
		return nil
	}
	q := &bot.QuotedMessage{Quote: bot.Quote{ID: ci.GetStanzaID(), Raw: ci.GetQuotedMessage()}}
	if p := ci.GetParticipant(); p != "" {
		if jid, err := types.ParseJID(p); err == nil {
			q.Sender = jid
		}
	}
	if q.Sender.IsEmpty() && !info.IsGroup {
		q.Sender = info.Chat
	}
	if a := ci.GetQuotedMessage().GetAudioMessage(); isVoice(a) {
		q.Voice = a
	}
	return q
}

// isVoice: push-to-talk notes, or Ogg/Opus audio whose PTT flag got lost.
func isVoice(a *waE2E.AudioMessage) bool {
	if a == nil {
		return false
	}
	if a.GetPTT() {
		return true
	}
	mime := strings.ToLower(strings.TrimSpace(strings.SplitN(a.GetMimetype(), ";", 2)[0]))
	return mime == "audio/ogg" || mime == "audio/opus"
}

// DownloadAudio fetches and decrypts a voice note (transcribe.Downloader).
func (c *Client) DownloadAudio(ctx context.Context, a *waE2E.AudioMessage) ([]byte, error) {
	return c.cli.Download(ctx, a)
}

// -- bot.Messenger ----------------------------------------------------------

// Reply posts text in chat quoting q.
func (c *Client) Reply(ctx context.Context, chat types.JID, q bot.Quote, text string) error {
	ci := &waE2E.ContextInfo{StanzaID: proto.String(q.ID), QuotedMessage: q.Raw}
	if !q.Sender.IsEmpty() {
		ci.Participant = proto.String(q.Sender.ToNonAD().String())
	}
	_, err := c.cli.SendMessage(ctx, chat, &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String(text), ContextInfo: ci},
	})
	return err
}

// Delete revokes one of the owner's messages for everyone.
func (c *Client) Delete(ctx context.Context, chat types.JID, id string) error {
	_, err := c.cli.SendMessage(ctx, chat, c.cli.BuildRevoke(chat, types.EmptyJID, id))
	return err
}

// CanonicalChat returns the phone-number form of a LID chat when known, so
// the two keys of one chat can be shown as one.
func (c *Client) CanonicalChat(ctx context.Context, chat types.JID) types.JID {
	if chat.Server == types.HiddenUserServer {
		if pn, err := c.cli.Store.LIDs.GetPNForLID(ctx, chat); err == nil && !pn.IsEmpty() {
			return pn.ToNonAD()
		}
	}
	return chat.ToNonAD()
}

// ChatName is a human name for a chat ("" if unknown): contact name, or the
// group subject (cached).
func (c *Client) ChatName(ctx context.Context, chat types.JID) string {
	switch chat.Server {
	case types.DefaultUserServer, types.HiddenUserServer:
		ci, err := c.cli.Store.Contacts.GetContact(ctx, chat)
		if err != nil || !ci.Found {
			return ""
		}
		for _, n := range []string{ci.FullName, ci.FirstName, ci.PushName, ci.BusinessName} {
			if n != "" {
				return n
			}
		}
	case types.GroupServer:
		c.groupMu.Lock()
		e, ok := c.groupCache[chat]
		c.groupMu.Unlock()
		if ok && time.Now().Before(e.expires) {
			return e.name
		}
		if !c.cli.IsConnected() {
			return ""
		}
		info, err := c.cli.GetGroupInfo(ctx, chat)
		if err != nil {
			return ""
		}
		c.cacheGroup(chat, info)
		return info.Name
	}
	return ""
}

func (c *Client) cacheGroup(chat types.JID, info *types.GroupInfo) {
	selfOnly := len(info.Participants) == 1 && c.isMe(info.Participants[0])
	c.groupMu.Lock()
	c.groupCache[chat] = groupEntry{selfOnly: selfOnly, name: info.Name, expires: time.Now().Add(groupCacheTTL)}
	c.groupMu.Unlock()
}

func (c *Client) isMe(p types.GroupParticipant) bool {
	for _, j := range []types.JID{p.JID, p.PhoneNumber, p.LID} {
		if c.isOwnJID(j) {
			return true
		}
	}
	return false
}

func (c *Client) isOwnJID(j types.JID) bool {
	if j.IsEmpty() {
		return false
	}
	if id := c.cli.Store.ID; id != nil && j.Server == types.DefaultUserServer && j.User == id.User {
		return true
	}
	lid := c.cli.Store.LID
	return !lid.IsEmpty() && j.Server == types.HiddenUserServer && j.User == lid.User
}

// IsSelfChat: the "Message yourself" chat or a group whose only member is
// the owner. Group lookups are cached briefly.
func (c *Client) IsSelfChat(ctx context.Context, chat types.JID) bool {
	switch chat.Server {
	case types.DefaultUserServer, types.HiddenUserServer:
		return c.isOwnJID(chat)
	case types.GroupServer:
	default:
		return false
	}
	c.groupMu.Lock()
	e, ok := c.groupCache[chat]
	c.groupMu.Unlock()
	if ok && time.Now().Before(e.expires) {
		return e.selfOnly
	}
	info, err := c.cli.GetGroupInfo(ctx, chat)
	if err != nil {
		c.log.Warn("group info lookup failed", "chat", chat, "err", err)
		return false
	}
	c.cacheGroup(chat, info)
	return len(info.Participants) == 1 && c.isMe(info.Participants[0])
}
