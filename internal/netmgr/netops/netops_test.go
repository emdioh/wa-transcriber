package netops

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
)

func TestDefaultImplementsOps(t *testing.T) {
	var ops Ops = Default()
	if ops == nil {
		t.Fatal("Default() returned nil")
	}
}

func TestAlign4(t *testing.T) {
	cases := [][2]int{
		{0, 0}, {1, 4}, {2, 4}, {3, 4}, {4, 4},
		{5, 8}, {7, 8}, {8, 8}, {11, 12}, {16, 16},
	}
	for _, tc := range cases {
		if got := align4(tc[0]); got != tc[1] {
			t.Errorf("align4(%d) = %d, want %d", tc[0], got, tc[1])
		}
	}
}

func TestIPv4Broadcast(t *testing.T) {
	tests := []struct {
		ip   string
		pfx  int
		want string // empty => nil
	}{
		{"192.168.4.1", 24, "192.168.4.255"},
		{"169.254.12.34", 16, "169.254.255.255"},
		{"10.1.2.3", 8, "10.255.255.255"},
		{"192.168.1.5", 30, "192.168.1.7"},
		{"192.168.1.5", 31, ""},
		{"192.168.1.5", 32, ""},
		{"0.0.0.0", 0, "255.255.255.255"},
	}
	for _, tc := range tests {
		got := ipv4Broadcast(net.ParseIP(tc.ip).To4(), tc.pfx)
		if tc.want == "" {
			if got != nil {
				t.Errorf("ipv4Broadcast(%s/%d) = %v, want nil", tc.ip, tc.pfx, got)
			}
			continue
		}
		if got == nil || got.String() != tc.want {
			t.Errorf("ipv4Broadcast(%s/%d) = %v, want %s", tc.ip, tc.pfx, got, tc.want)
		}
	}
}

func TestValidateIPv4AndPrefix(t *testing.T) {
	if _, err := validateIPv4(nil); err == nil {
		t.Error("expected error for nil IP")
	}
	if _, err := validateIPv4(net.ParseIP("fe80::1")); err == nil {
		t.Error("expected error for IPv6")
	}
	if _, err := validateIPv4Prefix(net.IPv4(192, 168, 1, 1), -1); err == nil {
		t.Error("expected error for prefix -1")
	}
	if _, err := validateIPv4Prefix(net.IPv4(192, 168, 1, 1), 33); err == nil {
		t.Error("expected error for prefix 33")
	}
	ip4, err := validateIPv4Prefix(net.ParseIP("192.168.1.1"), 24)
	if err != nil || !ip4.Equal(net.IPv4(192, 168, 1, 1)) || len(ip4) != 4 {
		t.Errorf("validateIPv4Prefix valid case failed: ip4=%v err=%v", ip4, err)
	}
}

func TestEncodeLinkState(t *testing.T) {
	for _, tc := range []struct {
		name      string
		up        bool
		wantFlags uint32
	}{
		{"up", true, iffUp},
		{"down", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := encodeLinkState(7, tc.up, 42)
			if len(raw) != nlmsgHdrLen+ifInfoMsgLen {
				t.Fatalf("len = %d, want %d", len(raw), nlmsgHdrLen+ifInfoMsgLen)
			}
			msgs, err := parseNLMessages(raw)
			if err != nil || len(msgs) != 1 {
				t.Fatalf("parseNLMessages: msgs=%d err=%v", len(msgs), err)
			}
			h := msgs[0].Header
			if h.Len != uint32(len(raw)) || h.Type != rtmNewLink || h.Flags != (nlmFRequest|nlmFAck) || h.Seq != 42 || h.Pid != 0 {
				t.Fatalf("unexpected header: %+v", h)
			}
			d := msgs[0].Data
			if d[0] != afUnspec {
				t.Errorf("ifi_family = %d, want %d", d[0], afUnspec)
			}
			if idx := int32(binary.NativeEndian.Uint32(d[4:8])); idx != 7 {
				t.Errorf("ifi_index = %d, want 7", idx)
			}
			if fl := binary.NativeEndian.Uint32(d[8:12]); fl != tc.wantFlags {
				t.Errorf("ifi_flags = 0x%x, want 0x%x", fl, tc.wantFlags)
			}
			if ch := binary.NativeEndian.Uint32(d[12:16]); ch != iffUp {
				t.Errorf("ifi_change = 0x%x, want 0x%x", ch, iffUp)
			}
		})
	}
}

