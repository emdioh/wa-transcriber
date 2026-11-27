package dhcpc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestPacketRoundTrip(t *testing.T) {
	mac := net.HardwareAddr{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0x01}

	cases := []struct {
		name string
		pkt  *Packet
	}{
		{
			name: "DISCOVER",
			pkt:  NewDiscover(0x12345678, mac, "wa-device"),
		},
		{
			name: "OFFER",
			pkt: &Packet{
				Op:            OpReply,
				HType:         HTypeEthernet,
				HLen:          HLenEthernet,
				XID:           0x12345678,
				Flags:         FlagBroadcast,
				YIAddr:        net.IPv4(192, 168, 1, 50),
				SIAddr:        net.IPv4(192, 168, 1, 1),
				CHAddr:        mac,
				MessageType:   MsgOffer,
				SubnetMask:    net.CIDRMask(24, 32),
				Router:        net.IPv4(192, 168, 1, 1),
				DNS:           []net.IP{net.IPv4(8, 8, 8, 8), net.IPv4(1, 1, 1, 1)},
				LeaseTime:     3600 * time.Second,
				RenewalTime:   1800 * time.Second,
				RebindingTime: 3150 * time.Second,
				ServerID:      net.IPv4(192, 168, 1, 1),
			},
		},
		{
			name: "REQUEST",
			pkt:  NewRequest(0x12345678, mac, "wa-device", net.IPv4(192, 168, 1, 50), net.IPv4(192, 168, 1, 1)),
		},
		{
			name: "ACK",
			pkt: &Packet{
				Op:            OpReply,
				HType:         HTypeEthernet,
				HLen:          HLenEthernet,
				XID:           0xdeadbeef,
				Flags:         FlagBroadcast,
				CIAddr:        net.IPv4(10, 0, 0, 20),
				YIAddr:        net.IPv4(10, 0, 0, 20),
				CHAddr:        mac,
				MessageType:   MsgAck,
				SubnetMask:    net.CIDRMask(16, 32),
				Router:        net.IPv4(10, 0, 0, 1),
				DNS:           []net.IP{net.IPv4(10, 0, 0, 53)},
				LeaseTime:     7200 * time.Second,
				RenewalTime:   3600 * time.Second,
				RebindingTime: 6300 * time.Second,
				ServerID:      net.IPv4(10, 0, 0, 1),
			},
		},
		{
			name: "NAK",
			pkt: &Packet{
				Op:          OpReply,
				HType:       HTypeEthernet,
				HLen:        HLenEthernet,
				XID:         0xfeedface,
				Flags:       FlagBroadcast,
				CHAddr:      mac,
				MessageType: MsgNak,
				ServerID:    net.IPv4(192, 168, 1, 1),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := tc.pkt.Marshal()
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if len(raw) < minWireSize {
				t.Fatalf("wire length = %d, want >= %d", len(raw), minWireSize)
			}
			got, err := ParsePacket(raw)
			if err != nil {
				t.Fatalf("ParsePacket: %v", err)
			}

			if got.Op != tc.pkt.Op {
				t.Errorf("Op = %d, want %d", got.Op, tc.pkt.Op)
			}
			if got.HType != tc.pkt.HType || got.HLen != tc.pkt.HLen {
				t.Errorf("HType/HLen = %d/%d, want %d/%d", got.HType, got.HLen, tc.pkt.HType, tc.pkt.HLen)
			}
			if got.XID != tc.pkt.XID {
				t.Errorf("XID = %#x, want %#x", got.XID, tc.pkt.XID)
			}
			if got.Flags != tc.pkt.Flags {
				t.Errorf("Flags = %#x, want %#x", got.Flags, tc.pkt.Flags)
			}
			if !bytes.Equal(got.CHAddr, tc.pkt.CHAddr) {
				t.Errorf("CHAddr = %v, want %v", got.CHAddr, tc.pkt.CHAddr)
			}
			if got.MessageType != tc.pkt.MessageType {
				t.Errorf("MessageType = %v, want %v", got.MessageType, tc.pkt.MessageType)
			}
			if tc.pkt.YIAddr != nil && !got.YIAddr.Equal(tc.pkt.YIAddr) {
				t.Errorf("YIAddr = %v, want %v", got.YIAddr, tc.pkt.YIAddr)
			}
			if tc.pkt.CIAddr != nil && !got.CIAddr.Equal(tc.pkt.CIAddr) {
				t.Errorf("CIAddr = %v, want %v", got.CIAddr, tc.pkt.CIAddr)
			}
			if tc.pkt.RequestedIP != nil && !got.RequestedIP.Equal(tc.pkt.RequestedIP) {
				t.Errorf("RequestedIP = %v, want %v", got.RequestedIP, tc.pkt.RequestedIP)
			}
			if tc.pkt.ServerID != nil && !got.ServerID.Equal(tc.pkt.ServerID) {
				t.Errorf("ServerID = %v, want %v", got.ServerID, tc.pkt.ServerID)
			}
			if !bytes.Equal(got.SubnetMask, tc.pkt.SubnetMask) {
				t.Errorf("SubnetMask = %v, want %v", got.SubnetMask, tc.pkt.SubnetMask)
			}
			if tc.pkt.Router != nil && !got.Router.Equal(tc.pkt.Router) {
				t.Errorf("Router = %v, want %v", got.Router, tc.pkt.Router)
			}
			if len(got.DNS) != len(tc.pkt.DNS) {
				t.Fatalf("DNS len = %d, want %d", len(got.DNS), len(tc.pkt.DNS))
			}
			for i := range tc.pkt.DNS {
				if !got.DNS[i].Equal(tc.pkt.DNS[i]) {
					t.Errorf("DNS[%d] = %v, want %v", i, got.DNS[i], tc.pkt.DNS[i])
				}
			}
			if got.Hostname != tc.pkt.Hostname {
				t.Errorf("Hostname = %q, want %q", got.Hostname, tc.pkt.Hostname)
			}
			if !bytes.Equal(got.ClientID, tc.pkt.ClientID) {
				t.Errorf("ClientID = %x, want %x", got.ClientID, tc.pkt.ClientID)
			}
			if !bytes.Equal(got.ParamReqList, tc.pkt.ParamReqList) {
				t.Errorf("ParamReqList = %v, want %v", got.ParamReqList, tc.pkt.ParamReqList)
			}
			if got.LeaseTime != tc.pkt.LeaseTime {
				t.Errorf("LeaseTime = %v, want %v", got.LeaseTime, tc.pkt.LeaseTime)
			}
			if got.RenewalTime != tc.pkt.RenewalTime {
				t.Errorf("RenewalTime = %v, want %v", got.RenewalTime, tc.pkt.RenewalTime)
			}
			if got.RebindingTime != tc.pkt.RebindingTime {
				t.Errorf("RebindingTime = %v, want %v", got.RebindingTime, tc.pkt.RebindingTime)
			}
		})
	}
}

