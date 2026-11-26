// Package sntp implements an RFC 4330 SNTPv4 client over UDP.
//
// Boards that boot with this binary as init have no /etc/init.d/S40ntpd and no
// battery-backed RTC, so they start at the Unix epoch until the network comes
// up. Run queries SNTP servers, steps the system clock, clears the kernel
// STA_UNSYNC flag so netmgr.NTPSynced reports synchronised, and re-syncs
// periodically.
package sntp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"
)

const (
	packetLen = 48

	// ntpEpochOffset is the number of seconds between the NTP epoch
	// (1900-01-01 00:00:00 UTC) and the Unix epoch (1970-01-01 00:00:00 UTC).
	ntpEpochOffset = 2208988800

	// ntpEraPivot splits 32-bit NTP seconds between Era 0 (MSB=1, 1968..2036)
	// and Era 1 (MSB=0, 2036..2104), per RFC 4330 Section 3.
	ntpEraPivot = 0x80000000

	// clientHeader is LI=0, VN=4, Mode=3 (client): (0<<6) | (4<<3) | 3.
	clientHeader = 0x23

	// DefaultTimeout is the per-server UDP query timeout.
	DefaultTimeout = 5 * time.Second

	// DefaultInterval is how often the clock is re-synchronised after a
	// successful sync. Must remain well below the Linux kernel's ~8.8 hour
	// NTP_PHASE_LIMIT drift window so STA_UNSYNC stays clear.
	DefaultInterval = 4 * time.Hour
)

// DefaultServers matches the two servers /etc/init.d/S40ntpd used, so one DNS
// or upstream outage does not leave a board without time.
var DefaultServers = []string{
	"pool.ntp.org:123",
	"time.cloudflare.com:123",
}

// DefaultBackoff is the retry schedule used when every configured server fails
// in a round. The final entry repeats until a sync succeeds.
var DefaultBackoff = []time.Duration{
	2 * time.Second,
	5 * time.Second,
	10 * time.Second,
	30 * time.Second,
}

// Options configures Run. Zero fields fall back to package defaults.
type Options struct {
	// Servers is the ordered list of SNTP servers ("host" or "host:port").
	// Defaults to DefaultServers.
	Servers []string
	// Timeout is the per-server query timeout. Defaults to DefaultTimeout.
	Timeout time.Duration
	// Interval is the delay between successful syncs. Defaults to DefaultInterval.
	Interval time.Duration
	// Backoff is the bounded retry schedule when all servers fail.
	// Defaults to DefaultBackoff.
	Backoff []time.Duration
	// SetClock applies the synchronised time to the system. Defaults to the
	// platform clock setter (clock_settime + adjtimex on Linux).
	SetClock func(time.Time) error
	// Now returns the local clock reading. Defaults to time.Now.
	Now func() time.Time
	// Log receives sync and warning messages. Defaults to slog.Default().
	Log *slog.Logger
}

// Response is the validated result of a single SNTP query.
type Response struct {
	// Time is the estimated UTC time at the moment the reply arrived.
	Time time.Time
	// Offset is the adjustment relative to the local clock at receipt.
	Offset time.Duration
	// RTT is the estimated round-trip network delay.
	RTT time.Duration
	// Stratum is the server's stratum (1..15).
	Stratum uint8
}

