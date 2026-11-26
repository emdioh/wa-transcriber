package sntp

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestNTPEpochAndFractionConversion(t *testing.T) {
	// Unix epoch (1970-01-01 00:00:00 UTC) must encode to 2208988800 (0x83AA7E80).
	unixEpoch := time.Unix(0, 0).UTC()
	gotEpoch := timeToNTP(unixEpoch)
	const wantEpoch uint64 = 0x83AA7E80 << 32
	if gotEpoch != wantEpoch {
		t.Fatalf("timeToNTP(1970) = %#016x, want %#016x", gotEpoch, wantEpoch)
	}
	if back := ntpToTime(gotEpoch); !back.Equal(unixEpoch) {
		t.Fatalf("ntpToTime(%#016x) = %v, want %v", gotEpoch, back, unixEpoch)
	}

	// Exact fractional values: 0.5 s = 0x80000000, 0.25 s = 0x40000000.
	half := time.Date(2026, 11, 26, 12, 34, 56, 500_000_000, time.UTC)
	if frac := uint32(timeToNTP(half)); frac != 0x80000000 {
		t.Fatalf("half-second frac = %#08x, want 0x80000000", frac)
	}
	quarter := time.Date(2026, 11, 26, 12, 34, 56, 250_000_000, time.UTC)
	if frac := uint32(timeToNTP(quarter)); frac != 0x40000000 {
		t.Fatalf("quarter-second frac = %#08x, want 0x40000000", frac)
	}

	// Round-trip accuracy across Era 0 (2026) and Era 1 (2040) with arbitrary nanoseconds.
	cases := []time.Time{
		time.Date(2026, 11, 26, 22, 17, 3, 123_456_789, time.UTC),
		time.Date(2026, 11, 26, 22, 17, 3, 999_999_999, time.UTC),
		time.Date(2036, 2, 7, 6, 28, 15, 0, time.UTC),
		time.Date(2040, 6, 15, 8, 30, 0, 654_321_000, time.UTC),
	}
	for _, want := range cases {
		got := ntpToTime(timeToNTP(want))
		diff := got.Sub(want)
		if diff < -time.Nanosecond || diff > time.Nanosecond {
			t.Errorf("round-trip %v -> %v (diff %v)", want, got, diff)
		}
	}
}

func TestBuildRequest(t *testing.T) {
	const tx uint64 = 0xEE801234_80000000
	pkt := buildRequest(tx)
	if len(pkt) != packetLen {
		t.Fatalf("len = %d, want %d", len(pkt), packetLen)
	}
	if pkt[0] != 0x23 {
		t.Fatalf("header byte = %#02x, want 0x23 (LI=0, VN=4, Mode=3)", pkt[0])
	}
	if got := binary.BigEndian.Uint64(pkt[40:48]); got != tx {
		t.Fatalf("transmit timestamp = %#016x, want %#016x", got, tx)
	}
}