func TestEncodeAddrMessages(t *testing.T) {
	ip := net.IPv4(192, 168, 4, 1).To4()
	wantBrd := net.IPv4(192, 168, 4, 255).To4()

	tests := []struct {
		name      string
		encode    func() []byte
		wantType  uint16
		wantFlags uint16
		wantPfx   uint8
		wantBrd   net.IP
	}{
		{
			name:      "add /24",
			encode:    func() []byte { return encodeAddrAdd(3, ip, 24, 10) },
			wantType:  rtmNewAddr,
			wantFlags: nlmFRequest | nlmFAck | nlmFCreate | nlmFExcl,
			wantPfx:   24,
			wantBrd:   wantBrd,
		},
		{
			name:      "replace /24",
			encode:    func() []byte { return encodeAddrReplace(3, ip, 24, 11) },
			wantType:  rtmNewAddr,
			wantFlags: nlmFRequest | nlmFAck | nlmFCreate | nlmFReplace,
			wantPfx:   24,
			wantBrd:   wantBrd,
		},
		{
			name:      "replace /32 (no broadcast)",
			encode:    func() []byte { return encodeAddrReplace(3, ip, 32, 12) },
			wantType:  rtmNewAddr,
			wantFlags: nlmFRequest | nlmFAck | nlmFCreate | nlmFReplace,
			wantPfx:   32,
			wantBrd:   nil,
		},
		{
			name:      "del /24 (no broadcast)",
			encode:    func() []byte { return encodeAddrDel(3, ip, 24, 13) },
			wantType:  rtmDelAddr,
			wantFlags: nlmFRequest | nlmFAck,
			wantPfx:   24,
			wantBrd:   nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.encode()
			msgs, err := parseNLMessages(raw)
			if err != nil || len(msgs) != 1 {
				t.Fatalf("parseNLMessages: msgs=%d err=%v", len(msgs), err)
			}
			m := msgs[0]
			if m.Header.Type != tc.wantType || m.Header.Flags != tc.wantFlags {
				t.Fatalf("header type=%d flags=0x%x, want type=%d flags=0x%x",
					m.Header.Type, m.Header.Flags, tc.wantType, tc.wantFlags)
			}
			if len(m.Data) < ifAddrMsgLen {
				t.Fatalf("body too short: %d", len(m.Data))
			}
			if m.Data[0] != afInet || m.Data[1] != tc.wantPfx || m.Data[3] != rtScopeUniverse {
				t.Errorf("ifaddrmsg fields = %v", m.Data[:4])
			}
			if idx := binary.NativeEndian.Uint32(m.Data[4:8]); idx != 3 {
				t.Errorf("ifa_index = %d, want 3", idx)
			}

			attrs, err := parseNLAttrs(m.Data[ifAddrMsgLen:])
			if err != nil {
				t.Fatalf("parseNLAttrs: %v", err)
			}
			gotAttrs := map[uint16][]byte{}
			for _, a := range attrs {
				gotAttrs[a.Type] = a.Value
			}
			if !bytes.Equal(gotAttrs[ifaLocal], ip) {
				t.Errorf("IFA_LOCAL = %v, want %v", gotAttrs[ifaLocal], ip)
			}
			if !bytes.Equal(gotAttrs[ifaAddress], ip) {
				t.Errorf("IFA_ADDRESS = %v, want %v", gotAttrs[ifaAddress], ip)
			}
			if tc.wantBrd == nil {
				if _, ok := gotAttrs[ifaBroadcast]; ok {
					t.Errorf("unexpected IFA_BROADCAST = %v", gotAttrs[ifaBroadcast])
				}
			} else if !bytes.Equal(gotAttrs[ifaBroadcast], tc.wantBrd) {
				t.Errorf("IFA_BROADCAST = %v, want %v", gotAttrs[ifaBroadcast], tc.wantBrd)
			}
		})
	}
}

