package mdns

import (
	"net"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func buildQuery(t *testing.T, name string, qtype dnsmessage.Type, unicast bool) []byte {
	t.Helper()
	n, err := dnsmessage.NewName(name)
	if err != nil {
		t.Fatal(err)
	}
	class := dnsmessage.ClassINET
	if unicast {
		class |= unicastResponse
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{})
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := b.Question(dnsmessage.Question{Name: n, Type: qtype, Class: class}); err != nil {
		t.Fatal(err)
	}
	pkt, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return pkt
}

// parseA extracts the A record from a response packet.
func parseA(t *testing.T, pkt []byte) (name string, ip net.IP, hdr dnsmessage.Header) {
	t.Helper()
	var p dnsmessage.Parser
	hdr, err := p.Start(pkt)
	if err != nil {
		t.Fatal(err)
	}
	name = ""
	if _, err := p.AllQuestions(); err != nil {
		t.Fatal(err)
	}
	for {
		h, err := p.AnswerHeader()
		if err != nil {
			break
		}
		if h.Type != dnsmessage.TypeA {
			if err := p.SkipAnswer(); err != nil {
				t.Fatal(err)
			}
			continue
		}
		a, err := p.AResource()
		if err != nil {
			t.Fatal(err)
		}
		name = h.Name.String()
		ip = net.IP(a.A[:])
		if h.TTL != TTL {
			t.Errorf("TTL = %d, want %d", h.TTL, TTL)
		}
		if uint16(h.Class)&cacheFlush == 0 {
			t.Error("cache-flush bit not set")
		}
	}
	return name, ip, hdr
}

func TestAnswerAQuery(t *testing.T) {
	r := &Responder{Name: "wa"}
	q := buildQuery(t, "wa.local.", dnsmessage.TypeA, false)
	resp, err := r.Answer(q, net.IPv4(192, 168, 1, 42))
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Unicast {
		t.Fatalf("resp = %+v", resp)
	}
	name, ip, hdr := parseA(t, resp.Packet)
	if name != "wa.local." {
		t.Fatalf("name = %q", name)
	}
	if !ip.Equal(net.IPv4(192, 168, 1, 42)) {
		t.Fatalf("ip = %v", ip)
	}
	if !hdr.Response || !hdr.Authoritative {
		t.Fatalf("header = %+v", hdr)
	}
}

func TestAnswerCaseInsensitiveAndANY(t *testing.T) {
	r := &Responder{Name: "wa"}
	for _, q := range [][]byte{
		buildQuery(t, "WA.LOCAL.", dnsmessage.TypeA, false),
		buildQuery(t, "wa.local.", dnsmessage.TypeALL, false),
	} {
		resp, err := r.Answer(q, net.IPv4(10, 0, 0, 2))
		if err != nil {
			t.Fatal(err)
		}
		if resp == nil {
			t.Fatal("no response")
		}
	}
}

func TestAnswerUnicastBit(t *testing.T) {
	r := &Responder{Name: "wa"}
	resp, err := r.Answer(buildQuery(t, "wa.local.", dnsmessage.TypeA, true), net.IPv4(10, 0, 0, 2))
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || !resp.Unicast {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestAnswerIgnoresOtherNamesAndTypes(t *testing.T) {
	r := &Responder{Name: "wa"}
	cases := [][]byte{
		buildQuery(t, "other.local.", dnsmessage.TypeA, false),
		buildQuery(t, "wa.local.", dnsmessage.TypeAAAA, false),
		buildQuery(t, "wa.local.", dnsmessage.TypeTXT, false),
	}
	for _, q := range cases {
		resp, err := r.Answer(q, net.IPv4(10, 0, 0, 2))
		if err != nil {
			t.Fatal(err)
		}
		if resp != nil {
			t.Fatalf("answered %v", q)
		}
	}
}

func TestAnswerIgnoresResponses(t *testing.T) {
	r := &Responder{Name: "wa"}
	pkt, err := r.Announce(net.IPv4(10, 0, 0, 2))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := r.Answer(pkt, net.IPv4(10, 0, 0, 2))
	if err != nil {
		t.Fatal(err)
	}
	if resp != nil {
		t.Fatal("answered a response")
	}
}

func TestAnnouncePacket(t *testing.T) {
	r := &Responder{Name: "wa"}
	pkt, err := r.Announce(net.IPv4(192, 168, 0, 7))
	if err != nil {
		t.Fatal(err)
	}
	name, ip, _ := parseA(t, pkt)
	if name != "wa.local." || !ip.Equal(net.IPv4(192, 168, 0, 7)) {
		t.Fatalf("name=%q ip=%v", name, ip)
	}
}

func TestOtherAddressDetectsConflict(t *testing.T) {
	mine := &Responder{Name: "wa"}
	other := &Responder{Name: "wa"}
	pkt, err := other.Announce(net.IPv4(192, 168, 1, 99))
	if err != nil {
		t.Fatal(err)
	}
	ip, ok := mine.OtherAddress(pkt)
	if !ok || !ip.Equal(net.IPv4(192, 168, 1, 99)) {
		t.Fatalf("conflict not detected: %v %v", ip, ok)
	}
	// A different name is not a conflict.
	other2 := &Responder{Name: "nas"}
	pkt, _ = other2.Announce(net.IPv4(192, 168, 1, 50))
	if _, ok := mine.OtherAddress(pkt); ok {
		t.Fatal("false conflict for another name")
	}
	// Queries are not conflicts.
	if _, ok := mine.OtherAddress(buildQuery(t, "wa.local.", dnsmessage.TypeA, false)); ok {
		t.Fatal("query treated as conflict")
	}
}

func TestNameConflictRenames(t *testing.T) {
	// The loop renames with a suffix; here we check the generated name is
	// valid DNS and answers.
	r := &Responder{Name: "wa-2"}
	resp, err := r.Answer(buildQuery(t, "wa-2.local.", dnsmessage.TypeA, false), net.IPv4(10, 1, 2, 3))
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil {
		t.Fatal("no answer for wa-2.local")
	}
	name, ip, _ := parseA(t, resp.Packet)
	if name != "wa-2.local." || !ip.Equal(net.IPv4(10, 1, 2, 3)) {
		t.Fatalf("name=%q ip=%v", name, ip)
	}
}

func TestRenameSequence(t *testing.T) {
	r := &Responder{Name: "wa"}
	if got := r.Rename(); got != "wa-2" {
		t.Fatalf("first rename = %q", got)
	}
	if got := r.Rename(); got != "wa-3" {
		t.Fatalf("second rename = %q", got)
	}
	if got := r.Label(); got != "wa-3" {
		t.Fatalf("label = %q", got)
	}
}