// Run keeps the system clock synchronised until ctx is cancelled.
func Run(ctx context.Context, opts Options) {
	servers := opts.Servers
	if len(servers) == 0 {
		servers = DefaultServers
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	backoff := opts.Backoff
	if len(backoff) == 0 {
		backoff = DefaultBackoff
	}
	setClock := opts.SetClock
	if setClock == nil {
		setClock = setSystemClock
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}

	log.Info("sntp client started", "servers", servers)

	backoffIdx := 0
	for {
		if ctx.Err() != nil {
			return
		}

		synced := false
		var lastErr error
		for _, srv := range servers {
			if ctx.Err() != nil {
				return
			}
			qctx, cancel := context.WithTimeout(ctx, timeout)
			resp, err := query(qctx, srv, now)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				lastErr = fmt.Errorf("%s: %w", srv, err)
				log.Debug("sntp query failed", "server", srv, "err", err)
				continue
			}

			if err := setClock(resp.Time); err != nil {
				lastErr = fmt.Errorf("set clock: %w", err)
				log.Warn("sntp failed to set system clock",
					"server", srv,
					"time", resp.Time.UTC().Format(time.RFC3339),
					"err", err)
				break
			}

			log.Info("sntp clock synchronised",
				"server", srv,
				"time", resp.Time.UTC().Format(time.RFC3339),
				"offset", resp.Offset.Round(time.Millisecond),
				"rtt", resp.RTT.Round(time.Millisecond),
				"stratum", resp.Stratum)
			synced = true
			backoffIdx = 0
			break
		}

		wait := interval
		if !synced {
			wait = backoff[min(backoffIdx, len(backoff)-1)]
			backoffIdx++
			log.Warn("sntp sync failed, retrying", "retry_in", wait, "err", lastErr)
		}

		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// Query performs a single SNTPv4 request against server ("host" or "host:port")
// and returns the validated time estimate.
func Query(ctx context.Context, server string) (Response, error) {
	return query(ctx, server, time.Now)
}

func query(ctx context.Context, server string, now func() time.Time) (Response, error) {
	addr := normalizeAddr(server)
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", addr)
	if err != nil {
		return Response{}, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	if dl, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(dl); err != nil {
			return Response{}, fmt.Errorf("deadline: %w", err)
		}
	}
	// Unblock Read/Write immediately if ctx is cancelled before the deadline.
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()

	t1 := now()
	reqTx := timeToNTP(t1)
	if reqTx == 0 {
		// Avoid a zero Transmit Timestamp; some servers treat 0 specially.
		reqTx = 1
	}
	req := buildRequest(reqTx)
	if _, err := conn.Write(req[:]); err != nil {
		return Response{}, fmt.Errorf("send: %w", err)
	}

	var buf [packetLen + 64]byte
	n, err := conn.Read(buf[:])
	if err != nil {
		return Response{}, fmt.Errorf("recv: %w", err)
	}
	t4 := now()

	return parseResponse(buf[:n], reqTx, t1, t4)
}

// normalizeAddr ensures server has a port, defaulting to 123.
func normalizeAddr(server string) string {
	if _, _, err := net.SplitHostPort(server); err == nil {
		return server
	}
	host := strings.TrimPrefix(strings.TrimSuffix(server, "]"), "[")
	return net.JoinHostPort(host, "123")
}

// buildRequest encodes a 48-byte RFC 4330 SNTPv4 client request packet.
func buildRequest(tx uint64) [packetLen]byte {
	var pkt [packetLen]byte
	pkt[0] = clientHeader
	binary.BigEndian.PutUint64(pkt[40:48], tx)
	return pkt
}

// parseResponse validates an SNTP reply per RFC 4330 Section 5 and computes
// the estimated current time, offset, and round-trip delay.
func parseResponse(buf []byte, reqTx uint64, t1, t4 time.Time) (Response, error) {
	if len(buf) < packetLen {
		return Response{}, fmt.Errorf("short packet: %d bytes", len(buf))
	}

	li := buf[0] >> 6
	if li == 3 {
		return Response{}, errors.New("server clock unsynchronised (LI=3)")
	}

	mode := buf[0] & 0x07
	if mode != 4 && mode != 5 {
		return Response{}, fmt.Errorf("unexpected mode %d", mode)
	}

	stratum := buf[1]
	if stratum == 0 {
		kiss := strings.TrimRight(string(buf[12:16]), "\x00")
		if kiss != "" {
			return Response{}, fmt.Errorf("kiss-o'-death (%q)", kiss)
		}
		return Response{}, errors.New("invalid stratum 0 (kiss-o'-death)")
	}
	if stratum > 15 {
		return Response{}, fmt.Errorf("invalid stratum %d", stratum)
	}

	orig := binary.BigEndian.Uint64(buf[24:32])
	if orig != reqTx {
		return Response{}, fmt.Errorf("originate timestamp mismatch: got %#x, want %#x", orig, reqTx)
	}

	rxRaw := binary.BigEndian.Uint64(buf[32:40])
	txRaw := binary.BigEndian.Uint64(buf[40:48])
	if txRaw == 0 {
		return Response{}, errors.New("zero transmit timestamp")
	}

	t3 := ntpToTime(txRaw)
	// Using rtt = (T4 - T1) - (T3 - T2) and target = T3 + rtt/2 is algebraically
	// identical to T4 + ((T2 - T1) + (T3 - T4))/2, while avoiding duration
	// overflow even if the local clock is centuries away from real time.
	rtt := max(t4.Sub(t1), 0)
	if rxRaw != 0 {
		t2 := ntpToTime(rxRaw)
		if proc := t3.Sub(t2); proc > 0 && proc <= rtt {
			rtt -= proc
		}
	}

	target := t3.Add(rtt / 2)
	return Response{
		Time:    target,
		Offset:  target.Sub(t4),
		RTT:     rtt,
		Stratum: stratum,
	}, nil
}

// timeToNTP converts a time.Time to an unsigned 64-bit NTP timestamp
// (32-bit seconds since 1900-01-01 UTC, 32-bit fractional seconds).
func timeToNTP(t time.Time) uint64 {
	sec := uint32(uint64(t.Unix()) + ntpEpochOffset)
	frac := uint32((uint64(t.Nanosecond()) << 32) / 1e9)
	return uint64(sec)<<32 | uint64(frac)
}

// ntpToTime converts an unsigned 64-bit NTP timestamp to UTC time.Time.
// Timestamps with MSB=1 map to Era 0 (1968..2036); MSB=0 maps to Era 1 (2036..2104).
func ntpToTime(ts uint64) time.Time {
	sec := uint32(ts >> 32)
	frac := uint32(ts)

	var unixSec int64
	if sec >= ntpEraPivot {
		unixSec = int64(sec) - ntpEpochOffset
	} else {
		unixSec = int64(sec) + (1 << 32) - ntpEpochOffset
	}
	nsec := int64((uint64(frac)*1e9 + (1 << 31)) >> 32)
	return time.Unix(unixSec, nsec).UTC()
}