func TestEncodeRouteReplaceDefault(t *testing.T) {
	gw := net.IPv4(192, 168, 1, 1).To4()
	raw := encodeRouteReplaceDefault(5, gw, 99)

	msgs, err := parseNLMessages(raw)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("parseNLMessages: msgs=%d err=%v", len(msgs), err)
	}
	m := msgs[0]
	if m.Header.Type != rtmNewRoute || m.Header.Flags != (nlmFRequest|nlmFAck|nlmFCreate|nlmFReplace) || m.Header.Seq != 99 {
		t.Fatalf("unexpected header: %+v", m.Header)
	}
	body := m.Data
	if body[0] != afInet || body[1] != 0 || body[2] != 0 || body[3] != 0 ||
		body[4] != rtTableMain || body[5] != rtProtBoot || body[6] != rtScopeUniverse || body[7] != rtNUnicast {
		t.Fatalf("unexpected rtmsg fields: %v", body[:8])
	}
	if flags := binary.NativeEndian.Uint32(body[8:12]); flags != 0 {
		t.Errorf("rtm_flags = %d, want 0", flags)
	}

	attrs, err := parseNLAttrs(body[rtMsgLen:])
	if err != nil {
		t.Fatalf("parseNLAttrs: %v", err)
	}
	gotAttrs := map[uint16][]byte{}
	for _, a := range attrs {
		gotAttrs[a.Type] = a.Value
	}
	if !bytes.Equal(gotAttrs[rtaGateway], gw) {
		t.Errorf("RTA_GATEWAY = %v, want %v", gotAttrs[rtaGateway], gw)
	}
	if len(gotAttrs[rtaOIF]) != 4 || int32(binary.NativeEndian.Uint32(gotAttrs[rtaOIF])) != 5 {
		t.Errorf("RTA_OIF = %v, want 5", gotAttrs[rtaOIF])
	}
}

func TestNLAttribute4ByteAlignment(t *testing.T) {
	b := newNLBuilder(genlIDCtrl, nlmFRequest, 1, genlHdrLen)
	// Add 1-byte, 3-byte, and 5-byte attributes to exercise all padding widths.
	b.addAttr(1, []byte{0xaa})
	b.addAttr(2, []byte{0x01, 0x02, 0x03})
	b.addAttr(3, []byte("hello"))
	raw := b.bytes()

	if len(raw)%4 != 0 {
		t.Fatalf("message length %d not 4-byte aligned", len(raw))
	}
	msgs, err := parseNLMessages(raw)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("parseNLMessages: %v", err)
	}
	attrs, err := parseNLAttrs(msgs[0].Data[genlHdrLen:])
	if err != nil {
		t.Fatalf("parseNLAttrs: %v", err)
	}
	if len(attrs) != 3 {
		t.Fatalf("len(attrs) = %d, want 3", len(attrs))
	}
	if !bytes.Equal(attrs[0].Value, []byte{0xaa}) {
		t.Errorf("attr[0] = %x", attrs[0].Value)
	}
	if !bytes.Equal(attrs[1].Value, []byte{0x01, 0x02, 0x03}) {
		t.Errorf("attr[1] = %x", attrs[1].Value)
	}
	if string(attrs[2].Value) != "hello" {
		t.Errorf("attr[2] = %q", string(attrs[2].Value))
	}
}

func makeNLMsgError(seq uint32, code int32) []byte {
	// nlmsghdr (16) + error (4) + echoed nlmsghdr (16) = 36 bytes
	b := newNLBuilder(nlmsgError, 0, seq, 20)
	binary.NativeEndian.PutUint32(b.body()[0:4], uint32(code))
	return b.bytes()
}