func TestParsePacketErrorsAndEdgeCases(t *testing.T) {
	t.Run("short packet", func(t *testing.T) {
		_, err := ParsePacket(make([]byte, minPacketSize-1))
		if !errors.Is(err, ErrShortPacket) {
			t.Fatalf("err = %v, want ErrShortPacket", err)
		}
	})

	t.Run("bad magic cookie", func(t *testing.T) {
		b := make([]byte, minWireSize)
		b[236], b[237], b[238], b[239] = 1, 2, 3, 4
		_, err := ParsePacket(b)
		if !errors.Is(err, ErrBadMagicCookie) {
			t.Fatalf("err = %v, want ErrBadMagicCookie", err)
		}
	})

	t.Run("truncated option missing length", func(t *testing.T) {
		b := make([]byte, minPacketSize+1)
		copy(b[headerSize:minPacketSize], magicCookie[:])
		b[minPacketSize] = OptMessageType // missing length byte
		_, err := ParsePacket(b)
		if !errors.Is(err, ErrMalformedOption) {
			t.Fatalf("err = %v, want ErrMalformedOption", err)
		}
	})

	t.Run("truncated option payload", func(t *testing.T) {
		b := make([]byte, minPacketSize+4)
		copy(b[headerSize:minPacketSize], magicCookie[:])
		b[minPacketSize] = OptLeaseTime
		b[minPacketSize+1] = 4 // claims 4 bytes, only 2 follow
		b[minPacketSize+2] = 0
		b[minPacketSize+3] = 60
		_, err := ParsePacket(b)
		if !errors.Is(err, ErrMalformedOption) {
			t.Fatalf("err = %v, want ErrMalformedOption", err)
		}
	})

	t.Run("wrong fixed option length", func(t *testing.T) {
		b := make([]byte, minPacketSize+5)
		copy(b[headerSize:minPacketSize], magicCookie[:])
		b[minPacketSize] = OptSubnetMask
		b[minPacketSize+1] = 2 // must be 4
		b[minPacketSize+2] = 255
		b[minPacketSize+3] = 255
		b[minPacketSize+4] = OptEnd
		_, err := ParsePacket(b)
		if !errors.Is(err, ErrMalformedOption) {
			t.Fatalf("err = %v, want ErrMalformedOption", err)
		}
	})

	t.Run("missing options parses with zero message type", func(t *testing.T) {
		b := make([]byte, minPacketSize+1)
		copy(b[headerSize:minPacketSize], magicCookie[:])
		b[minPacketSize] = OptEnd
		pkt, err := ParsePacket(b)
		if err != nil {
			t.Fatalf("ParsePacket: %v", err)
		}
		if pkt.MessageType != 0 {
			t.Errorf("MessageType = %v, want 0", pkt.MessageType)
		}
	})

	t.Run("pad and unknown options ignored", func(t *testing.T) {
		b := make([]byte, minPacketSize)
		copy(b[headerSize:minPacketSize], magicCookie[:])
		b = append(b, OptPad, OptPad, 200, 3, 1, 2, 3, OptMessageType, 1, byte(MsgAck), OptEnd)
		pkt, err := ParsePacket(b)
		if err != nil {
			t.Fatalf("ParsePacket: %v", err)
		}
		if pkt.MessageType != MsgAck {
			t.Errorf("MessageType = %v, want ACK", pkt.MessageType)
		}
	})
}

