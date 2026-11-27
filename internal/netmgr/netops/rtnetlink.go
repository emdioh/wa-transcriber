package netops

import (
	"encoding/binary"
	"fmt"
	"net"
)

// Netlink header and message constants (linux/netlink.h).
const (
	nlmsgHdrLen = 16
	nlmsgErrLen = 4 // minimum error code field inside struct nlmsgerr
	rtaHdrLen   = 4

	nlmsgNoop    = 1
	nlmsgError   = 2
	nlmsgDone    = 3
	nlmsgOverrun = 4

	nlmFRequest = 0x0001
	nlmFMulti   = 0x0002
	nlmFAck     = 0x0004
	nlmFRoot    = 0x0100
	nlmFMatch   = 0x0200
	nlmFDump    = nlmFRoot | nlmFMatch
	nlmFReplace = 0x0100
	nlmFExcl    = 0x0200
	nlmFCreate  = 0x0400

	nlaTypeMask = 0x3fff
)

// RTNETLINK message types, struct sizes, and attribute constants
// (linux/rtnetlink.h, linux/if_link.h, linux/if_addr.h, linux/if.h).
const (
	rtmNewLink  = 16
	rtmNewAddr  = 20
	rtmDelAddr  = 21
	rtmNewRoute = 24

	ifInfoMsgLen = 16
	ifAddrMsgLen = 8
	rtMsgLen     = 12

	afUnspec = 0
	afInet   = 2

	iffUp = 0x1

	ifaAddress   = 1
	ifaLocal     = 2
	ifaBroadcast = 4

	rtTableMain     = 254
	rtProtBoot      = 3
	rtScopeUniverse = 0
	rtNUnicast      = 1

	rtaOIF     = 4
	rtaGateway = 5
)

// nlMsgHdr represents a decoded struct nlmsghdr.
type nlMsgHdr struct {
	Len   uint32
	Type  uint16
	Flags uint16
	Seq   uint32
	Pid   uint32
}

// nlMsg represents one decoded Netlink message from a stream/datagram.
type nlMsg struct {
	Header nlMsgHdr
	Data   []byte
}

// nlAttr represents one decoded struct rtattr / struct nlattr.
type nlAttr struct {
	Type  uint16
	Value []byte
}

// nlBuilder constructs a single Netlink message with native-endian headers
// and 4-byte aligned attributes.
type nlBuilder struct {
	buf []byte
}

func newNLBuilder(msgType, flags uint16, seq uint32, bodyLen int) *nlBuilder {
	b := &nlBuilder{buf: make([]byte, nlmsgHdrLen+bodyLen)}
	binary.NativeEndian.PutUint16(b.buf[4:6], msgType)
	binary.NativeEndian.PutUint16(b.buf[6:8], flags)
	binary.NativeEndian.PutUint32(b.buf[8:12], seq)
	// pid (bytes 12..15) stays 0 (kernel assigns port ID).
	return b
}

func (b *nlBuilder) body() []byte {
	return b.buf[nlmsgHdrLen:]
}

func (b *nlBuilder) addAttr(attrType uint16, val []byte) {
	rawLen := rtaHdrLen + len(val)
	alignedLen := align4(rawLen)
	off := len(b.buf)
	b.buf = append(b.buf, make([]byte, alignedLen)...)
	binary.NativeEndian.PutUint16(b.buf[off:off+2], uint16(rawLen))
	binary.NativeEndian.PutUint16(b.buf[off+2:off+4], attrType)
	copy(b.buf[off+rtaHdrLen:off+rawLen], val)
}

func (b *nlBuilder) addAttrUint32(attrType uint16, v uint32) {
	var raw [4]byte
	binary.NativeEndian.PutUint32(raw[:], v)
	b.addAttr(attrType, raw[:])
}

func (b *nlBuilder) addAttrString(attrType uint16, s string) {
	raw := make([]byte, len(s)+1) // NUL-terminated
	copy(raw, s)
	b.addAttr(attrType, raw)
}

func (b *nlBuilder) bytes() []byte {
	binary.NativeEndian.PutUint32(b.buf[0:4], uint32(len(b.buf)))
	return b.buf
}

// encodeLinkState encodes an RTM_NEWLINK request to bring an interface up or down.
func encodeLinkState(ifindex int, up bool, seq uint32) []byte {
	b := newNLBuilder(rtmNewLink, nlmFRequest|nlmFAck, seq, ifInfoMsgLen)
	body := b.body()
	body[0] = afUnspec
	// body[1] = pad (0), body[2:4] = ifi_type (0)
	binary.NativeEndian.PutUint32(body[4:8], uint32(int32(ifindex)))
	if up {
		binary.NativeEndian.PutUint32(body[8:12], iffUp)
	}
	binary.NativeEndian.PutUint32(body[12:16], iffUp)
	return b.bytes()
}

// encodeAddrMsg encodes an RTM_NEWADDR or RTM_DELADDR request for an IPv4 address.
func encodeAddrMsg(msgType, flags uint16, ifindex int, ip4 net.IP, prefixLen int, seq uint32) []byte {
	b := newNLBuilder(msgType, flags, seq, ifAddrMsgLen)
	body := b.body()
	body[0] = afInet
	body[1] = uint8(prefixLen)
	body[2] = 0 // ifa_flags
	body[3] = rtScopeUniverse
	binary.NativeEndian.PutUint32(body[4:8], uint32(ifindex))

	b.addAttr(ifaLocal, ip4)
	b.addAttr(ifaAddress, ip4)
	if msgType == rtmNewAddr {
		if brd := ipv4Broadcast(ip4, prefixLen); brd != nil {
			b.addAttr(ifaBroadcast, brd)
		}
	}
	return b.bytes()
}