func TestNormalizeAddr(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"pool.ntp.org", "pool.ntp.org:123"},
		{"pool.ntp.org:123", "pool.ntp.org:123"},
		{"127.0.0.1", "127.0.0.1:123"},
		{"127.0.0.1:12345", "127.0.0.1:12345"},
		{"::1", "[::1]:123"},
		{"[::1]", "[::1]:123"},
		{"[::1]:1234", "[::1]:1234"},
	}
	for _, tc := range cases {
		if got := normalizeAddr(tc.in); got != tc.want {
			t.Errorf("normalizeAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// makeValidReply builds a valid 48-byte SNTP server response for unit tests.
func makeValidReply(reqTx uint64, rx, tx time.Time) []byte {
	pkt := make([]byte, packetLen)
	pkt[0] = (0 << 6) | (4 << 3) | 4 // LI=0, VN=4, Mode=4 (server)
	pkt[1] = 2                       // stratum 2
	binary.BigEndian.PutUint64(pkt[24:32], reqTx)
	binary.BigEndian.PutUint64(pkt[32:40], timeToNTP(rx))
	binary.BigEndian.PutUint64(pkt[40:48], timeToNTP(tx))
	return pkt
}

func TestParseResponseValidAndOffset(t *testing.T) {
	// Simulate a board with no RTC (local clock at 1970-01-01) talking to a
	// server in 2026 with 40 ms symmetric RTT and 2 ms server processing time.
	t1 := time.Unix(0, 0).UTC()
	t4 := t1.Add(42 * time.Millisecond)
	reqTx := timeToNTP(t1)

	realStart := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	t2 := realStart.Add(20 * time.Millisecond)
	t3 := realStart.Add(22 * time.Millisecond)
	wantTime := realStart.Add(42 * time.Millisecond)

	pkt := makeValidReply(reqTx, t2, t3)
	resp, err := parseResponse(pkt, reqTx, t1, t4)
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if diff := resp.Time.Sub(wantTime); diff < -time.Microsecond || diff > time.Microsecond {
		t.Errorf("Time = %v, want %v (diff %v)", resp.Time, wantTime, diff)
	}
	if resp.RTT < 39*time.Millisecond || resp.RTT > 41*time.Millisecond {
		t.Errorf("RTT = %v, want ~40ms", resp.RTT)
	}
	if resp.Stratum != 2 {
		t.Errorf("Stratum = %d, want 2", resp.Stratum)
	}

	// Mode 5 (broadcast) and packets with trailing extension bytes are also accepted.
	pktExt := append(makeValidReply(reqTx, t2, t3), make([]byte, 16)...)
	pktExt[0] = (0 << 6) | (4 << 3) | 5
	if _, err := parseResponse(pktExt, reqTx, t1, t4); err != nil {
		t.Errorf("mode 5 with extensions rejected: %v", err)
	}
}

func TestParseResponseRejectsInvalidPackets(t *testing.T) {
	t1 := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	t4 := t1.Add(10 * time.Millisecond)
	reqTx := timeToNTP(t1)
	rx := t1.Add(4 * time.Millisecond)
	tx := t1.Add(6 * time.Millisecond)

	cases := []struct {
		name    string
		mutate  func([]byte) []byte
		wantErr string
	}{
		{
			name:    "short packet",
			mutate:  func(b []byte) []byte { return b[:47] },
			wantErr: "short packet",
		},
		{
			name: "LI=3 unsynchronised",
			mutate: func(b []byte) []byte {
				b[0] = (3 << 6) | (4 << 3) | 4
				return b
			},
			wantErr: "LI=3",
		},
		{
			name: "wrong mode",
			mutate: func(b []byte) []byte {
				b[0] = (0 << 6) | (4 << 3) | 3 // client mode
				return b
			},
			wantErr: "mode",
		},
		{
			name: "stratum 0 kiss-o-death with code",
			mutate: func(b []byte) []byte {
				b[1] = 0
				copy(b[12:16], "RATE")
				return b
			},
			wantErr: "RATE",
		},
		{
			name: "stratum 0 without code",
			mutate: func(b []byte) []byte {
				b[1] = 0
				return b
			},
			wantErr: "stratum 0",
		},
		{
			name: "stratum 16 unsynchronised",
			mutate: func(b []byte) []byte {
				b[1] = 16
				return b
			},
			wantErr: "stratum 16",
		},
		{
			name: "originate timestamp mismatch",
			mutate: func(b []byte) []byte {
				binary.BigEndian.PutUint64(b[24:32], reqTx^1)
				return b
			},
			wantErr: "originate timestamp mismatch",
		},
		{
			name: "zero transmit timestamp",
			mutate: func(b []byte) []byte {
				binary.BigEndian.PutUint64(b[40:48], 0)
				return b
			},
			wantErr: "zero transmit timestamp",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pkt := tc.mutate(makeValidReply(reqTx, rx, tx))
			_, err := parseResponse(pkt, reqTx, t1, t4)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// startFakeNTPServer listens on 127.0.0.1:0 and passes each received packet to
// handler. If handler returns a non-empty slice it is sent back to the client.
func startFakeNTPServer(t *testing.T, handler func(req []byte) []byte) string {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var buf [256]byte
		for {
			n, addr, err := pc.ReadFrom(buf[:])
			if err != nil {
				return
			}
			if reply := handler(buf[:n]); len(reply) > 0 {
				_, _ = pc.WriteTo(reply, addr)
			}
		}
	}()
	t.Cleanup(func() {
		_ = pc.Close()
		wg.Wait()
	})
	return pc.LocalAddr().String()
}

func TestRunRetryFallbackAccuracyPeriodicAndCancel(t *testing.T) {
	wantTime := time.Date(2026, 11, 26, 22, 30, 0, 250_000_000, time.UTC)

	// Server 1 always returns Kiss-o'-Death (stratum 0) so Run must fall back
	// to Server 2 on every round.
	var srv1Hits atomic.Int32
	srv1 := startFakeNTPServer(t, func(req []byte) []byte {
		srv1Hits.Add(1)
		if len(req) < packetLen {
			return nil
		}
		orig := binary.BigEndian.Uint64(req[40:48])
		reply := makeValidReply(orig, wantTime, wantTime)
		reply[1] = 0
		copy(reply[12:16], "DENY")
		return reply
	})

	// Server 2 fails the first 2 attempts (returning LI=3 on #1 and dropping #2
	// to exercise timeout + backoff retry), then serves wantTime + 1s per call.
	var srv2Hits atomic.Int32
	srv2 := startFakeNTPServer(t, func(req []byte) []byte {
		hit := srv2Hits.Add(1)
		if len(req) < packetLen {
			return nil
		}
		if req[0] != clientHeader {
			return nil
		}
		orig := binary.BigEndian.Uint64(req[40:48])
		switch hit {
		case 1:
			reply := makeValidReply(orig, wantTime, wantTime)
			reply[0] = (3 << 6) | (4 << 3) | 4 // LI=3 unsynchronised
			return reply
		case 2:
			return nil // drop packet to force timeout
		default:
			served := wantTime.Add(time.Duration(hit-3) * time.Second)
			return makeValidReply(orig, served, served.Add(time.Millisecond))
		}
	})

	var mu sync.Mutex
	var setCalls []time.Time
	var failFirstSet atomic.Bool
	failFirstSet.Store(true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, Options{
			Servers:  []string{srv1, srv2},
			Timeout:  30 * time.Millisecond,
			Interval: 20 * time.Millisecond,
			Backoff:  []time.Duration{5 * time.Millisecond, 10 * time.Millisecond},
			Log:      testLog(),
			SetClock: func(got time.Time) error {
				// Fail the very first SetClock call to verify Run retries it.
				if failFirstSet.Swap(false) {
					return errors.New("transient clock error")
				}
				mu.Lock()
				setCalls = append(setCalls, got)
				count := len(setCalls)
				mu.Unlock()
				if count >= 3 {
					cancel()
				}
				return nil
			},
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("Run did not complete 3 periodic syncs in time")
	}

	if srv1Hits.Load() < 3 {
		t.Errorf("srv1 hits = %d, want >= 3 (fallback tried server 1 first)", srv1Hits.Load())
	}
	if srv2Hits.Load() < 5 {
		t.Errorf("srv2 hits = %d, want >= 5 (2 failures + 1 set error + 3 successes)", srv2Hits.Load())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(setCalls) < 3 {
		t.Fatalf("setCalls = %d, want >= 3", len(setCalls))
	}
	// Hit #3 failed SetClock, so setCalls[0] came from hit #4 (wantTime + 1s).
	for i, got := range setCalls[:3] {
		expected := wantTime.Add(time.Duration(i+1) * time.Second)
		diff := got.Sub(expected)
		if diff < -50*time.Millisecond || diff > 50*time.Millisecond {
			t.Errorf("setCalls[%d] = %v, want ~%v (diff %v)", i, got, expected, diff)
		}
	}
}

func TestRunCancelsPromptlyDuringBlockedQuery(t *testing.T) {
	// Blackhole UDP server that never replies, with a long per-query timeout.
	blackhole := startFakeNTPServer(t, func([]byte) []byte { return nil })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, Options{
			Servers:  []string{blackhole},
			Timeout:  10 * time.Second,
			Interval: time.Hour,
			Log:      testLog(),
			SetClock: func(time.Time) error { return nil },
		})
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not exit promptly after context cancellation")
	}
}