func TestLeaseFromPacketDefaultsAndClamping(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

	t.Run("defaults T1 and T2 when omitted", func(t *testing.T) {
		ack := &Packet{
			Op:          OpReply,
			YIAddr:      net.IPv4(192, 168, 10, 42),
			MessageType: MsgAck,
			SubnetMask:  net.CIDRMask(24, 32),
			Router:      net.IPv4(192, 168, 10, 1),
			DNS:         []net.IP{net.IPv4(9, 9, 9, 9)},
			ServerID:    net.IPv4(192, 168, 10, 1),
			LeaseTime:   800 * time.Second,
		}
		lease, err := LeaseFromPacket(ack, now)
		if err != nil {
			t.Fatalf("LeaseFromPacket: %v", err)
		}
		if lease.Duration != 800*time.Second {
			t.Errorf("Duration = %v, want 800s", lease.Duration)
		}
		if lease.T1 != 400*time.Second {
			t.Errorf("T1 = %v, want 400s", lease.T1)
		}
		if lease.T2 != 700*time.Second {
			t.Errorf("T2 = %v, want 700s", lease.T2)
		}
		if lease.PrefixLen != 24 {
			t.Errorf("PrefixLen = %d, want 24", lease.PrefixLen)
		}
	})

	t.Run("clamps short and missing lease to MinLeaseDuration", func(t *testing.T) {
		for _, rawDur := range []time.Duration{0, 5 * time.Second} {
			ack := &Packet{
				Op:          OpReply,
				YIAddr:      net.IPv4(192, 168, 10, 42),
				MessageType: MsgAck,
				LeaseTime:   rawDur,
			}
			lease, err := LeaseFromPacket(ack, now)
			if err != nil {
				t.Fatalf("LeaseFromPacket(%v): %v", rawDur, err)
			}
			if lease.Duration != MinLeaseDuration {
				t.Errorf("raw %v: Duration = %v, want %v", rawDur, lease.Duration, MinLeaseDuration)
			}
			if lease.T1 != MinLeaseDuration/2 {
				t.Errorf("raw %v: T1 = %v, want %v", rawDur, lease.T1, MinLeaseDuration/2)
			}
			if lease.T2 != MinLeaseDuration*7/8 {
				t.Errorf("raw %v: T2 = %v, want %v", rawDur, lease.T2, MinLeaseDuration*7/8)
			}
			if lease.PrefixLen != 24 {
				t.Errorf("default mask PrefixLen = %d, want 24", lease.PrefixLen)
			}
		}
	})

	t.Run("rejects non-contiguous subnet mask", func(t *testing.T) {
		ack := &Packet{
			Op:          OpReply,
			YIAddr:      net.IPv4(192, 168, 10, 42),
			MessageType: MsgAck,
			SubnetMask:  net.IPMask{255, 0, 255, 0},
			LeaseTime:   60 * time.Second,
		}
		if _, err := LeaseFromPacket(ack, now); err == nil {
			t.Fatal("expected error for non-contiguous subnet mask")
		}
	})
}

// fakeDHCPServer runs a real UDP socket on 127.0.0.1:0 so tests exercise the
// actual UDP transport and packet encoding/decoding without root privileges.
type fakeDHCPServer struct {
	conn *net.UDPConn
	addr *net.UDPAddr

	mu       sync.Mutex
	received []*Packet
	handler  func(srv *fakeDHCPServer, clientAddr *net.UDPAddr, req *Packet)
	done     chan struct{}
}

func startFakeDHCPServer(t *testing.T, handler func(srv *fakeDHCPServer, clientAddr *net.UDPAddr, req *Packet)) *fakeDHCPServer {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen fake server: %v", err)
	}
	srv := &fakeDHCPServer{
		conn:    conn,
		addr:    conn.LocalAddr().(*net.UDPAddr),
		handler: handler,
		done:    make(chan struct{}),
	}
	go func() {
		defer close(srv.done)
		buf := make([]byte, 2048)
		for {
			n, raddr, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			pkt, err := ParsePacket(buf[:n])
			if err != nil {
				continue
			}
			srv.mu.Lock()
			srv.received = append(srv.received, pkt)
			srv.mu.Unlock()
			if srv.handler != nil {
				srv.handler(srv, raddr, pkt)
			}
		}
	}()
	t.Cleanup(func() {
		_ = conn.Close()
		<-srv.done
	})
	return srv
}

func (s *fakeDHCPServer) send(t *testing.T, dst *net.UDPAddr, pkt *Packet) {
	t.Helper()
	raw, err := pkt.Marshal()
	if err != nil {
		t.Errorf("server marshal: %v", err)
		return
	}
	if _, err := s.conn.WriteToUDP(raw, dst); err != nil {
		t.Errorf("server write: %v", err)
	}
}

