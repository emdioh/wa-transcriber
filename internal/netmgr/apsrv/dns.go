// Package apsrv implements an in-process DHCPv4 server and wildcard DNS
// responder for the setup access point, replacing dnsmasq.
package apsrv

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
)

const (
	// DefaultDNSTTL is the TTL in seconds for synthetic wildcard A responses.
	DefaultDNSTTL = 30

	dnsHeaderLen = 12
	dnsTypeA     = 1
	dnsClassIN   = 1
	dnsMaxName   = 255
	dnsBufSize   = 2048
)

// DNSServer is a minimal RFC 1035 wildcard DNS UDP responder for a captive
// portal setup AP. Every A/IN query receives ServerIP; all other query types
// (AAAA, HTTPS, TXT, etc.) receive an empty NOERROR response so clients do not
// stall waiting on IPv6 or secondary lookups.
type DNSServer struct {
	serverIP [4]byte
	ttl      uint32
	log      *slog.Logger
}

// NewDNSServer creates a wildcard DNS responder that answers A queries with
// serverIP.
func NewDNSServer(serverIP net.IP, log *slog.Logger) (*DNSServer, error) {
	ip4 := serverIP.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("apsrv: dns server IP %v is not IPv4", serverIP)
	}
	if log == nil {
		log = slog.Default()
	}
	s := &DNSServer{
		ttl: DefaultDNSTTL,
		log: log,
	}
	copy(s.serverIP[:], ip4)
	return s, nil
}

// HandleQuery parses a single DNS query packet and builds the wire-format
// response. Returns (nil, false) for malformed packets or non-queries.
func (s *DNSServer) HandleQuery(req []byte) ([]byte, bool) {
	if len(req) < dnsHeaderLen {
		return nil, false
	}

	flags := binary.BigEndian.Uint16(req[2:4])
	// Ignore responses (QR=1) and non-standard opcodes (OPCODE != 0).
	if flags&0x8000 != 0 || (flags>>11)&0x0F != 0 {
		return nil, false
	}

	qdcount := binary.BigEndian.Uint16(req[4:6])
	if qdcount == 0 {
		return nil, false
	}

	// Parse first QNAME starting at byte 12.
	off := dnsHeaderLen
	for {
		if off >= len(req) {
			return nil, false
		}
		labelLen := int(req[off])
		if labelLen == 0 {
			off++
			break
		}
		// Reject compression pointers and reserved label types in the first question.
		if labelLen&0xC0 != 0 {
			return nil, false
		}
		off += 1 + labelLen
		if off > len(req) || off-dnsHeaderLen >= dnsMaxName {
			return nil, false
		}
	}

	// QTYPE (2 bytes) + QCLASS (2 bytes) follow QNAME.
	if off+4 > len(req) {
		return nil, false
	}
	qtype := binary.BigEndian.Uint16(req[off : off+2])
	qclass := binary.BigEndian.Uint16(req[off+2 : off+4])
	qEnd := off + 4
	question := req[dnsHeaderLen:qEnd]

	answerA := qtype == dnsTypeA && qclass == dnsClassIN

	respLen := dnsHeaderLen + len(question)
	if answerA {
		respLen += 16
	}
	resp := make([]byte, respLen)

	// Header: copy ID; QR=1, AA=1, RD copied from query, RA=1, RCODE=0.
	copy(resp[0:2], req[0:2])
	respFlags := uint16(0x8480) | (flags & 0x0100)
	binary.BigEndian.PutUint16(resp[2:4], respFlags)
	binary.BigEndian.PutUint16(resp[4:6], 1) // QDCOUNT=1
	if answerA {
		binary.BigEndian.PutUint16(resp[6:8], 1) // ANCOUNT=1
	}
	// NSCOUNT (8:10) and ARCOUNT (10:12) remain 0.

	// Echo exact question bytes.
	copy(resp[dnsHeaderLen:qEnd], question)

	if answerA {
		ans := resp[qEnd:]
		ans[0] = 0xC0 // Name compression pointer to offset 12 (QNAME).
		ans[1] = 0x0C
		binary.BigEndian.PutUint16(ans[2:4], dnsTypeA)
		binary.BigEndian.PutUint16(ans[4:6], dnsClassIN)
		binary.BigEndian.PutUint32(ans[6:10], s.ttl)
		binary.BigEndian.PutUint16(ans[10:12], 4)
		copy(ans[12:16], s.serverIP[:])
	}

	return resp, true
}

// Serve reads DNS queries from conn and writes responses until conn is closed.
func (s *DNSServer) Serve(conn net.PacketConn) error {
	buf := make([]byte, dnsBufSize)
	for {
		n, src, err := conn.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		resp, ok := s.HandleQuery(buf[:n])
		if !ok {
			continue
		}
		if _, err := conn.WriteTo(resp, src); err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			s.log.Debug("apsrv: dns write failed", "dst", src, "err", err)
		}
	}
}
