// Package dhcpc implements a pure-Go RFC 2131 DHCPv4 client.
package dhcpc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
)

const (
	// ServerPort and ClientPort are the standard DHCPv4 UDP ports.
	ServerPort = 67
	ClientPort = 68

	// BOOTP opcodes (RFC 2131 section 2).
	OpRequest byte = 1
	OpReply   byte = 2

	// Hardware address type and length for 10Mb Ethernet.
	HTypeEthernet byte = 1
	HLenEthernet  byte = 6

	// FlagBroadcast requests broadcast replies when the client has no IP yet.
	FlagBroadcast uint16 = 0x8000

	// Fixed BOOTP header (236 bytes) + 4-byte DHCP magic cookie.
	headerSize    = 236
	minPacketSize = 240
	// Minimum BOOTP/DHCP wire packet size for interoperability with relays.
	minWireSize = 300

	// MinLeaseDuration clamps unreasonably short server leases to prevent
	// renewal spin loops.
	MinLeaseDuration = 30 * time.Second
)

// DHCP option codes (RFC 2132).
const (
	OptPad           byte = 0
	OptSubnetMask    byte = 1
	OptRouter        byte = 3
	OptDNS           byte = 6
	OptHostname      byte = 12
	OptRequestedIP   byte = 50
	OptLeaseTime     byte = 51
	OptMessageType   byte = 53
	OptServerID      byte = 54
	OptParamReqList  byte = 55
	OptRenewalTime   byte = 58
	OptRebindingTime byte = 59
	OptClientID      byte = 61
	OptEnd           byte = 255
)

// MessageType is the value of DHCP option 53.
type MessageType byte

const (
	MsgDiscover MessageType = 1
	MsgOffer    MessageType = 2
	MsgRequest  MessageType = 3
	MsgDecline  MessageType = 4
	MsgAck      MessageType = 5
	MsgNak      MessageType = 6
	MsgRelease  MessageType = 7
	MsgInform   MessageType = 8
)

// String returns the RFC 2131 name of the DHCP message type.
func (m MessageType) String() string {
	switch m {
	case MsgDiscover:
		return "DISCOVER"
	case MsgOffer:
		return "OFFER"
	case MsgRequest:
		return "REQUEST"
	case MsgDecline:
		return "DECLINE"
	case MsgAck:
		return "ACK"
	case MsgNak:
		return "NAK"
	case MsgRelease:
		return "RELEASE"
	case MsgInform:
		return "INFORM"
	default:
		return fmt.Sprintf("MessageType(%d)", byte(m))
	}
}

var (
	magicCookie = [4]byte{99, 130, 83, 99}

	defaultParamReqList = []byte{
		OptSubnetMask,
		OptRouter,
		OptDNS,
		OptLeaseTime,
		OptRenewalTime,
		OptRebindingTime,
	}

	// ErrShortPacket is returned when a DHCP packet is shorter than 240 bytes.
	ErrShortPacket = errors.New("dhcpc: packet too short")
	// ErrBadMagicCookie is returned when bytes 236..239 do not match 99.130.83.99.
	ErrBadMagicCookie = errors.New("dhcpc: invalid DHCP magic cookie")
	// ErrMalformedOption is returned when a DHCP option is truncated or has an invalid length.
	ErrMalformedOption = errors.New("dhcpc: malformed DHCP option")
)

// Packet represents a decoded or outgoing RFC 2131 DHCPv4 message.
type Packet struct {
	Op     byte
	HType  byte
	HLen   byte
	Hops   byte
	XID    uint32
	Secs   uint16
	Flags  uint16
	CIAddr net.IP
	YIAddr net.IP
	SIAddr net.IP
	GIAddr net.IP
	CHAddr net.HardwareAddr

	// Parsed DHCP options.
	MessageType   MessageType
	SubnetMask    net.IPMask
	Router        net.IP
	DNS           []net.IP
	Hostname      string
	RequestedIP   net.IP
	LeaseTime     time.Duration
	ServerID      net.IP
	ParamReqList  []byte
	RenewalTime   time.Duration
	RebindingTime time.Duration
	ClientID      []byte
}

// Lease describes an acquired IPv4 DHCP lease.
type Lease struct {
	IP        net.IP
	Mask      net.IPMask
	PrefixLen int
	Router    net.IP
	DNS       []net.IP
	ServerID  net.IP
	Duration  time.Duration
	T1        time.Duration
	T2        time.Duration
	Obtained  time.Time
}

// NewDiscover builds a DHCPDISCOVER packet.
func NewDiscover(xid uint32, mac net.HardwareAddr, hostname string) *Packet {
	return &Packet{
		Op:           OpRequest,
		HType:        HTypeEthernet,
		HLen:         byte(len(mac)),
		XID:          xid,
		Flags:        FlagBroadcast,
		CHAddr:       cloneBytes(mac),
		MessageType:  MsgDiscover,
		ClientID:     ethernetClientID(mac),
		Hostname:     hostname,
		ParamReqList: cloneBytes(defaultParamReqList),
	}
}

