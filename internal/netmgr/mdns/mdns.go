// Package mdns answers mDNS (multicast DNS) queries for a name such as
// wa.local on every interface, so the device is reachable without knowing its
// IP address.
package mdns

import (
	"errors"
	"net"
	"strconv"
	"sync"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	// TTL is the record lifetime in seconds.
	TTL = 120
	// cacheFlush is the top bit of the record class in a response.
	cacheFlush = 0x8000
	// unicastResponse is the top bit of the question class: reply by unicast.
	unicastResponse = 0x8000
)

// Response is a packet to send plus how to send it.
type Response struct {
	Packet  []byte
	Unicast bool // send to the querier only, not to the multicast group
}

// Responder answers for one name (e.g. "wa.local").
type Responder struct {
	// Name is the label before .local, e.g. "wa" or "wa-2".
	Name string

	mu   sync.Mutex
	next int
}

// Rename switches to the next free suffix (wa -> wa-2 -> wa-3) after a name
// conflict and returns the new label.
func (r *Responder) Rename() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	base := r.Name
	if i := indexOf(base, '-'); i > 0 {
		base = base[:i]
	}
	r.next++
	r.Name = base + "-" + strconv.Itoa(r.next+1)
	return r.Name
}

// Label returns the current label (safe for concurrent use).
func (r *Responder) Label() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Name
}

func indexOf(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// Answer turns a query packet into a response for ip, or returns nil when the
// packet asks for something else. ifIP is the address on the interface the
// query arrived on.
func (r *Responder) Answer(query []byte, ifIP net.IP) (*Response, error) {
	return r.answer(query, ifIP, false)
}

// AnswerTo is Answer for a query that came from a source port other than 5353.
// Such a querier is a "simple resolver" doing one-shot queries (RFC 6762
// section 5.1), and section 6.7 requires a conventional unicast response:
// the query ID and the question are repeated, the cache-flush bit is not set
// and the TTL is at most ten seconds. Android's resolver sends exactly these
// one-shot queries, so without this its resolve of wa.local fails even when
// the packet arrives.
func (r *Responder) AnswerTo(query []byte, ifIP net.IP, legacy bool) (*Response, error) {
	return r.answer(query, ifIP, legacy)
}

const legacyTTL = 10

func (r *Responder) answer(query []byte, ifIP net.IP, legacy bool) (*Response, error) {
	ip4 := ifIP.To4()
	if ip4 == nil {
		return nil, errors.New("mdns: not an IPv4 address")
	}
	var p dnsmessage.Parser
	hdr, err := p.Start(query)
	if err != nil {
		return nil, err
	}
	if hdr.Response {
		return nil, nil // a response, not a query: nothing to answer
	}

	var questions []dnsmessage.Question
	unicast := false
	for {
		q, err := p.Question()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			break
		}
		if err != nil {
			return nil, err
		}
		if !r.matches(q.Name) {
			continue
		}
		if q.Type != dnsmessage.TypeA && q.Type != dnsmessage.TypeALL {
			continue
		}
		if uint16(q.Class)&unicastResponse != 0 {
			unicast = true
		}
		questions = append(questions, dnsmessage.Question{
			Name:  q.Name,
			Type:  dnsmessage.TypeA,
			Class: dnsmessage.ClassINET,
		})
	}
	if len(questions) == 0 {
		return nil, nil
	}

	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:            responseID(hdr.ID, legacy),
		Response:      true,
		Authoritative: true,
	})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	for _, q := range questions {
		if err := b.Question(q); err != nil {
			return nil, err
		}
	}
	if err := b.StartAnswers(); err != nil {
		return nil, err
	}
	var a [4]byte
	copy(a[:], ip4)
	rr := dnsmessage.ResourceHeader{
		Name:  r.fqdn(),
		Type:  dnsmessage.TypeA,
		Class: dnsmessage.ClassINET,
		TTL:   TTL,
	}
	if legacy {
		// Conventional response: no cache-flush bit, short TTL.
		rr.TTL = legacyTTL
	} else {
		rr.Class |= cacheFlush
	}
	if err := b.AResource(rr, dnsmessage.AResource{A: a}); err != nil {
		return nil, err
	}
	pkt, err := b.Finish()
	if err != nil {
		return nil, err
	}
	return &Response{Packet: pkt, Unicast: unicast || legacy}, nil
}

// responseID returns the identifier a response carries: zero for multicast
// responses (RFC 6762 section 18.1) and the query's identifier echoed back for
// a legacy unicast response (section 6.7).
func responseID(queryID uint16, legacy bool) uint16 {
	if legacy {
		return queryID
	}
	return 0
}

// Announce builds an unsolicited response (used after an address change).
func (r *Responder) Announce(ifIP net.IP) ([]byte, error) {
	ip4 := ifIP.To4()
	if ip4 == nil {
		return nil, errors.New("mdns: not an IPv4 address")
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{Response: true, Authoritative: true})
	b.EnableCompression()
	if err := b.StartAnswers(); err != nil {
		return nil, err
	}
	var a [4]byte
	copy(a[:], ip4)
	rr := dnsmessage.ResourceHeader{
		Name:  r.fqdn(),
		Type:  dnsmessage.TypeA,
		Class: dnsmessage.ClassINET | cacheFlush,
		TTL:   TTL,
	}
	if err := b.AResource(rr, dnsmessage.AResource{A: a}); err != nil {
		return nil, err
	}
	return b.Finish()
}

// OtherAddress looks at an incoming packet and, when it is a response that
// claims r's name with a different address, returns that address. It is how
// a name conflict is detected (another device already answers as wa.local).
func (r *Responder) OtherAddress(pkt []byte) (net.IP, bool) {
	var p dnsmessage.Parser
	hdr, err := p.Start(pkt)
	if err != nil || !hdr.Response {
		return nil, false
	}
	if _, err := p.AllQuestions(); err != nil {
		return nil, false
	}
	for {
		h, err := p.AnswerHeader()
		if err != nil {
			return nil, false
		}
		if h.Type == dnsmessage.TypeA && r.matches(h.Name) {
			a, err := p.AResource()
			if err != nil {
				return nil, false
			}
			return net.IP(a.A[:]), true
		}
		if err := p.SkipAnswer(); err != nil {
			return nil, false
		}
	}
}

func (r *Responder) matches(n dnsmessage.Name) bool {
	return equalFold(n.String(), r.fqdn().String())
}

func (r *Responder) fqdn() dnsmessage.Name {
	name := r.Label()
	if name == "" {
		name = "wa"
	}
	n, err := dnsmessage.NewName(name + ".local.")
	if err != nil {
		n, _ = dnsmessage.NewName("wa.local.")
	}
	return n
}

// equalFold compares two DNS names case-insensitively (DNS names are
// case-insensitive).
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
