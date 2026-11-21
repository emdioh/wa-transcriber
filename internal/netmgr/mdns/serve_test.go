package mdns

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/ipv4"
)

func iface(index int, ip string, name string) Iface {
	return Iface{Index: index, IP: net.ParseIP(ip).To4(), Name: name}
}

func TestAddrTrackerAnnouncesOnlyChanges(t *testing.T) {
	tests := []struct {
		name   string
		before [][]Iface
		now    []Iface
		want   []int
	}{
		{
			name: "first call announces every address",
			now:  []Iface{iface(2, "192.168.1.40", "eth0"), iface(3, "192.168.4.1", "wlan0")},
			want: []int{2, 3},
		},
		{
			name:   "same address is not announced twice",
			before: [][]Iface{{iface(2, "192.168.1.40", "eth0")}},
			now:    []Iface{iface(2, "192.168.1.40", "eth0")},
			want:   nil,
		},
		{
			name:   "changed address is announced again",
			before: [][]Iface{{iface(3, "192.168.4.1", "wlan0")}},
			now:    []Iface{iface(3, "192.168.1.41", "wlan0")},
			want:   []int{3},
		},
		{
			name:   "only the changed interface",
			before: [][]Iface{{iface(2, "192.168.1.40", "eth0"), iface(3, "192.168.4.1", "wlan0")}},
			now:    []Iface{iface(2, "192.168.1.40", "eth0"), iface(3, "10.0.0.7", "wlan0")},
			want:   []int{3},
		},
		{
			name:   "new interface",
			before: [][]Iface{{iface(2, "192.168.1.40", "eth0")}},
			now:    []Iface{iface(2, "192.168.1.40", "eth0"), iface(4, "169.254.1.2", "usb0")},
			want:   []int{4},
		},
		{
			name:   "interface that goes away is announced when it returns",
			before: [][]Iface{{iface(3, "192.168.4.1", "wlan0")}, nil},
			now:    []Iface{iface(3, "192.168.4.1", "wlan0")},
			want:   []int{3},
		},
		{
			name:   "no address, nothing to announce",
			before: [][]Iface{{iface(2, "192.168.1.40", "eth0")}},
			now:    []Iface{{Index: 2, Name: "eth0"}},
			want:   nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := &addrTracker{last: map[int]net.IP{}}
			for _, step := range tc.before {
				tr.Changed(step)
			}
			got := tr.Changed(tc.now)
			if len(got) != len(tc.want) {
				t.Fatalf("changed %d interfaces %v, want %v", len(got), got, tc.want)
			}
			for i, idx := range tc.want {
				if got[i].Index != idx {
					t.Fatalf("changed[%d] = iface %d, want %d", i, got[i].Index, idx)
				}
			}
		})
	}
}

func TestAddrTrackerKeepsIPv4Only(t *testing.T) {
	tr := &addrTracker{last: map[int]net.IP{}}
	got := tr.Changed([]Iface{{Index: 5, IP: net.ParseIP("fe80::1"), Name: "wlan0"}})
	if len(got) != 0 {
		t.Fatalf("announced a non-IPv4 address: %v", got)
	}
	if len(tr.last) != 0 {
		t.Fatalf("recorded a non-IPv4 address: %v", tr.last)
	}
}

// fakeSender records the announcements instead of sending them.
type fakeSender struct {
	mu      sync.Mutex
	writes  []fakeWrite
	err     error
	onWrite func(n int)
}

type fakeWrite struct {
	ifIndex int
	dst     net.Addr
	packet  []byte
}

func (f *fakeSender) WriteTo(b []byte, cm *ipv4.ControlMessage, dst net.Addr) (int, error) {
	f.mu.Lock()
	idx := 0
	if cm != nil {
		idx = cm.IfIndex
	}
	f.writes = append(f.writes, fakeWrite{ifIndex: idx, dst: dst, packet: append([]byte(nil), b...)})
	n := len(f.writes)
	err := f.err
	f.mu.Unlock()
	if f.onWrite != nil {
		f.onWrite(n)
	}
	return len(b), err
}

func (f *fakeSender) snapshot() []fakeWrite {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeWrite(nil), f.writes...)
}

func TestAnnounceSendsTwoPacketPerInterface(t *testing.T) {
	r := &Responder{Name: "wa"}
	s := &fakeSender{}
	gaddr := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}
	want := iface(3, "192.168.1.41", "wlan0")
	if err := announce(context.Background(), s, r, want, gaddr, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	writes := s.snapshot()
	if len(writes) != 2 {
		t.Fatalf("%d writes, want 2 (PLAN M5 step 9)", len(writes))
	}
	for i, w := range writes {
		if w.ifIndex != want.Index {
			t.Fatalf("write %d went out of iface %d, want %d", i, w.ifIndex, want.Index)
		}
		if w.dst.String() != gaddr.String() {
			t.Fatalf("write %d to %v, want %v", i, w.dst, gaddr)
		}
		name, ip, _ := parseA(t, w.packet)
		if name != "wa.local." || !ip.Equal(want.IP) {
			t.Fatalf("write %d: name=%q ip=%v", i, name, ip)
		}
	}
	if string(writes[0].packet) != string(writes[1].packet) {
		t.Fatal("the two announcements differ")
	}
}

func TestAnnounceStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &fakeSender{onWrite: func(n int) {
		if n == 1 {
			cancel()
		}
	}}
	// A gap far longer than the test: only the cancellation can end it.
	if err := announce(ctx, s, &Responder{Name: "wa"}, iface(2, "10.0.0.2", "eth0"), &net.UDPAddr{Port: 5353}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if got := len(s.snapshot()); got != 1 {
		t.Fatalf("%d writes after cancel, want 1", got)
	}
}

func TestAnnounceReportsWriteError(t *testing.T) {
	boom := errors.New("boom")
	s := &fakeSender{err: boom}
	err := announce(context.Background(), s, &Responder{Name: "wa"}, iface(2, "10.0.0.2", "eth0"), &net.UDPAddr{Port: 5353}, time.Millisecond)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if got := len(s.snapshot()); got != 1 {
		t.Fatalf("%d writes after the failure, want 1", got)
	}
}

// A conflict must be a *different* address. Our own announcements come back on
// the socket (multicast loopback) and mDNS reflectors echo them around the LAN;
// treating those as a conflict made the device rename itself to wa-2.local on
// its own address, and then nothing resolved wa.local any more (seen on the
// board: "name conflict, renamed ... ip=192.168.15.43", the board's own IP).
func TestOwnAnnouncementIsNotAConflict(t *testing.T) {
	r := &Responder{Name: "wa"}
	own := Iface{Index: 2, IP: net.IPv4(192, 168, 15, 43), Name: "eth0"}
	pkt, err := r.Announce(own.IP)
	if err != nil {
		t.Fatal(err)
	}
	ip, ok := r.OtherAddress(pkt)
	if !ok {
		t.Fatal("OtherAddress did not see the A record")
	}
	if !ip.Equal(own.IP) {
		t.Fatalf("OtherAddress = %v, want %v", ip, own.IP)
	}
	if !isOwnAddress(ip, []Iface{own}) {
		t.Fatal("our own address was not recognised: the device would rename itself")
	}
	other := net.IPv4(192, 168, 15, 99)
	if isOwnAddress(other, []Iface{own}) {
		t.Fatal("another device's address was taken for ours: a real conflict would be ignored")
	}
	if r.Label() != "wa" {
		t.Fatalf("label changed to %q", r.Label())
	}
}

func TestLocalIPFor(t *testing.T) {
	eth0 := Iface{Index: 2, IP: net.IPv4(192, 168, 15, 43), Name: "eth0"}
	wlan0 := Iface{Index: 3, IP: net.IPv4(192, 168, 15, 44), Name: "wlan0"}
	ifaces := []Iface{eth0, wlan0}
	src := &net.UDPAddr{IP: net.IPv4(192, 168, 15, 13), Port: 40000}

	cases := []struct {
		name   string
		ifaces []Iface
		cm     *ipv4.ControlMessage
		want   string
	}{
		{"destination address wins", ifaces, &ipv4.ControlMessage{IfIndex: 2, Dst: wlan0.IP}, "192.168.15.44"},
		{"interface index", ifaces, &ipv4.ControlMessage{IfIndex: 3}, "192.168.15.44"},
		{"one interface, no control message", []Iface{eth0}, nil, "192.168.15.43"},
		{"same /24 fallback", ifaces, nil, "192.168.15.43"},
		// Two interfaces, neither in the querier's subnet and no control
		// message: there is no defensible answer, so stay quiet.
		{"nothing to answer with", []Iface{
			{Index: 4, IP: net.IPv4(10, 0, 0, 2), Name: "usb0"},
			{Index: 5, IP: net.IPv4(172, 16, 0, 2), Name: "eth1"},
		}, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := localIPFor(c.ifaces, c.cm, src)
			if c.want == "" {
				if got != nil {
					t.Fatalf("got %v, want nil", got)
				}
				return
			}
			if got == nil || got.String() != c.want {
				t.Fatalf("got %v, want %s", got, c.want)
			}
		})
	}
}

func TestIsMulticast(t *testing.T) {
	gaddr := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}
	if !isMulticast(&ipv4.ControlMessage{Dst: gaddr.IP}, gaddr) {
		t.Fatal("group destination not recognised as multicast")
	}
	if isMulticast(&ipv4.ControlMessage{Dst: net.IPv4(192, 168, 15, 43)}, gaddr) {
		t.Fatal("unicast destination taken for multicast")
	}
	if isMulticast(nil, gaddr) {
		t.Fatal("missing control message must not count as multicast")
	}
}