// NewRequest builds a DHCPREQUEST packet for the SELECTING state (answering an OFFER).
func NewRequest(xid uint32, mac net.HardwareAddr, hostname string, requestedIP, serverID net.IP) *Packet {
	return &Packet{
		Op:           OpRequest,
		HType:        HTypeEthernet,
		HLen:         byte(len(mac)),
		XID:          xid,
		Flags:        FlagBroadcast,
		CHAddr:       cloneBytes(mac),
		MessageType:  MsgRequest,
		ClientID:     ethernetClientID(mac),
		RequestedIP:  cloneIP4(requestedIP),
		ServerID:     cloneIP4(serverID),
		Hostname:     hostname,
		ParamReqList: cloneBytes(defaultParamReqList),
	}
}

// NewRenewRequest builds a DHCPREQUEST packet for the RENEWING or REBINDING state.
// Per RFC 2131 Table 4, ciaddr carries the current IP and options 50/54 are omitted.
func NewRenewRequest(xid uint32, mac net.HardwareAddr, hostname string, currentIP net.IP, broadcast bool) *Packet {
	var flags uint16
	if broadcast {
		flags = FlagBroadcast
	}
	return &Packet{
		Op:           OpRequest,
		HType:        HTypeEthernet,
		HLen:         byte(len(mac)),
		XID:          xid,
		Flags:        flags,
		CIAddr:       cloneIP4(currentIP),
		CHAddr:       cloneBytes(mac),
		MessageType:  MsgRequest,
		ClientID:     ethernetClientID(mac),
		Hostname:     hostname,
		ParamReqList: cloneBytes(defaultParamReqList),
	}
}

// Marshal encodes p into RFC 2131 wire format, padded to at least 300 bytes.
func (p *Packet) Marshal() ([]byte, error) {
	if len(p.CHAddr) > 16 {
		return nil, fmt.Errorf("dhcpc: chaddr length %d exceeds 16", len(p.CHAddr))
	}
	htype := p.HType
	if htype == 0 {
		htype = HTypeEthernet
	}
	hlen := p.HLen
	if hlen == 0 && len(p.CHAddr) > 0 {
		hlen = byte(len(p.CHAddr))
	}
	if hlen > 16 {
		return nil, fmt.Errorf("dhcpc: hlen %d exceeds 16", hlen)
	}

	buf := make([]byte, minPacketSize, minWireSize)
	buf[0] = p.Op
	buf[1] = htype
	buf[2] = hlen
	buf[3] = p.Hops
	binary.BigEndian.PutUint32(buf[4:8], p.XID)
	binary.BigEndian.PutUint16(buf[8:10], p.Secs)
	binary.BigEndian.PutUint16(buf[10:12], p.Flags)
	putIP4(buf[12:16], p.CIAddr)
	putIP4(buf[16:20], p.YIAddr)
	putIP4(buf[20:24], p.SIAddr)
	putIP4(buf[24:28], p.GIAddr)
	copy(buf[28:44], p.CHAddr)
	copy(buf[headerSize:minPacketSize], magicCookie[:])

	if p.MessageType != 0 {
		buf = appendOpt(buf, OptMessageType, byte(p.MessageType))
	}
	if len(p.ClientID) > 0 {
		if len(p.ClientID) > 255 {
			return nil, fmt.Errorf("dhcpc: client ID length %d exceeds 255", len(p.ClientID))
		}
		buf = appendOpt(buf, OptClientID, p.ClientID...)
	}
	if ip := p.RequestedIP.To4(); ip != nil {
		buf = appendOpt(buf, OptRequestedIP, ip...)
	}
	if ip := p.ServerID.To4(); ip != nil {
		buf = appendOpt(buf, OptServerID, ip...)
	}
	if p.Hostname != "" {
		host := p.Hostname
		if len(host) > 255 {
			host = host[:255]
		}
		buf = appendOpt(buf, OptHostname, []byte(host)...)
	}
	if len(p.SubnetMask) == 4 {
		buf = appendOpt(buf, OptSubnetMask, p.SubnetMask...)
	}
	if ip := p.Router.To4(); ip != nil {
		buf = appendOpt(buf, OptRouter, ip...)
	}
	if len(p.DNS) > 0 {
		var dnsBytes []byte
		for _, s := range p.DNS {
			if ip := s.To4(); ip != nil {
				dnsBytes = append(dnsBytes, ip...)
			}
		}
		if len(dnsBytes) > 0 && len(dnsBytes) <= 252 {
			buf = appendOpt(buf, OptDNS, dnsBytes...)
		}
	}
	if p.LeaseTime > 0 {
		buf = appendUint32Opt(buf, OptLeaseTime, durationToSeconds(p.LeaseTime))
	}
	if p.RenewalTime > 0 {
		buf = appendUint32Opt(buf, OptRenewalTime, durationToSeconds(p.RenewalTime))
	}
	if p.RebindingTime > 0 {
		buf = appendUint32Opt(buf, OptRebindingTime, durationToSeconds(p.RebindingTime))
	}
	if len(p.ParamReqList) > 0 {
		if len(p.ParamReqList) > 255 {
			return nil, fmt.Errorf("dhcpc: param request list length %d exceeds 255", len(p.ParamReqList))
		}
		buf = appendOpt(buf, OptParamReqList, p.ParamReqList...)
	}

	buf = append(buf, OptEnd)
	if len(buf) < minWireSize {
		buf = append(buf, make([]byte, minWireSize-len(buf))...)
	}
	return buf, nil
}

