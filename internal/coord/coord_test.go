package coord

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

var (
	chat  = types.NewJID("120363000000000000", types.GroupServer)
	voice = types.NewJID("393331111111", types.DefaultUserServer)
)

// clock is a fake clock: After registers a timer, fire() advances the fake
// "now" to the latest deadline and releases every armed timer. In auto mode
// timers fire (and time advances) immediately.
type clock struct {
	mu     sync.Mutex
	now    time.Time
	auto   bool
	timers []timer
}

type timer struct {
	ch chan time.Time
	at time.Time
}

func newClock(auto bool) *clock {
	return &clock{auto: auto, now: time.Unix(1700000000, 0)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	if c.auto {
		c.now = c.now.Add(d)
		now := c.now
		c.mu.Unlock()
		ch <- now
		return ch
	}
	c.timers = append(c.timers, timer{ch: ch, at: c.now.Add(d)})
	c.mu.Unlock()
	return ch
}

// fire advances the clock to the latest armed deadline and releases them all.
func (c *clock) fire() {
	c.mu.Lock()
	timers := c.timers
	c.timers = nil
	var max time.Time
	for _, t := range timers {
		if t.at.After(max) {
			max = t.at
		}
	}
	if max.After(c.now) {
		c.now = max
	}
	now := c.now
	c.mu.Unlock()
	for _, t := range timers {
		select {
		case t.ch <- now:
		default:
		}
	}
}

func (c *clock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

type fakeSignals struct {
	mu     sync.Mutex
	claims []string
	// onClaim is called synchronously after a claim (simulates the peer bus).
	onClaim func(chat types.JID, voiceID string)
}

func (f *fakeSignals) Claim(_ context.Context, _ chat2, _ voice2, voiceID string) error {
	f.mu.Lock()
	f.claims = append(f.claims, voiceID)
	fn := f.onClaim
	f.mu.Unlock()
	if fn != nil {
		fn(chat, voiceID)
	}
	return nil
}

func (f *fakeSignals) claimed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.claims...)
}

// Aliases keep the fake's signature readable without importing types twice.
type chat2 = types.JID
type voice2 = types.JID