func TestParseNLMsgErrorAndAck(t *testing.T) {
	// Stream with an unrelated seq message followed by our ACK (0).
	stream := append(makeNLMsgError(999, -17), makeNLMsgError(42, 0)...)
	msgs, err := parseNLMessages(stream)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("parseNLMessages: msgs=%d err=%v", len(msgs), err)
	}
	found, errno, err := parseNLAck(msgs, 42)
	if err != nil || !found || errno != 0 {
		t.Fatalf("parseNLAck(42) = (%v, %d, %v), want (true, 0, nil)", found, errno, err)
	}

	// Negative errno (-17 == -EEXIST).
	found, errno, err = parseNLAck(msgs, 999)
	if err != nil || !found || errno != 17 {
		t.Fatalf("parseNLAck(999) = (%v, %d, %v), want (true, 17, nil)", found, errno, err)
	}

	// Non-matching seq.
	found, errno, err = parseNLAck(msgs, 1234)
	if err != nil || found || errno != 0 {
		t.Fatalf("parseNLAck(1234) = (%v, %d, %v), want (false, 0, nil)", found, errno, err)
	}

	// Truncated payload.
	if _, err := parseNLMsgError([]byte{1, 2}); err == nil {
		t.Error("expected error for truncated NLMSG_ERROR payload")
	}
}

func TestParseNLMessagesErrors(t *testing.T) {
	if _, err := parseNLMessages([]byte{1, 2, 3}); err == nil {
		t.Error("expected error on truncated header")
	}
	// nlmsg_len = 0 must not loop infinitely.
	zero := make([]byte, 16)
	if _, err := parseNLMessages(zero); err == nil {
		t.Error("expected error on zero nlmsg_len")
	}
	// nlmsg_len larger than buffer.
	oversized := make([]byte, 16)
	binary.NativeEndian.PutUint32(oversized[0:4], 64)
	if _, err := parseNLMessages(oversized); err == nil {
		t.Error("expected error on oversized nlmsg_len")
	}
}