// ParsePacket decodes an RFC 2131 DHCPv4 packet from raw UDP payload bytes.
func ParsePacket(b []byte) (*Packet, error) {
	if len(b) < minPacketSize {
		return nil, fmt.Errorf("%w: got %d bytes, want >= %d", ErrShortPacket, len(b), minPacketSize)
	}
	if !bytes.Equal(b[headerSize:minPacketSize], magicCookie[:]) {
		return nil, fmt.Errorf("%w: got %x", ErrBadMagicCookie, b[headerSize:minPacketSize])
	}
	hlen := int(b[2])
	if hlen > 16 {
		return nil, fmt.Errorf("dhcpc: invalid hlen %d", hlen)
	}

	p := &Packet{
		Op:     b[0],
		HType:  b[1],
		HLen:   b[2],
		Hops:   b[3],
		XID:    binary.BigEndian.Uint32(b[4:8]),
		Secs:   binary.BigEndian.Uint16(b[8:10]),
		Flags:  binary.BigEndian.Uint16(b[10:12]),
		CIAddr: cloneIP4(net.IP(b[12:16])),
		YIAddr: cloneIP4(net.IP(b[16:20])),
		SIAddr: cloneIP4(net.IP(b[20:24])),
		GIAddr: cloneIP4(net.IP(b[24:28])),
		CHAddr: net.HardwareAddr(cloneBytes(b[28 : 28+hlen])),
	}

	opts := b[minPacketSize:]
	for i := 0; i < len(opts); {
		code := opts[i]
		i++
		if code == OptPad {
			continue
		}
		if code == OptEnd {
			break
		}
		if i >= len(opts) {
			return nil, fmt.Errorf("%w: option %d missing length byte", ErrMalformedOption, code)
		}
		optLen := int(opts[i])
		i++
		if i+optLen > len(opts) {
			return nil, fmt.Errorf("%w: option %d declares length %d with only %d bytes left", ErrMalformedOption, code, optLen, len(opts)-i)
		}
		val := opts[i : i+optLen]
		i += optLen

		switch code {
		case OptMessageType:
			if optLen != 1 {
				return nil, fmt.Errorf("%w: option 53 length %d, want 1", ErrMalformedOption, optLen)
			}
			p.MessageType = MessageType(val[0])
		case OptSubnetMask:
			if optLen != 4 {
				return nil, fmt.Errorf("%w: option 1 length %d, want 4", ErrMalformedOption, optLen)
			}
			p.SubnetMask = net.IPMask(cloneBytes(val))
		case OptRouter:
			if optLen%4 != 0 {
				return nil, fmt.Errorf("%w: option 3 length %d not a multiple of 4", ErrMalformedOption, optLen)
			}
			if optLen >= 4 && p.Router == nil {
				p.Router = cloneIP4(net.IP(val[:4]))
			}
		case OptDNS:
			if optLen%4 != 0 {
				return nil, fmt.Errorf("%w: option 6 length %d not a multiple of 4", ErrMalformedOption, optLen)
			}
			for off := 0; off < optLen; off += 4 {
				p.DNS = append(p.DNS, cloneIP4(net.IP(val[off:off+4])))
			}
		case OptHostname:
			p.Hostname = string(val)
		case OptRequestedIP:
			if optLen != 4 {
				return nil, fmt.Errorf("%w: option 50 length %d, want 4", ErrMalformedOption, optLen)
			}
			p.RequestedIP = cloneIP4(net.IP(val))
		case OptLeaseTime:
			if optLen != 4 {
				return nil, fmt.Errorf("%w: option 51 length %d, want 4", ErrMalformedOption, optLen)
			}
			p.LeaseTime = time.Duration(binary.BigEndian.Uint32(val)) * time.Second
		case OptServerID:
			if optLen != 4 {
				return nil, fmt.Errorf("%w: option 54 length %d, want 4", ErrMalformedOption, optLen)
			}
			p.ServerID = cloneIP4(net.IP(val))
		case OptParamReqList:
			p.ParamReqList = cloneBytes(val)
		case OptRenewalTime:
			if optLen != 4 {
				return nil, fmt.Errorf("%w: option 58 length %d, want 4", ErrMalformedOption, optLen)
			}
			p.RenewalTime = time.Duration(binary.BigEndian.Uint32(val)) * time.Second
		case OptRebindingTime:
			if optLen != 4 {
				return nil, fmt.Errorf("%w: option 59 length %d, want 4", ErrMalformedOption, optLen)
			}
			p.RebindingTime = time.Duration(binary.BigEndian.Uint32(val)) * time.Second
		case OptClientID:
			p.ClientID = cloneBytes(val)
		}
	}

	return p, nil
}