// encodeAddrAdd encodes an RTM_NEWADDR request with NLM_F_CREATE|NLM_F_EXCL.
func encodeAddrAdd(ifindex int, ip4 net.IP, prefixLen int, seq uint32) []byte {
	return encodeAddrMsg(rtmNewAddr, nlmFRequest|nlmFAck|nlmFCreate|nlmFExcl, ifindex, ip4, prefixLen, seq)
}

// encodeAddrReplace encodes an RTM_NEWADDR request with NLM_F_CREATE|NLM_F_REPLACE.
func encodeAddrReplace(ifindex int, ip4 net.IP, prefixLen int, seq uint32) []byte {
	return encodeAddrMsg(rtmNewAddr, nlmFRequest|nlmFAck|nlmFCreate|nlmFReplace, ifindex, ip4, prefixLen, seq)
}

// encodeAddrDel encodes an RTM_DELADDR request.
func encodeAddrDel(ifindex int, ip4 net.IP, prefixLen int, seq uint32) []byte {
	return encodeAddrMsg(rtmDelAddr, nlmFRequest|nlmFAck, ifindex, ip4, prefixLen, seq)
}

// encodeRouteReplaceDefault encodes an RTM_NEWROUTE request for 0.0.0.0/0 via gw on ifindex.
func encodeRouteReplaceDefault(ifindex int, gw4 net.IP, seq uint32) []byte {
	b := newNLBuilder(rtmNewRoute, nlmFRequest|nlmFAck|nlmFCreate|nlmFReplace, seq, rtMsgLen)
	body := b.body()
	body[0] = afInet
	body[1] = 0 // rtm_dst_len = 0 (default route)
	body[2] = 0 // rtm_src_len
	body[3] = 0 // rtm_tos
	body[4] = rtTableMain
	body[5] = rtProtBoot
	body[6] = rtScopeUniverse
	body[7] = rtNUnicast
	// body[8:12] = rtm_flags (0)

	b.addAttr(rtaGateway, gw4)
	b.addAttrUint32(rtaOIF, uint32(int32(ifindex)))
	return b.bytes()
}

// parseNLMessages splits a Netlink datagram into individual messages.
func parseNLMessages(buf []byte) ([]nlMsg, error) {
	var msgs []nlMsg
	for len(buf) > 0 {
		if len(buf) < nlmsgHdrLen {
			return nil, fmt.Errorf("netops: truncated netlink header (%d bytes)", len(buf))
		}
		hdr := nlMsgHdr{
			Len:   binary.NativeEndian.Uint32(buf[0:4]),
			Type:  binary.NativeEndian.Uint16(buf[4:6]),
			Flags: binary.NativeEndian.Uint16(buf[6:8]),
			Seq:   binary.NativeEndian.Uint32(buf[8:12]),
			Pid:   binary.NativeEndian.Uint32(buf[12:16]),
		}
		if hdr.Len < nlmsgHdrLen || int(hdr.Len) > len(buf) {
			return nil, fmt.Errorf("netops: invalid netlink message length %d (remaining %d)", hdr.Len, len(buf))
		}
		msgs = append(msgs, nlMsg{
			Header: hdr,
			Data:   buf[nlmsgHdrLen:hdr.Len],
		})
		step := align4(int(hdr.Len))
		if step >= len(buf) {
			break
		}
		buf = buf[step:]
	}
	return msgs, nil
}

// parseNLAttrs parses a sequence of 4-byte-aligned rtattr/nlattr attributes.
func parseNLAttrs(buf []byte) ([]nlAttr, error) {
	var attrs []nlAttr
	for len(buf) >= rtaHdrLen {
		aLen := int(binary.NativeEndian.Uint16(buf[0:2]))
		aType := binary.NativeEndian.Uint16(buf[2:4]) & nlaTypeMask
		if aLen < rtaHdrLen || aLen > len(buf) {
			return nil, fmt.Errorf("netops: invalid netlink attr length %d (remaining %d)", aLen, len(buf))
		}
		attrs = append(attrs, nlAttr{
			Type:  aType,
			Value: buf[rtaHdrLen:aLen],
		})
		step := align4(aLen)
		if step >= len(buf) {
			break
		}
		buf = buf[step:]
	}
	return attrs, nil
}

// parseNLMsgError parses an NLMSG_ERROR payload and returns the positive errno
// (0 indicates a successful ACK).
func parseNLMsgError(data []byte) (int, error) {
	if len(data) < nlmsgErrLen {
		return 0, fmt.Errorf("netops: truncated NLMSG_ERROR payload (%d bytes)", len(data))
	}
	code := int32(binary.NativeEndian.Uint32(data[0:4]))
	if code == 0 {
		return 0, nil
	}
	if code < 0 {
		return int(-code), nil
	}
	return int(code), nil
}

// parseNLAck scans parsed Netlink messages for an NLMSG_ERROR matching seq.
// It returns (found, errno, parseErr). errno == 0 means ACK (success).
func parseNLAck(msgs []nlMsg, seq uint32) (found bool, errno int, err error) {
	for _, m := range msgs {
		if m.Header.Seq != seq {
			continue
		}
		if m.Header.Type == nlmsgNoop {
			continue
		}
		if m.Header.Type == nlmsgError {
			code, err := parseNLMsgError(m.Data)
			return true, code, err
		}
	}
	return false, 0, nil
}