func TestEncodeARPProbeAndConflictDetection(t *testing.T) {
	selfMAC := net.HardwareAddr{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0x01}
	otherMAC := net.HardwareAddr{0x11, 0x22, 0x33, 0x44, 0x55, 0x66}
	targetIP := net.IPv4(169, 254, 10, 20).To4()
	otherIP := net.IPv4(169, 254, 10, 99).To4()

	probe, err := encodeARPProbe(selfMAC, targetIP)
	if err != nil {
		t.Fatalf("encodeARPProbe: %v", err)
	}
	if len(probe) != arpFrameLen {
		t.Fatalf("probe len = %d, want %d", len(probe), arpFrameLen)
	}
	// Verify Ethernet header.
	if !bytes.Equal(probe[0:6], ethBroadcast[:]) {
		t.Errorf("eth dst = %x", probe[0:6])
	}
	if !bytes.Equal(probe[6:12], selfMAC) {
		t.Errorf("eth src = %x", probe[6:12])
	}
	if et := binary.BigEndian.Uint16(probe[12:14]); et != ethProtoARP {
		t.Errorf("ethertype = 0x%04x, want 0x0806", et)
	}
	// Verify ARP fields.
	arp := probe[14:]
	if binary.BigEndian.Uint16(arp[0:2]) != arpHTypeEthernet ||
		binary.BigEndian.Uint16(arp[2:4]) != ethProtoIPv4 ||
		arp[4] != 6 || arp[5] != 4 ||
		binary.BigEndian.Uint16(arp[6:8]) != arpOpRequest {
		t.Fatalf("unexpected ARP header: %x", arp[:8])
	}
	if !bytes.Equal(arp[8:14], selfMAC) {
		t.Errorf("SHA = %x, want %x", arp[8:14], selfMAC)
	}
	if !bytes.Equal(arp[14:18], ipv4Zero[:]) {
		t.Errorf("SPA = %x, want 0.0.0.0", arp[14:18])
	}
	if !bytes.Equal(arp[18:24], make([]byte, 6)) {
		t.Errorf("THA = %x, want 00:00:00:00:00:00", arp[18:24])
	}
	if !bytes.Equal(arp[24:28], targetIP) {
		t.Errorf("TPA = %x, want %x", arp[24:28], targetIP)
	}

	buildARPFrame := func(ethType uint16, op uint16, sha net.HardwareAddr, spa, tpa net.IP, pad int) []byte {
		f := make([]byte, arpFrameLen+pad)
		copy(f[0:6], selfMAC)
		copy(f[6:12], sha)
		binary.BigEndian.PutUint16(f[12:14], ethType)
		a := f[14:]
		binary.BigEndian.PutUint16(a[0:2], arpHTypeEthernet)
		binary.BigEndian.PutUint16(a[2:4], ethProtoIPv4)
		a[4] = 6
		a[5] = 4
		binary.BigEndian.PutUint16(a[6:8], op)
		copy(a[8:14], sha)
		copy(a[14:18], spa.To4())
		copy(a[18:24], selfMAC)
		copy(a[24:28], tpa.To4())
		return f
	}

	tests := []struct {
		name  string
		frame []byte
		want  bool
	}{
		{
			name:  "matching ARP reply from other host (conflict)",
			frame: buildARPFrame(ethProtoARP, arpOpReply, otherMAC, targetIP, net.IPv4zero, 18),
			want:  true,
		},
		{
			name:  "matching ARP announcement/request from owner (conflict)",
			frame: buildARPFrame(ethProtoARP, arpOpRequest, otherMAC, targetIP, targetIP, 0),
			want:  true,
		},
		{
			name:  "simultaneous RFC 5227 probe from other host (conflict)",
			frame: buildARPFrame(ethProtoARP, arpOpRequest, otherMAC, net.IPv4zero, targetIP, 0),
			want:  true,
		},
		{
			name:  "own MAC probe echo (not conflict)",
			frame: probe,
			want:  false,
		},
		{
			name:  "own MAC reply echo (not conflict)",
			frame: buildARPFrame(ethProtoARP, arpOpReply, selfMAC, targetIP, net.IPv4zero, 0),
			want:  false,
		},
		{
			name:  "other IP reply (not conflict)",
			frame: buildARPFrame(ethProtoARP, arpOpReply, otherMAC, otherIP, net.IPv4zero, 0),
			want:  false,
		},
		{
			name:  "other host querying targetIP with non-zero SPA (not conflict)",
			frame: buildARPFrame(ethProtoARP, arpOpRequest, otherMAC, otherIP, targetIP, 0),
			want:  false,
		},
		{
			name:  "non-ARP IPv4 frame ignored",
			frame: buildARPFrame(ethProtoIPv4, arpOpReply, otherMAC, targetIP, net.IPv4zero, 0),
			want:  false,
		},
		{
			name:  "truncated frame ignored",
			frame: probe[:20],
			want:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isARPConflict(tc.frame, selfMAC, targetIP); got != tc.want {
				t.Errorf("isARPConflict() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGenlFamilyIDEncodeAndParse(t *testing.T) {
	req := encodeGenlGetFamily(nl80211FamilyName, 7)
	msgs, err := parseNLMessages(req)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("parseNLMessages(req): %v", err)
	}
	if msgs[0].Header.Type != genlIDCtrl || msgs[0].Header.Flags != nlmFRequest || msgs[0].Header.Seq != 7 {
		t.Fatalf("unexpected req header: %+v", msgs[0].Header)
	}
	if msgs[0].Data[0] != ctrlCmdGetFamily || msgs[0].Data[1] != 1 {
		t.Fatalf("unexpected genlmsghdr: %v", msgs[0].Data[:4])
	}
	attrs, err := parseNLAttrs(msgs[0].Data[genlHdrLen:])
	if err != nil || len(attrs) != 1 {
		t.Fatalf("attrs: %v err=%v", attrs, err)
	}
	if attrs[0].Type != ctrlAttrFamilyNam || string(attrs[0].Value) != "nl80211\x00" {
		t.Fatalf("unexpected attr: type=%d val=%q", attrs[0].Type, string(attrs[0].Value))
	}

	// Build a synthetic CTRL_CMD_NEWFAMILY response with family ID 31.
	resp := newNLBuilder(genlIDCtrl, 0, 7, genlHdrLen)
	resp.body()[0] = ctrlCmdNewFamily
	resp.body()[1] = 2
	resp.addAttrString(ctrlAttrFamilyNam, nl80211FamilyName)
	var idRaw [2]byte
	binary.NativeEndian.PutUint16(idRaw[:], 31)
	resp.addAttr(ctrlAttrFamilyID, idRaw[:])

	respMsgs, err := parseNLMessages(resp.bytes())
	if err != nil {
		t.Fatalf("parseNLMessages(resp): %v", err)
	}
	id, found, errno, err := parseGenlFamilyID(respMsgs, 7)
	if err != nil || !found || errno != 0 || id != 31 {
		t.Fatalf("parseGenlFamilyID = (id=%d found=%v errno=%d err=%v), want (31, true, 0, nil)", id, found, errno, err)
	}

	// Error response (e.g. ENOENT = 2).
	errMsgs, _ := parseNLMessages(makeNLMsgError(7, -2))
	_, found, errno, err = parseGenlFamilyID(errMsgs, 7)
	if err != nil || !found || errno != 2 {
		t.Fatalf("parseGenlFamilyID error = (found=%v errno=%d err=%v), want (true, 2, nil)", found, errno, err)
	}
}

func TestNL80211StationDumpEncodeAndCount(t *testing.T) {
	const famID uint16 = 31
	const seq uint32 = 15

	req := encodeNL80211GetStationDump(famID, 4, seq)
	msgs, err := parseNLMessages(req)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("parseNLMessages(req): %v", err)
	}
	if msgs[0].Header.Type != famID || msgs[0].Header.Flags != (nlmFRequest|nlmFDump) || msgs[0].Header.Seq != seq {
		t.Fatalf("unexpected header: %+v", msgs[0].Header)
	}
	if msgs[0].Data[0] != nl80211CmdGetStation {
		t.Fatalf("genl cmd = %d, want %d", msgs[0].Data[0], nl80211CmdGetStation)
	}
	attrs, err := parseNLAttrs(msgs[0].Data[genlHdrLen:])
	if err != nil || len(attrs) != 1 || attrs[0].Type != nl80211AttrIfindex {
		t.Fatalf("unexpected attrs: %+v err=%v", attrs, err)
	}
	if idx := binary.NativeEndian.Uint32(attrs[0].Value); idx != 4 {
		t.Fatalf("ifindex = %d, want 4", idx)
	}

	// Build a multipart dump response: 3 NEW_STATION messages + 1 unrelated cmd + NLMSG_DONE.
	var stream []byte
	for i := 0; i < 3; i++ {
		st := newNLBuilder(famID, nlmFMulti, seq, genlHdrLen)
		st.body()[0] = nl80211CmdNewStation
		st.addAttrUint32(nl80211AttrIfindex, 4)
		stream = append(stream, st.bytes()...)
	}
	// Non-station message in same family (should not be counted).
	other := newNLBuilder(famID, nlmFMulti, seq, genlHdrLen)
	other.body()[0] = nl80211CmdGetStation
	stream = append(stream, other.bytes()...)

	// Message with different seq (should be ignored).
	wrongSeq := newNLBuilder(famID, nlmFMulti, seq+1, genlHdrLen)
	wrongSeq.body()[0] = nl80211CmdNewStation
	stream = append(stream, wrongSeq.bytes()...)

	// NLMSG_DONE with 0 error payload.
	doneMsg := newNLBuilder(nlmsgDone, nlmFMulti, seq, 4)
	stream = append(stream, doneMsg.bytes()...)

	parsed, err := parseNLMessages(stream)
	if err != nil {
		t.Fatalf("parseNLMessages: %v", err)
	}
	cnt, done, errno, err := parseNL80211StationDump(parsed, famID, seq)
	if err != nil || !done || errno != 0 || cnt != 3 {
		t.Fatalf("parseNL80211StationDump = (cnt=%d done=%v errno=%d err=%v), want (3, true, 0, nil)",
			cnt, done, errno, err)
	}

	// Empty dump (0 stations): just NLMSG_DONE.
	emptyDone, _ := parseNLMessages(doneMsg.bytes())
	cnt, done, errno, err = parseNL80211StationDump(emptyDone, famID, seq)
	if err != nil || !done || errno != 0 || cnt != 0 {
		t.Fatalf("empty dump = (cnt=%d done=%v errno=%d err=%v), want (0, true, 0, nil)",
			cnt, done, errno, err)
	}

	// Error response (e.g. ENODEV = 19).
	errMsgs, _ := parseNLMessages(makeNLMsgError(seq, -19))
	_, done, errno, err = parseNL80211StationDump(errMsgs, famID, seq)
	if err != nil || !done || errno != 19 {
		t.Fatalf("error dump = (done=%v errno=%d err=%v), want (true, 19, nil)", done, errno, err)
	}
}
