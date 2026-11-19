package coord

import (
	"context"
	"io"
	"log/slog"
	"math/rand"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

// bus is a fake transport: every claim is delivered to all other instances
// and every posted transcription is delivered the same way. Latency is
// simulated by delaying delivery, which is what can make two instances both
// believe they won.
type bus struct {
	mu      sync.Mutex
	nodes   map[types.JID]*node
	rng     *rand.Rand
	maxLat  time.Duration
	posted  []types.JID // which instance posted
	postedM map[string]bool
}

type node struct {
	jid types.JID
	c   *Coordinator
	bus *bus
}

func newBus(seed int64, maxLat time.Duration) *bus {
	return &bus{
		nodes:   map[types.JID]*node{},
		rng:     rand.New(rand.NewSource(seed)),
		maxLat:  maxLat,
		postedM: map[string]bool{},
	}
}

func (b *bus) latency() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.maxLat <= 0 {
		return 0
	}
	return time.Duration(b.rng.Int63n(int64(b.maxLat)))
}

// Claim implements Signals for one node.
func (n *node) Claim(_ context.Context, chat, sender types.JID, voiceID string) error {
	for _, other := range n.bus.others(n.jid) {
		d := n.bus.latency()
		go func(o *node) {
			if d > 0 {
				time.Sleep(d)
			}
			o.c.ObserveClaim(chat, voiceID, n.jid, time.Now())
		}(other)
	}
	return nil
}

// post simulates posting the tagged transcription.
func (n *node) post(chat types.JID, voiceID string) {
	n.bus.mu.Lock()
	if n.bus.postedM[voiceID] {
		n.bus.mu.Unlock()
		return
	}
	n.bus.postedM[voiceID] = true
	n.bus.posted = append(n.bus.posted, n.jid)
	n.bus.mu.Unlock()
	for _, other := range n.bus.others(n.jid) {
		d := n.bus.latency()
		go func(o *node) {
			if d > 0 {
				time.Sleep(d)
			}
			o.c.ObserveTranscription(chat, voiceID, n.jid)
		}(other)
	}
}

func (b *bus) others(self types.JID) []*node {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []*node
	for jid, n := range b.nodes {
		if jid != self {
			out = append(out, n)
		}
	}
	return out
}

// run starts n instances (the first one is the voice-note sender), lets them
// coordinate one voice note and returns how many posted.
func (b *bus) run(t *testing.T, n int) int {
	t.Helper()
	b.mu.Lock()
	b.posted = nil
	b.postedM = map[string]bool{}
	b.mu.Unlock()

	voiceID := "VOICE-" + itoa(int(b.rng.Int63()%1000000))
	chat := types.NewJID("120363000000000001", types.GroupServer)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		jid := types.NewJID("3933300000"+itoa(i), types.DefaultUserServer)
		nod := &node{jid: jid, bus: b}
		nod.c = &Coordinator{
			Self:      func() []types.JID { return []types.JID{jid} },
			Signals:   nod,
			Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
			ClaimMode: true,
			// Very short timings so the test stays fast; the ratios match the
			// production constants.
			DelayBase:    2 * time.Millisecond,
			DelaySpread:  8 * time.Millisecond,
			Settle:       5 * time.Millisecond,
			WatchdogBase: 40 * time.Millisecond,
		}
		b.mu.Lock()
		b.nodes[jid] = nod
		b.mu.Unlock()

		wg.Add(1)
		go func(nod *node, sender bool) {
			defer wg.Done()
			lease := nod.c.Acquire(ctx, Voice{Chat: chat, ID: voiceID, Sender: types.NewJID("3933300000"+itoa(i), types.DefaultUserServer), FromMe: sender, Seconds: 0})
			if lease == nil {
				return
			}
			if !lease.StillMine() {
				return
			}
			nod.post(chat, voiceID)
		}(nod, i == 0)
	}
	wg.Wait()
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.posted)
}

func TestSimulationExactlyOnePosts(t *testing.T) {
	if testing.Short() {
		t.Skip("simulation")
	}
	for n := 2; n <= 5; n++ {
		zero, multi := 0, 0
		const runs = 40
		for seed := int64(0); seed < runs; seed++ {
			b := newBus(seed, 15*time.Millisecond)
			posted := b.run(t, n)
			switch {
			case posted == 0:
				zero++
			case posted > 1:
				multi++
			}
		}
		// With watchdogs a note is never left untranscribed, and the rank +
		// settle window keeps duplicates rare.
		if zero > 0 {
			t.Errorf("n=%d: %d/%d runs posted nothing", n, zero, runs)
		}
		if limit := runs / 10; multi > limit {
			t.Errorf("n=%d: %d/%d runs posted twice (limit %d)", n, multi, runs, limit)
		}
		t.Logf("n=%d: zero=%d duplicates=%d of %d runs", n, zero, multi, runs)
	}
}

func TestSimulationNoNetworkPostsSingle(t *testing.T) {
	// Latency 0: every claim is delivered before the settle window ends, so
	// exactly one instance must post.
	for n := 2; n <= 5; n++ {
		b := newBus(42, 0)
		if posted := b.run(t, n); posted != 1 {
			t.Errorf("n=%d: posted %d times, want 1", n, posted)
		}
	}
}