func newCoord(self types.JID, sig Signals, clk *clock, claimMode bool) *Coordinator {
	return &Coordinator{
		Self:      func() []types.JID { return []types.JID{self} },
		Signals:   sig,
		Now:       clk.Now,
		After:     clk.After,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		ClaimMode: claimMode,
	}
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestAloneInChatProceeds(t *testing.T) {
	clk := newClock(true)
	sig := &fakeSignals{}
	c := newCoord(voice, sig, clk, true)
	lease := c.Acquire(ctxT(t), Voice{Chat: chat, ID: "V1", Sender: voice, Seconds: 3})
	if lease == nil || !lease.StillMine() {
		t.Fatal("a lone instance must transcribe")
	}
	if got := sig.claimed(); len(got) != 1 || got[0] != "V1" {
		t.Fatalf("claims = %v", got)
	}
}

func TestOwnVoiceNoteSkipsDelay(t *testing.T) {
	clk := newClock(false) // timers never fire
	c := newCoord(voice, nil, clk, false)
	// FromMe must not wait: with a manual clock that would block.
	done := make(chan *Lease, 1)
	go func() { done <- c.Acquire(ctxT(t), Voice{Chat: chat, ID: "V2", Sender: voice, FromMe: true}) }()
	select {
	case lease := <-done:
		if lease == nil {
			t.Fatal("own voice note must be transcribed")
		}
	case <-time.After(time.Second):
		t.Fatal("Acquire blocked on our own voice note (delay not skipped)")
	}
}

func waitPending(t *testing.T, clk *clock, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for clk.pending() < n {
		if time.Now().After(deadline) {
			t.Fatalf("timers armed: %d, want %d", clk.pending(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// lowerPeerFor returns a JID that wins the rank against self for voiceID.
func lowerPeerFor(t *testing.T, voiceID string, self types.JID) types.JID {
	t.Helper()
	selfRank := rankOf(voiceID, self)
	for i := 1; i < 20000; i++ {
		j := types.NewJID("3933"+itoa(100000+i), types.DefaultUserServer)
		if rankOf(voiceID, j) < selfRank {
			return j
		}
	}
	t.Fatal("no lower-rank peer found")
	return types.EmptyJID
}

// higherPeerFor returns a JID that loses the rank against self for voiceID.
func higherPeerFor(t *testing.T, voiceID string, self types.JID) types.JID {
	t.Helper()
	selfRank := rankOf(voiceID, self)
	for i := 1; i < 20000; i++ {
		j := types.NewJID("3933"+itoa(100000+i), types.DefaultUserServer)
		if rankOf(voiceID, j) > selfRank {
			return j
		}
	}
	t.Fatal("no higher-rank peer found")
	return types.EmptyJID
}

func TestPeerClaimDuringDelayStandsDown(t *testing.T) {
	clk := newClock(false)
	sig := &fakeSignals{}
	c := newCoord(voice, sig, clk, true)
	peer := lowerPeerFor(t, "V3", voice)

	done := make(chan *Lease, 1)
	go func() { done <- c.Acquire(ctxT(t), Voice{Chat: chat, ID: "V3", Sender: voice}) }()

	waitPending(t, clk, 1) // our delay
	c.ObserveClaim(chat, "V3", peer, time.Now())
	clk.fire() // delay over: stood down, watchdog armed
	waitPending(t, clk, 1)
	clk.fire() // watchdog: the peer never posted

	select {
	case lease := <-done:
		if lease == nil {
			t.Fatal("watchdog must take over when the peer never posts")
		}
		_, stoodDown, tookOver, mode := c.Counters()
		if stoodDown != 1 || tookOver != 1 || mode != "claim" {
			t.Fatalf("counters: stood=%d took=%d mode=%s", stoodDown, tookOver, mode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire did not return")
	}
	if got := sig.claimed(); len(got) == 0 {
		t.Fatal("takeover did not claim")
	}
}

func TestTaggedTranscriptionSkipsBeforeTranscribing(t *testing.T) {
	clk := newClock(true)
	sig := &fakeSignals{}
	c := newCoord(voice, sig, clk, true)
	peer := types.NewJID("393330000000", types.DefaultUserServer)
	c.ObserveTranscription(chat, "V4", peer)
	if lease := c.Acquire(ctxT(t), Voice{Chat: chat, ID: "V4", Sender: voice}); lease != nil {
		t.Fatal("must not transcribe when a tagged transcription exists")
	}
	if got := sig.claimed(); len(got) != 0 {
		t.Fatalf("claimed anyway: %v", got)
	}
}

func TestDelayIsZeroForOwnVoice(t *testing.T) {
	clk := newClock(false)
	c := newCoord(voice, nil, clk, false)
	if d := c.delayFor(12345, true); d != 0 {
		t.Fatalf("own voice note delay = %v, want 0", d)
	}
	if d := c.delayFor(0, false); d != defaultDelayBase {
		t.Fatalf("peer voice note delay = %v", d)
	}
}

func TestCrossingClaimsLowestRankWins(t *testing.T) {
	clk := newClock(false)
	sig := &fakeSignals{}
	c := newCoord(voice, sig, clk, true)
	winner := lowerPeerFor(t, "V5", voice)
	c.ObserveClaim(chat, "V5", winner, time.Now())

	done := make(chan *Lease, 1)
	go func() { done <- c.Acquire(ctxT(t), Voice{Chat: chat, ID: "V5", Sender: voice}) }()

	waitPending(t, clk, 1) // the watchdog: we stood down before claiming
	c.ObserveTranscription(chat, "V5", winner)
	clk.fire()

	select {
	case lease := <-done:
		if lease != nil {
			t.Fatal("the lower-rank peer must win crossing claims")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire did not return")
	}
	if got := sig.claimed(); len(got) != 0 {
		t.Fatalf("the loser must not claim: %v", got)
	}
}

func TestHigherRankPeerDoesNotBlockUs(t *testing.T) {
	clk := newClock(true)
	sig := &fakeSignals{}
	c := newCoord(voice, sig, clk, true)
	c.ObserveClaim(chat, "V6", higherPeerFor(t, "V6", voice), time.Now())
	lease := c.Acquire(ctxT(t), Voice{Chat: chat, ID: "V6", Sender: voice, FromMe: true})
	if lease == nil {
		t.Fatal("a higher-rank claim must not block us")
	}
	if !lease.StillMine() {
		t.Fatal("the lease must stay ours against a higher-rank peer")
	}
}

func TestStillMineFalseAfterPeerClaimWhileTranscribing(t *testing.T) {
	clk := newClock(true)
	sig := &fakeSignals{}
	c := newCoord(voice, sig, clk, true)
	lease := c.Acquire(ctxT(t), Voice{Chat: chat, ID: "V7", Sender: voice, FromMe: true})
	if lease == nil {
		t.Fatal("expected a lease")
	}
	if !lease.StillMine() {
		t.Fatal("lease should still be ours")
	}
	// A lower-rank peer claims while we are transcribing.
	lower := types.NewJID("1", types.DefaultUserServer)
	for rankOf("V7", lower) >= rankOf("V7", voice) {
		lower = types.NewJID(lower.User+"9", types.DefaultUserServer)
	}
	c.ObserveClaim(chat, "V7", lower, time.Now())
	if lease.StillMine() {
		t.Fatal("a lower-rank claim must invalidate the lease")
	}
	c.ObserveTranscription(chat, "V7", lower)
	if lease.StillMine() {
		t.Fatal("a tagged transcription must invalidate the lease")
	}
}

func TestObserveOnlyNeverClaimsButSkips(t *testing.T) {
	clk := newClock(true)
	sig := &fakeSignals{}
	c := newCoord(voice, sig, clk, false) // observe-only

	lease := c.Acquire(ctxT(t), Voice{Chat: chat, ID: "V8", Sender: voice, FromMe: true})
	if lease == nil {
		t.Fatal("observe-only must still transcribe untagged notes")
	}
	if got := sig.claimed(); len(got) != 0 {
		t.Fatalf("claims in observe-only mode: %v", got)
	}

	peer := types.NewJID("393330000000", types.DefaultUserServer)
	c.ObserveTranscription(chat, "V9", peer)
	if lease := c.Acquire(ctxT(t), Voice{Chat: chat, ID: "V9", Sender: voice, FromMe: true}); lease != nil {
		t.Fatal("observe-only must skip tagged transcriptions")
	}
}

func TestObserveOnlyStandsDownOnPeerClaim(t *testing.T) {
	clk := newClock(false)
	sig := &fakeSignals{}
	c := newCoord(voice, sig, clk, false)
	peer := types.NewJID("393330000000", types.DefaultUserServer)

	done := make(chan *Lease, 1)
	go func() { done <- c.Acquire(ctxT(t), Voice{Chat: chat, ID: "V10", Sender: voice}) }()
	deadline := time.Now().Add(time.Second)
	for clk.pending() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	c.ObserveClaim(chat, "V10", peer, time.Now())
	c.ObserveTranscription(chat, "V10", peer)
	clk.fire()

	select {
	case lease := <-done:
		if lease != nil {
			t.Fatal("must stand down when the peer claims and posts")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire did not return")
	}
}

func TestContextCancellationReturns(t *testing.T) {
	clk := newClock(false)
	c := newCoord(voice, &fakeSignals{}, clk, true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *Lease, 1)
	go func() { done <- c.Acquire(ctx, Voice{Chat: chat, ID: "V11", Sender: voice}) }()
	time.Sleep(5 * time.Millisecond)
	cancel()
	select {
	case lease := <-done:
		if lease != nil {
			t.Fatal("cancelled Acquire must not return a lease")
		}
	case <-time.After(time.Second):
		t.Fatal("Acquire ignored cancellation")
	}
}

func rankOf(voiceID string, j types.JID) uint64 {
	c := &Coordinator{}
	return c.rankUnlocked(voiceID, j)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