// LeaseFromPacket constructs a normalized Lease from a DHCPACK packet.
// Lease duration is clamped to MinLeaseDuration, and omitted T1/T2 default to
// Duration/2 and Duration*7/8 per RFC 2131 section 4.4.5.
func LeaseFromPacket(p *Packet, obtained time.Time) (Lease, error) {
	if p == nil {
		return Lease{}, errors.New("dhcpc: nil packet")
	}
	if p.MessageType != MsgAck {
		return Lease{}, fmt.Errorf("dhcpc: expected ACK packet, got %s", p.MessageType)
	}
	ip := p.YIAddr.To4()
	if ip == nil || ip.Equal(net.IPv4zero) {
		return Lease{}, errors.New("dhcpc: ACK missing valid yiaddr")
	}

	mask := p.SubnetMask
	if mask == nil {
		mask = ip.DefaultMask()
	}
	ones, bits := mask.Size()
	if bits != 32 {
		return Lease{}, fmt.Errorf("dhcpc: invalid subnet mask %v", mask)
	}

	dur := p.LeaseTime
	if dur < MinLeaseDuration {
		dur = MinLeaseDuration
	}
	t2 := p.RebindingTime
	if t2 <= 0 || t2 >= dur {
		t2 = dur * 7 / 8
	}
	t1 := p.RenewalTime
	if t1 <= 0 || t1 >= t2 {
		t1 = dur / 2
		if t1 >= t2 {
			t1 = t2 / 2
		}
	}

	if obtained.IsZero() {
		obtained = time.Now()
	}

	var dns []net.IP
	for _, s := range p.DNS {
		if v4 := s.To4(); v4 != nil && !v4.Equal(net.IPv4zero) {
			dns = append(dns, cloneIP4(v4))
		}
	}

	var router net.IP
	if r := p.Router.To4(); r != nil && !r.Equal(net.IPv4zero) {
		router = cloneIP4(r)
	}

	return Lease{
		IP:        cloneIP4(ip),
		Mask:      net.IPMask(cloneBytes(mask)),
		PrefixLen: ones,
		Router:    router,
		DNS:       dns,
		ServerID:  p.effectiveServerID(),
		Duration:  dur,
		T1:        t1,
		T2:        t2,
		Obtained:  obtained,
	}, nil
}

func (p *Packet) effectiveServerID() net.IP {
	if ip := p.ServerID.To4(); ip != nil && !ip.Equal(net.IPv4zero) {
		return cloneIP4(ip)
	}
	if ip := p.SIAddr.To4(); ip != nil && !ip.Equal(net.IPv4zero) {
		return cloneIP4(ip)
	}
	return nil
}

func ethernetClientID(mac net.HardwareAddr) []byte {
	if len(mac) == 0 {
		return nil
	}
	out := make([]byte, 1+len(mac))
	out[0] = HTypeEthernet
	copy(out[1:], mac)
	return out
}

func appendOpt(dst []byte, code byte, val ...byte) []byte {
	dst = append(dst, code, byte(len(val)))
	return append(dst, val...)
}

func appendUint32Opt(dst []byte, code byte, v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return appendOpt(dst, code, b[:]...)
}

func durationToSeconds(d time.Duration) uint32 {
	if d <= 0 {
		return 0
	}
	sec := d / time.Second
	if sec > 0xffffffff {
		return 0xffffffff
	}
	return uint32(sec)
}

func putIP4(dst []byte, ip net.IP) {
	if v4 := ip.To4(); v4 != nil {
		copy(dst[:4], v4)
	}
}

func cloneIP4(ip net.IP) net.IP {
	v4 := ip.To4()
	if v4 == nil {
		return nil
	}
	out := make(net.IP, 4)
	copy(out, v4)
	return out
}

func cloneBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
