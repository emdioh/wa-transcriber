// Package coord decides which instance transcribes a voice note when several
// people in the same chat run wa-transcriber.
//
// The contract is deliberately small: the bot calls Acquire before
// transcribing and StillMine before posting, and feeds every claim and every
// tagged transcription it sees back into the coordinator. Everything else
// (delay, settle window, watchdog takeover) is internal.
package coord

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"log/slog"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/types"
)

// Voice identifies the voice note being coordinated.
type Voice struct {
	Chat    types.JID
	ID      string
	Sender  types.JID
	FromMe  bool // the owner sent the voice note
	Seconds int  // AudioMessage.Seconds, used for the watchdog
}

// Signals is the wire format: only Claim is needed here. Changing a
// transcription is done by the caller (wa.ReplyTagged).
type Signals interface {
	// Claim publishes "I am transcribing voiceID" without anything visible.
	Claim(ctx context.Context, chat, voiceSender types.JID, voiceID string) error
}

// Coordinator serialises transcription between instances. It is safe for
// concurrent use.
type Coordinator struct {
	// Self returns our own JIDs (phone-number and LID form).
	Self func() []types.JID
	// Signals sends a claim; nil in observe-only mode.
	Signals Signals
	Now     func() time.Time
	After   func(time.Duration) <-chan time.Time
	Log     *slog.Logger
	// ClaimMode false = observe-only: never claim, but still skip when a
	// tagged transcription already exists (M2 fallback).
	ClaimMode bool

	// Timings; zero values use the defaults below.
	DelayBase    time.Duration // 1 s
	DelaySpread  time.Duration // 3 s
	Settle       time.Duration // 1.5 s
	WatchdogBase time.Duration // 45 s
	Keep         time.Duration // 10 min of state

	mu       sync.Mutex
	state    map[string]*record
	counters Counters
}

const (
	defaultDelayBase    = time.Second
	defaultDelaySpread  = 3 * time.Second
	defaultSettle       = 1500 * time.Millisecond
	defaultWatchdogBase = 45 * time.Second
	defaultKeep         = 10 * time.Minute
)

// Lease is the right to post a transcription. StillMine must be checked right
// before posting: a peer may have won meanwhile.
type Lease struct {
	c     *Coordinator
	rec   *record
	rank  uint64
	claim bool
}

// StillMine reports whether this instance should still post the result.
func (l *Lease) StillMine() bool {
	if l == nil || l.c == nil {
		return false
	}
	return l.c.stillMine(l.rec, l.rank, l.claim)
}

// Counters are the coordination numbers shown by !status.
type Counters struct {
	Claims, StoodDown, TookOver uint64
}

// record is the shared state of one voice note.
type record struct {
	key         string
	ch          chan struct{} // closed and replaced on every change
	peerClaims  map[string]uint64
	transcribed bool
	selfClaimed bool
	lastTouch   time.Time
}

func (r *record) notify() {
	close(r.ch)
	r.ch = make(chan struct{})
}

// Acquire blocks until this instance should transcribe v (returns a Lease) or
// another instance is taking care of it (returns nil). ctx aborts the wait.
func (c *Coordinator) Acquire(ctx context.Context, v Voice) *Lease {
	if v.ID == "" {
		return nil
	}
	rank := c.rank(v.ID, c.selfJID())
	rec := c.record(v)

	// Someone else already posted for this note.
	if c.transcribed(rec) {
		return nil
	}

	if d := c.delayFor(rank, v.FromMe); d > 0 {
		if !c.wait(ctx, rec, d, rank) {
			return c.standDown(ctx, rec, v, rank)
		}
	}
	if c.transcribed(rec) {
		return nil
	}

	claim := c.ClaimMode && c.Signals != nil
	if claim {
		if !c.claimNow(ctx, rec, v, rank) {
			return c.standDown(ctx, rec, v, rank)
		}
		// Settle window: a peer with a lower rank may have claimed too.
		if !c.wait(ctx, rec, c.settle(), rank) {
			return c.standDown(ctx, rec, v, rank)
		}
	}
	return &Lease{c: c, rec: rec, rank: rank, claim: claim}
}

// ObserveClaim records a claim arriving from another instance.
func (c *Coordinator) ObserveClaim(chat types.JID, voiceID string, from types.JID, at time.Time) {
	if voiceID == "" || from.IsEmpty() {
		return
	}
	rec := c.record(Voice{Chat: chat, ID: voiceID})
	c.mu.Lock()
	if rec.peerClaims == nil {
		rec.peerClaims = map[string]uint64{}
	}
	rec.peerClaims[from.String()] = c.rankUnlocked(voiceID, from)
	rec.lastTouch = c.now()
	rec.notify()
	c.mu.Unlock()
}

// ObserveTranscription records a tagged transcription from another instance.
func (c *Coordinator) ObserveTranscription(chat types.JID, voiceID string, from types.JID) {
	if voiceID == "" || from.IsEmpty() {
		return
	}
	rec := c.record(Voice{Chat: chat, ID: voiceID})
	c.mu.Lock()
	rec.transcribed = true
	rec.lastTouch = c.now()
	rec.notify()
	c.mu.Unlock()
}

// -- internals --------------------------------------------------------------

func (c *Coordinator) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Coordinator) after(d time.Duration) <-chan time.Time {
	if c.After != nil {
		return c.After(d)
	}
	return time.After(d)
}

func (c *Coordinator) settle() time.Duration {
	if c.Settle > 0 {
		return c.Settle
	}
	return defaultSettle
}

