package main

import (
	"errors"
	"io"
	"sync"
	"time"
)

// port is the open console: the single descriptor and the reader goroutine
// that turns its bytes into events. All waiting is here and nowhere else.
//
// There are exactly two clocks and neither decides that an answer is
// complete:
//
//   - idle: the console has said nothing at all for this long -- a hung
//     board, a command that stopped the shell, a port that went away. It is a
//     safety net.
//   - prompt: the board never produced a shell prompt. After the command has
//     been sent this deadline is disabled, because a command is allowed to
//     run for as long as it likes as long as the board keeps talking.
//
// Both are expressed as a deadline and one timer that is re-armed for the
// next deadline; a session that ends with the END tag never waits for either.
type port struct {
	rw     io.ReadWriter
	events chan event

	idle, prompt time.Duration

	mu          sync.Mutex
	stopped     bool
	done        chan struct{} // closed when readLoop returns
	lastHeard   time.Time
	promptUntil time.Time
	promptLive  bool
}

// newPort wraps a descriptor. Nothing is read until start.
func newPort(rw io.ReadWriter, idle, prompt time.Duration) *port {
	return &port{rw: rw, events: make(chan event, 256), idle: idle, prompt: prompt, done: make(chan struct{})}
}

// start launches the reader.
func (p *port) start() {
	p.mu.Lock()
	p.lastHeard = time.Now()
	p.promptUntil = p.lastHeard.Add(p.prompt)
	p.promptLive = true
	p.mu.Unlock()
	go p.readLoop()
}

// stop asks the reader goroutine to end. It is safe to call more than once.
func (p *port) stop() {
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
}

// wait stops the reader and waits for it to return, so the caller can close
// the descriptor without racing a poll that is still in flight. It is meant
// for the *test* harness and for a future clean shutdown; the command path
// never calls it, because waiting for a poll to time out is precisely the
// fixed delay this tool removes. The wait is bounded: a Read stuck in the
// kernel (a USB unplug that never returns) must not hang forever.
func (p *port) wait(limit time.Duration) {
	p.stop()
	select {
	case <-p.done:
	case <-time.After(limit):
	}
}

// phaseStart is called when the session moves from the login dialogue to the
// command. It throws away events that were still queued from the previous
// phase -- the prompt line the login stopped at, a silence that was reported
// while the answer was being written -- and restarts the silence clock, so a
// command is never cut off by a gap that ended before it was sent.
func (p *port) phaseStart(now time.Time) {
	p.mu.Lock()
	p.promptLive = false
	p.lastHeard = now
	p.mu.Unlock()
	// Draining without blocking: the channel is only written by the reader,
	// and it may refill concurrently, which is fine.
	for {
		select {
		case <-p.events:
			continue
		default:
		}
		break
	}
}

func (p *port) alive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.stopped
}

// deliver hands an event to the session unless the session is done or the
// queue is full (a session that stopped listening must not hang the reader).
func (p *port) deliver(ev event) bool {
	if !p.alive() {
		return false
	}
	select {
	case p.events <- ev:
		return true
	default:
		return false
	}
}

// heard records that bytes arrived, which pushes the silence deadline out.
func (p *port) heard(now time.Time) {
	p.mu.Lock()
	p.lastHeard = now
	p.mu.Unlock()
}

// deadlineLocked picks whichever deadline comes first: silence or a missing
// prompt.
func (p *port) deadlineLocked(now time.Time) (time.Duration, eventKind) {
	silent := now.Add(p.idle)
	kind := evIdleTimeout
	if p.promptLive && p.promptUntil.Before(silent) {
		kind = evPromptTimeout
		return until(p.promptUntil, now), kind
	}
	return p.idle, kind
}

func until(t, now time.Time) time.Duration {
	if d := t.Sub(now); d > 0 {
		return d
	}
	return time.Millisecond
}

// silenceIfDue reports which deadline, if any, has passed. A poll slice that
// simply ran out is not one: the board is allowed to say nothing for a
// moment. Reporting closes the prompt deadline; the idle deadline starts
// counting again from now, so a session that somehow ignores it still wakes
// up eventually instead of spinning.
func (p *port) silenceIfDue(now time.Time) (eventKind, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.promptLive && !now.Before(p.promptUntil) {
		p.promptLive = false
		return evPromptTimeout, true
	}
	if !now.Before(p.lastHeard.Add(p.idle)) {
		p.lastHeard = now
		return evIdleTimeout, true
	}
	return 0, false
}

const readChunk = 4096

// pollSlice bounds how long one poll may block, so the reader can notice a
// stop promptly. It does not affect when a run ends: the END tag ends it.
const pollSlice = 50 * time.Millisecond

// readLoop is the only reader of the port. It waits for bytes or for a
// deadline with poll(2), reassembles lines, hands each one to the session as
// it arrives and re-arms the timer each time.
func (p *port) readLoop() {
	defer close(p.done)
	var l lines
	var lastPartial string
	buf := make([]byte, readChunk)
	for {
		if !p.alive() {
			return
		}
		// Poll in short slices: a stop must be noticed quickly, and the
		// deadline math above still decides when the silence timers fire.
		wait := until(p.armedDeadline(), time.Now())
		if wait > pollSlice {
			wait = pollSlice
		}
		readable, _ := waitReadableAny(p.rw, wait)
		if !p.alive() {
			return
		}
		if !readable {
			// A slice ending says nothing about the board: only a deadline
			// that has actually passed does.
			if kind, due := p.silenceIfDue(time.Now()); due {
				if !p.deliver(event{kind: kind}) {
					return
				}
			}
			continue
		}
		n, err := p.rw.Read(buf)
		if n > 0 {
			p.heard(time.Now())
			for _, line := range l.feed(buf[:n]) {
				if !p.deliver(event{kind: evLine, line: line}) {
					return
				}
			}
			// A partial is only news when it differs from the line just
			// delivered; re-sending an unchanged remainder would make the
			// session print the same text twice.
			if string(l.buf) != lastPartial {
				lastPartial = string(l.buf)
				p.deliver(event{kind: evPartial, line: lastPartial})
			}
		}
		if err != nil {
			if rest, ok := l.flush(); ok {
				p.deliver(event{kind: evLine, line: rest})
			}
			if errors.Is(err, io.EOF) {
				p.deliver(event{kind: evClosed})
			} else {
				p.deliver(event{kind: evReadErr, err: err})
			}
			return
		}
	}
}

// armedDeadline is how long the next poll may wait: whichever deadline is
// closer, but at least a millisecond so the loop can re-check p.alive().
func (p *port) armedDeadline() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	d, _ := p.deadlineLocked(time.Now())
	return time.Now().Add(d)
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