func (s *fakeDHCPServer) packets() []*Packet {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Packet, len(s.received))
	copy(out, s.received)
	return out
}

// loopbackDial returns an Options.Dial that binds an ephemeral 127.0.0.1:0 UDP
// socket and routes all sends (including unicast renewal sends addressed to a
// simulated LAN server IP) to srv.addr.
func loopbackDial(srv *fakeDHCPServer) func(context.Context) (Transport, error) {
	return func(ctx context.Context) (Transport, error) {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
		if err != nil {
			return nil, err
		}
		return &loopbackTransport{conn: conn, serverAddr: srv.addr}, nil
	}
}

type loopbackTransport struct {
	conn       *net.UDPConn
	serverAddr *net.UDPAddr
}

func (t *loopbackTransport) Send(pkt []byte, _ *net.UDPAddr) error {
	_, err := t.conn.WriteToUDP(pkt, t.serverAddr)
	return err
}

func (t *loopbackTransport) Recv(deadline time.Time) ([]byte, error) {
	if err := t.conn.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	buf := make([]byte, 2048)
	n, _, err := t.conn.ReadFromUDP(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func (t *loopbackTransport) Close() error {
	return t.conn.Close()
}

func TestObtainHappyPath(t *testing.T) {
	mac := net.HardwareAddr{0x02, 0x11, 0x22, 0x33, 0x44, 0x55}
	wantIP := net.IPv4(192, 168, 50, 101)
	wantServer := net.IPv4(192, 168, 50, 1)
	wantRouter := net.IPv4(192, 168, 50, 1)
	wantDNS := []net.IP{net.IPv4(8, 8, 8, 8), net.IPv4(1, 1, 1, 1)}

	srv := startFakeDHCPServer(t, func(s *fakeDHCPServer, client *net.UDPAddr, req *Packet) {
		switch req.MessageType {
		case MsgDiscover:
			s.send(t, client, &Packet{
				Op:          OpReply,
				HType:       HTypeEthernet,
				HLen:        HLenEthernet,
				XID:         req.XID,
				Flags:       FlagBroadcast,
				YIAddr:      wantIP,
				CHAddr:      req.CHAddr,
				MessageType: MsgOffer,
				ServerID:    wantServer,
				SubnetMask:  net.CIDRMask(24, 32),
				Router:      wantRouter,
				DNS:         wantDNS,
				LeaseTime:   1200 * time.Second,
			})
		case MsgRequest:
			if !req.RequestedIP.Equal(wantIP) || !req.ServerID.Equal(wantServer) {
				t.Errorf("unexpected REQUEST fields: reqIP=%v serverID=%v", req.RequestedIP, req.ServerID)
			}
			s.send(t, client, &Packet{
				Op:            OpReply,
				HType:         HTypeEthernet,
				HLen:          HLenEthernet,
				XID:           req.XID,
				Flags:         FlagBroadcast,
				YIAddr:        wantIP,
				CHAddr:        req.CHAddr,
				MessageType:   MsgAck,
				ServerID:      wantServer,
				SubnetMask:    net.CIDRMask(24, 32),
				Router:        wantRouter,
				DNS:           wantDNS,
				LeaseTime:     1200 * time.Second,
				RenewalTime:   500 * time.Second,
				RebindingTime: 1000 * time.Second,
			})
		}
	})

	var applied Lease
	applyCalled := false
	resolvPath := filepath.Join(t.TempDir(), "resolv.conf")

	lease, err := Obtain(context.Background(), Options{
		IfName:        "eth0",
		MAC:           mac,
		Hostname:      "wa-bbb",
		Timeout:       2 * time.Second,
		RetryInterval: 100 * time.Millisecond,
		ResolvConf:    resolvPath,
		Log:           testLogger(),
		Dial:          loopbackDial(srv),
		Apply: func(l Lease) error {
			applyCalled = true
			applied = l
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Obtain: %v", err)
	}
	if !applyCalled {
		t.Fatal("Apply callback was not called")
	}
	if !lease.IP.Equal(wantIP) || !applied.IP.Equal(wantIP) {
		t.Errorf("IP = %v (applied %v), want %v", lease.IP, applied.IP, wantIP)
	}
	if lease.PrefixLen != 24 || !bytes.Equal(lease.Mask, net.CIDRMask(24, 32)) {
		t.Errorf("Mask/PrefixLen = %v/%d, want /24", lease.Mask, lease.PrefixLen)
	}
	if !lease.Router.Equal(wantRouter) {
		t.Errorf("Router = %v, want %v", lease.Router, wantRouter)
	}
	if !lease.ServerID.Equal(wantServer) {
		t.Errorf("ServerID = %v, want %v", lease.ServerID, wantServer)
	}
	if len(lease.DNS) != 2 || !lease.DNS[0].Equal(wantDNS[0]) || !lease.DNS[1].Equal(wantDNS[1]) {
		t.Errorf("DNS = %v, want %v", lease.DNS, wantDNS)
	}
	if lease.Duration != 1200*time.Second || lease.T1 != 500*time.Second || lease.T2 != 1000*time.Second {
		t.Errorf("timers = (%v, %v, %v), want (1200s, 500s, 1000s)", lease.Duration, lease.T1, lease.T2)
	}

	gotResolv, err := os.ReadFile(resolvPath)
	if err != nil {
		t.Fatalf("read resolv.conf: %v", err)
	}
	wantResolv := "# written by wa-transcriber\nnameserver 8.8.8.8\nnameserver 1.1.1.1\n"
	if string(gotResolv) != wantResolv {
		t.Errorf("resolv.conf = %q, want %q", gotResolv, wantResolv)
	}

	pkts := srv.packets()
	if len(pkts) != 2 || pkts[0].MessageType != MsgDiscover || pkts[1].MessageType != MsgRequest {
		t.Fatalf("expected [DISCOVER, REQUEST], got %d packets", len(pkts))
	}
	if pkts[0].Flags != FlagBroadcast {
		t.Errorf("DISCOVER flags = %#x, want %#x", pkts[0].Flags, FlagBroadcast)
	}
	if pkts[0].Hostname != "wa-bbb" {
		t.Errorf("DISCOVER hostname = %q, want wa-bbb", pkts[0].Hostname)
	}
}

func TestObtainIgnoresMismatchedXIDAndMAC(t *testing.T) {
	mac := net.HardwareAddr{0x02, 0xaa, 0xbb, 0xcc, 0xdd, 0x01}
	otherMAC := net.HardwareAddr{0x02, 0x99, 0x99, 0x99, 0x99, 0x99}
	realIP := net.IPv4(192, 168, 2, 20)
	decoyIP := net.IPv4(192, 168, 2, 99)
	serverIP := net.IPv4(192, 168, 2, 1)

	srv := startFakeDHCPServer(t, func(s *fakeDHCPServer, client *net.UDPAddr, req *Packet) {
		switch req.MessageType {
		case MsgDiscover:
			// Wrong XID decoy
			s.send(t, client, &Packet{
				Op: OpReply, HType: HTypeEthernet, HLen: HLenEthernet,
				XID: req.XID ^ 0x1, YIAddr: decoyIP, CHAddr: req.CHAddr,
				MessageType: MsgOffer, ServerID: serverIP,
			})
			// Wrong MAC decoy
			s.send(t, client, &Packet{
				Op: OpReply, HType: HTypeEthernet, HLen: HLenEthernet,
				XID: req.XID, YIAddr: decoyIP, CHAddr: otherMAC,
				MessageType: MsgOffer, ServerID: serverIP,
			})
			// Matching OFFER
			s.send(t, client, &Packet{
				Op: OpReply, HType: HTypeEthernet, HLen: HLenEthernet,
				XID: req.XID, YIAddr: realIP, CHAddr: req.CHAddr,
				MessageType: MsgOffer, ServerID: serverIP,
			})
		case MsgRequest:
			// Wrong XID NAK decoy (must be ignored!)
			s.send(t, client, &Packet{
				Op: OpReply, HType: HTypeEthernet, HLen: HLenEthernet,
				XID: req.XID ^ 0xff, CHAddr: req.CHAddr,
				MessageType: MsgNak, ServerID: serverIP,
			})
			// Wrong MAC ACK decoy
			s.send(t, client, &Packet{
				Op: OpReply, HType: HTypeEthernet, HLen: HLenEthernet,
				XID: req.XID, YIAddr: decoyIP, CHAddr: otherMAC,
				MessageType: MsgAck, ServerID: serverIP, LeaseTime: 600 * time.Second,
			})
			// Matching ACK
			s.send(t, client, &Packet{
				Op: OpReply, HType: HTypeEthernet, HLen: HLenEthernet,
				XID: req.XID, YIAddr: realIP, CHAddr: req.CHAddr,
				MessageType: MsgAck, ServerID: serverIP,
				SubnetMask: net.CIDRMask(24, 32), LeaseTime: 600 * time.Second,
			})
		}
	})

	lease, err := Obtain(context.Background(), Options{
		IfName:        "eth0",
		MAC:           mac,
		Timeout:       2 * time.Second,
		RetryInterval: 100 * time.Millisecond,
		Log:           testLogger(),
		Dial:          loopbackDial(srv),
	})
	if err != nil {
		t.Fatalf("Obtain: %v", err)
	}
	if !lease.IP.Equal(realIP) {
		t.Fatalf("IP = %v, want %v", lease.IP, realIP)
	}
}

func TestObtainRetriesAfterDroppedDiscover(t *testing.T) {
	mac := net.HardwareAddr{0x02, 0x01, 0x02, 0x03, 0x04, 0x05}
	wantIP := net.IPv4(10, 20, 30, 40)
	serverIP := net.IPv4(10, 20, 30, 1)

	var mu sync.Mutex
	discovers := 0

	srv := startFakeDHCPServer(t, func(s *fakeDHCPServer, client *net.UDPAddr, req *Packet) {
		switch req.MessageType {
		case MsgDiscover:
			mu.Lock()
			discovers++
			n := discovers
			mu.Unlock()
			if n == 1 {
				return // drop first DISCOVER
			}
			s.send(t, client, &Packet{
				Op: OpReply, HType: HTypeEthernet, HLen: HLenEthernet,
				XID: req.XID, YIAddr: wantIP, CHAddr: req.CHAddr,
				MessageType: MsgOffer, ServerID: serverIP,
			})
		case MsgRequest:
			s.send(t, client, &Packet{
				Op: OpReply, HType: HTypeEthernet, HLen: HLenEthernet,
				XID: req.XID, YIAddr: wantIP, CHAddr: req.CHAddr,
				MessageType: MsgAck, ServerID: serverIP,
				SubnetMask: net.CIDRMask(24, 32), LeaseTime: 300 * time.Second,
			})
		}
	})

	lease, err := Obtain(context.Background(), Options{
		IfName:        "eth0",
		MAC:           mac,
		Timeout:       2 * time.Second,
		RetryInterval: 50 * time.Millisecond,
		Log:           testLogger(),
		Dial:          loopbackDial(srv),
	})
	if err != nil {
		t.Fatalf("Obtain: %v", err)
	}
	if !lease.IP.Equal(wantIP) {
		t.Fatalf("IP = %v, want %v", lease.IP, wantIP)
	}
	mu.Lock()
	gotDiscovers := discovers
	mu.Unlock()
	if gotDiscovers < 2 {
		t.Errorf("discovers = %d, want >= 2", gotDiscovers)
	}
}

func TestObtainHandlesNakByRediscovering(t *testing.T) {
	mac := net.HardwareAddr{0x02, 0xde, 0xad, 0xbe, 0xef, 0x01}
	firstIP := net.IPv4(192, 168, 7, 10)
	secondIP := net.IPv4(192, 168, 7, 11)
	serverIP := net.IPv4(192, 168, 7, 1)

	var mu sync.Mutex
	naksSent := 0
	var firstXID uint32
	var secondXID uint32

	srv := startFakeDHCPServer(t, func(s *fakeDHCPServer, client *net.UDPAddr, req *Packet) {
		mu.Lock()
		defer mu.Unlock()
		switch req.MessageType {
		case MsgDiscover:
			ip := firstIP
			if naksSent > 0 {
				ip = secondIP
				secondXID = req.XID
			} else {
				firstXID = req.XID
			}
			s.send(t, client, &Packet{
				Op: OpReply, HType: HTypeEthernet, HLen: HLenEthernet,
				XID: req.XID, YIAddr: ip, CHAddr: req.CHAddr,
				MessageType: MsgOffer, ServerID: serverIP,
			})
		case MsgRequest:
			if naksSent == 0 {
				naksSent++
				s.send(t, client, &Packet{
					Op: OpReply, HType: HTypeEthernet, HLen: HLenEthernet,
					XID: req.XID, CHAddr: req.CHAddr,
					MessageType: MsgNak, ServerID: serverIP,
				})
				return
			}
			s.send(t, client, &Packet{
				Op: OpReply, HType: HTypeEthernet, HLen: HLenEthernet,
				XID: req.XID, YIAddr: secondIP, CHAddr: req.CHAddr,
				MessageType: MsgAck, ServerID: serverIP,
				SubnetMask: net.CIDRMask(24, 32), LeaseTime: 120 * time.Second,
			})
		}
	})

	lease, err := Obtain(context.Background(), Options{
		IfName:        "eth0",
		MAC:           mac,
		Timeout:       2 * time.Second,
		RetryInterval: 50 * time.Millisecond,
		Log:           testLogger(),
		Dial:          loopbackDial(srv),
	})
	if err != nil {
		t.Fatalf("Obtain: %v", err)
	}
	if !lease.IP.Equal(secondIP) {
		t.Fatalf("IP = %v, want %v", lease.IP, secondIP)
	}
	mu.Lock()
	defer mu.Unlock()
	if naksSent != 1 {
		t.Errorf("naksSent = %d, want 1", naksSent)
	}
	if firstXID == secondXID {
		t.Errorf("expected fresh XID after NAK, both were %#x", firstXID)
	}
}

func TestObtainRespectsTimeout(t *testing.T) {
	mac := net.HardwareAddr{0x02, 0x10, 0x20, 0x30, 0x40, 0x50}
	// Silent server: never replies.
	srv := startFakeDHCPServer(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := Obtain(ctx, Options{
		IfName:        "eth0",
		MAC:           mac,
		Timeout:       5 * time.Second,
		RetryInterval: 40 * time.Millisecond,
		Log:           testLogger(),
		Dial:          loopbackDial(srv),
	})
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > time.Second {
		t.Errorf("Obtain took %v, expected prompt return near 150ms", elapsed)
	}
}

func TestMaintainRenewsAtT1AndExitsWithoutRelease(t *testing.T) {
	mac := net.HardwareAddr{0x02, 0xca, 0xfe, 0xba, 0xbe, 0x01}
	ip := net.IPv4(192, 168, 99, 10)
	serverIP := net.IPv4(192, 168, 99, 1)
	renewedDNS := []net.IP{net.IPv4(1, 0, 0, 1)}

	var mu sync.Mutex
	var sawRenewReq *Packet

	srv := startFakeDHCPServer(t, func(s *fakeDHCPServer, client *net.UDPAddr, req *Packet) {
		if req.MessageType != MsgRequest {
			return
		}
		mu.Lock()
		sawRenewReq = req
		mu.Unlock()
		s.send(t, client, &Packet{
			Op:          OpReply,
			HType:       HTypeEthernet,
			HLen:        HLenEthernet,
			XID:         req.XID,
			CIAddr:      req.CIAddr,
			YIAddr:      ip,
			CHAddr:      req.CHAddr,
			MessageType: MsgAck,
			ServerID:    serverIP,
			SubnetMask:  net.CIDRMask(24, 32),
			Router:      serverIP,
			DNS:         renewedDNS,
			LeaseTime:   600 * time.Second,
		})
	})

	appliedCh := make(chan Lease, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	initial := Lease{
		IP:        ip,
		Mask:      net.CIDRMask(24, 32),
		PrefixLen: 24,
		Router:    serverIP,
		ServerID:  serverIP,
		Duration:  250 * time.Millisecond,
		T1:        100 * time.Millisecond,
		T2:        200 * time.Millisecond,
		Obtained:  time.Now(),
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		Maintain(ctx, Options{
			IfName:        "eth0",
			MAC:           mac,
			Timeout:       500 * time.Millisecond,
			RetryInterval: 30 * time.Millisecond,
			Log:           testLogger(),
			Dial:          loopbackDial(srv),
			Apply: func(l Lease) error {
				appliedCh <- l
				return nil
			},
		}, initial)
	}()

	select {
	case renewed := <-appliedCh:
		if !renewed.IP.Equal(ip) {
			t.Errorf("renewed IP = %v, want %v", renewed.IP, ip)
		}
		if len(renewed.DNS) != 1 || !renewed.DNS[0].Equal(renewedDNS[0]) {
			t.Errorf("renewed DNS = %v, want %v", renewed.DNS, renewedDNS)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for T1 renewal")
	}

	// Cancel and ensure Maintain exits cleanly without sending RELEASE (D16).
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Maintain did not exit after cancel")
	}

	mu.Lock()
	req := sawRenewReq
	mu.Unlock()
	if req == nil {
		t.Fatal("server never received renewal REQUEST")
	}
	if !req.CIAddr.Equal(ip) {
		t.Errorf("renewal REQUEST ciaddr = %v, want %v", req.CIAddr, ip)
	}
	if req.RequestedIP != nil || req.ServerID != nil {
		t.Errorf("RFC 2131 renewal REQUEST must omit options 50/54, got reqIP=%v serverID=%v", req.RequestedIP, req.ServerID)
	}

	for _, p := range srv.packets() {
		if p.MessageType == MsgRelease {
			t.Fatalf("unexpected DHCPRELEASE sent on shutdown (violates D16)")
		}
	}
}

func TestMaintainFallsBackToObtainAfterNak(t *testing.T) {
	mac := net.HardwareAddr{0x02, 0xca, 0xfe, 0xba, 0xbe, 0x02}
	oldIP := net.IPv4(192, 168, 99, 10)
	newIP := net.IPv4(192, 168, 99, 25)
	serverIP := net.IPv4(192, 168, 99, 1)

	srv := startFakeDHCPServer(t, func(s *fakeDHCPServer, client *net.UDPAddr, req *Packet) {
		switch req.MessageType {
		case MsgDiscover:
			s.send(t, client, &Packet{
				Op: OpReply, HType: HTypeEthernet, HLen: HLenEthernet,
				XID: req.XID, YIAddr: newIP, CHAddr: req.CHAddr,
				MessageType: MsgOffer, ServerID: serverIP,
			})
		case MsgRequest:
			// Renewal request carries ciaddr; reject it with NAK so Maintain
			// must fall back to full DISCOVER -> OFFER -> REQUEST -> ACK.
			if req.CIAddr != nil && !req.CIAddr.Equal(net.IPv4zero) {
				s.send(t, client, &Packet{
					Op: OpReply, HType: HTypeEthernet, HLen: HLenEthernet,
					XID: req.XID, CHAddr: req.CHAddr,
					MessageType: MsgNak, ServerID: serverIP,
				})
				return
			}
			s.send(t, client, &Packet{
				Op: OpReply, HType: HTypeEthernet, HLen: HLenEthernet,
				XID: req.XID, YIAddr: newIP, CHAddr: req.CHAddr,
				MessageType: MsgAck, ServerID: serverIP,
				SubnetMask: net.CIDRMask(24, 32), LeaseTime: 600 * time.Second,
			})
		}
	})

	appliedCh := make(chan Lease, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	initial := Lease{
		IP:        oldIP,
		Mask:      net.CIDRMask(24, 32),
		PrefixLen: 24,
		ServerID:  serverIP,
		Duration:  200 * time.Millisecond,
		T1:        80 * time.Millisecond,
		T2:        160 * time.Millisecond,
		Obtained:  time.Now(),
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		Maintain(ctx, Options{
			IfName:        "eth0",
			MAC:           mac,
			Timeout:       time.Second,
			RetryInterval: 30 * time.Millisecond,
			Log:           testLogger(),
			Dial:          loopbackDial(srv),
			Apply: func(l Lease) error {
				appliedCh <- l
				return nil
			},
		}, initial)
	}()

	select {
	case rebound := <-appliedCh:
		if !rebound.IP.Equal(newIP) {
			t.Errorf("rebound IP = %v, want %v", rebound.IP, newIP)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for fallback Obtain after renewal NAK")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Maintain did not exit after cancel")
	}
}

func TestWriteResolvConf(t *testing.T) {
	t.Run("create idempotent and update", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "resolv.conf")
		dns1 := []net.IP{net.IPv4(8, 8, 8, 8), net.IPv4(1, 1, 1, 1)}

		if err := WriteResolvConf(p, dns1); err != nil {
			t.Fatalf("first WriteResolvConf: %v", err)
		}
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		want := "# written by wa-transcriber\nnameserver 8.8.8.8\nnameserver 1.1.1.1\n"
		if string(got) != want {
			t.Fatalf("content = %q, want %q", got, want)
		}

		// Pin an old mtime and re-write identical DNS servers: mtime must stay unchanged.
		oldTime := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
		if err := os.Chtimes(p, oldTime, oldTime); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
		if err := WriteResolvConf(p, dns1); err != nil {
			t.Fatalf("second WriteResolvConf: %v", err)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if !fi.ModTime().Equal(oldTime) {
			t.Errorf("mtime changed on identical content: got %v, want %v", fi.ModTime(), oldTime)
		}

		// Update with changed DNS list.
		dns2 := []net.IP{net.IPv4(9, 9, 9, 9)}
		if err := WriteResolvConf(p, dns2); err != nil {
			t.Fatalf("third WriteResolvConf: %v", err)
		}
		got, err = os.ReadFile(p)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		want2 := "# written by wa-transcriber\nnameserver 9.9.9.9\n"
		if string(got) != want2 {
			t.Errorf("updated content = %q, want %q", got, want2)
		}
	})

	t.Run("symlink to tmp target including dangling initial target", func(t *testing.T) {
		root := t.TempDir()
		etcDir := filepath.Join(root, "etc")
		tmpDir := filepath.Join(root, "tmp")
		if err := os.MkdirAll(etcDir, 0o755); err != nil {
			t.Fatalf("mkdir etc: %v", err)
		}
		if err := os.MkdirAll(tmpDir, 0o755); err != nil {
			t.Fatalf("mkdir tmp: %v", err)
		}

		target := filepath.Join(tmpDir, "resolv.conf")
		link := filepath.Join(etcDir, "resolv.conf")
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("Symlink: %v", err)
		}

		// Target does not exist yet (fresh tmpfs after boot).
		if err := WriteResolvConf(link, []net.IP{net.IPv4(192, 168, 1, 1)}); err != nil {
			t.Fatalf("WriteResolvConf via dangling symlink: %v", err)
		}

		// Verify symlink itself was preserved, not replaced with a regular file.
		lfi, err := os.Lstat(link)
		if err != nil {
			t.Fatalf("Lstat link: %v", err)
		}
		if lfi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("expected %s to remain a symlink", link)
		}
		gotTarget, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("ReadFile target: %v", err)
		}
		want := "# written by wa-transcriber\nnameserver 192.168.1.1\n"
		if string(gotTarget) != want {
			t.Errorf("target content = %q, want %q", gotTarget, want)
		}

		// Idempotency through existing symlink.
		oldTime := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
		if err := os.Chtimes(target, oldTime, oldTime); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
		if err := WriteResolvConf(link, []net.IP{net.IPv4(192, 168, 1, 1)}); err != nil {
			t.Fatalf("idempotent WriteResolvConf via symlink: %v", err)
		}
		tfi, err := os.Stat(target)
		if err != nil {
			t.Fatalf("Stat target: %v", err)
		}
		if !tfi.ModTime().Equal(oldTime) {
			t.Errorf("target mtime changed on identical content: got %v, want %v", tfi.ModTime(), oldTime)
		}

		// Update through existing symlink.
		if err := WriteResolvConf(link, []net.IP{net.IPv4(1, 1, 1, 1)}); err != nil {
			t.Fatalf("update WriteResolvConf via symlink: %v", err)
		}
		lfi, err = os.Lstat(link)
		if err != nil {
			t.Fatalf("Lstat link after update: %v", err)
		}
		if lfi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("expected %s to remain a symlink after update", link)
		}
		gotUpdated, err := os.ReadFile(link)
		if err != nil {
			t.Fatalf("ReadFile link: %v", err)
		}
		wantUpdated := "# written by wa-transcriber\nnameserver 1.1.1.1\n"
		if string(gotUpdated) != wantUpdated {
			t.Errorf("updated symlink content = %q, want %q", gotUpdated, wantUpdated)
		}
	})
}