func (c *Coordinator) delayFor(rank uint64, fromMe bool) time.Duration {
	if fromMe {
		return 0
	}
	base, spread := c.DelayBase, c.DelaySpread
	if base <= 0 {
		base = defaultDelayBase
	}
	if spread <= 0 {
		spread = defaultDelaySpread
	}
	return base + time.Duration(rank%uint64(spread/time.Millisecond))*time.Millisecond
}

func (c *Coordinator) watchdog(v Voice) time.Duration {
	base := c.WatchdogBase
	if base <= 0 {
		base = defaultWatchdogBase
	}
	return base + time.Duration(v.Seconds)*time.Second
}

// selfJID prefers the phone-number form: it is the same on every client, LIDs
// are per-account.
func (c *Coordinator) selfJID() types.JID {
	if c.Self == nil {
		return types.EmptyJID
	}
	var fallback types.JID
	for _, j := range c.Self() {
		if j.Server == types.DefaultUserServer {
			return j
		}
		if fallback.IsEmpty() {
			fallback = j
		}
	}
	return fallback
}

func (c *Coordinator) rank(voiceID string, j types.JID) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rankUnlocked(voiceID, j)
}

// rankUnlocked: lower wins. sha256(voiceID + "|" + jid.User), first 8 bytes.
func (c *Coordinator) rankUnlocked(voiceID string, j types.JID) uint64 {
	sum := sha256.Sum256([]byte(voiceID + "|" + j.User))
	return binary.BigEndian.Uint64(sum[:8])
}

// record returns the state for v, creating it and pruning old ones.
func (c *Coordinator) record(v Voice) *record {
	key := v.Chat.String() + "|" + v.ID
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == nil {
		c.state = map[string]*record{}
	}
	if rec, ok := c.state[key]; ok {
		rec.lastTouch = c.now()
		return rec
	}
	keep := c.Keep
	if keep <= 0 {
		keep = defaultKeep
	}
	now := c.now()
	for k, rec := range c.state {
		if now.Sub(rec.lastTouch) > keep {
			delete(c.state, k)
		}
	}
	rec := &record{key: key, ch: make(chan struct{}), lastTouch: now}
	c.state[key] = rec
	return rec
}

func (c *Coordinator) transcribed(rec *record) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return rec.transcribed
}

// standDown is reached when another instance won. It waits for the tagged
// transcription; if it never arrives within the watchdog window, this
// instance takes over (returns a Lease).
func (c *Coordinator) standDown(ctx context.Context, rec *record, v Voice, rank uint64) *Lease {
	c.mu.Lock()
	c.counters.StoodDown++
	c.mu.Unlock()

	timer := c.after(c.watchdog(v))
	for {
		c.mu.Lock()
		done := rec.transcribed
		wake := rec.ch
		c.mu.Unlock()
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-timer:
			c.mu.Lock()
			if rec.transcribed {
				c.mu.Unlock()
				return nil
			}
			c.counters.TookOver++
			c.mu.Unlock()
			c.log().Warn("coord: peer claimed but never posted, taking over",
				"chat", v.Chat, "msg", v.ID)
			claim := c.ClaimMode && c.Signals != nil
			if claim {
				if err := c.Signals.Claim(ctx, v.Chat, v.Sender, v.ID); err != nil {
					c.log().Warn("coord: claim on takeover failed", "err", err)
				}
			}
			return &Lease{c: c, rec: rec, rank: rank, claim: claim}
		case <-wake:
		}
	}
}

// claimNow sends our claim and waits for the settle window. It returns false
// when a peer with a lower rank claimed meanwhile.
func (c *Coordinator) claimNow(ctx context.Context, rec *record, v Voice, rank uint64) bool {
	if err := c.Signals.Claim(ctx, v.Chat, v.Sender, v.ID); err != nil {
		c.log().Warn("coord: claim failed, transcribing anyway", "err", err)
	}
	c.mu.Lock()
	rec.selfClaimed = true
	c.counters.Claims++
	rec.lastTouch = c.now()
	c.mu.Unlock()
	return !c.lowerPeer(rec, rank)
}

func (c *Coordinator) lowerPeer(rec *record, rank uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range rec.peerClaims {
		if r < rank {
			return true
		}
	}
	return false
}

// wait sleeps for d; false when stood down or cancelled meanwhile.
func (c *Coordinator) wait(ctx context.Context, rec *record, d time.Duration, rank uint64) bool {
	deadline := c.now().Add(d)
	for {
		c.mu.Lock()
		stoodDown := c.stoodDownLocked(rec, rank)
		wake := rec.ch
		now := c.now()
		c.mu.Unlock()
		if stoodDown {
			return false
		}
		if !now.Before(deadline) {
			return true
		}
		timer := c.after(deadline.Sub(now))
		select {
		case <-ctx.Done():
			return false
		case <-timer:
		case <-wake:
		}
	}
}

// stoodDownLocked: another instance is (or will be) doing this note.
func (c *Coordinator) stoodDownLocked(rec *record, rank uint64) bool {
	if rec.transcribed {
		return true
	}
	if len(rec.peerClaims) == 0 {
		return false
	}
	if !c.ClaimMode {
		return true // observe-only: any claim wins
	}
	for _, r := range rec.peerClaims {
		if r < rank {
			return true
		}
	}
	return false
}

func (c *Coordinator) stillMine(rec *record, rank uint64, claim bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if rec.transcribed {
		return false
	}
	if !claim {
		return true
	}
	for _, r := range rec.peerClaims {
		if r < rank {
			return false
		}
	}
	return true
}

func (c *Coordinator) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

// Counters implements bot.Coord's Counters (mode included).
func (c *Coordinator) Counters() (claims, stoodDown, tookOver uint64, mode string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	mode = "claim"
	if !c.ClaimMode {
		mode = "observe-only"
	}
	return c.counters.Claims, c.counters.StoodDown, c.counters.TookOver, mode
}
