package apsrv

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

const (
	// DefaultLease is the default DHCPv4 lease lifetime (matches dnsmasq).
	DefaultLease = time.Hour
)

// Config configures a combined DHCPv4 + wildcard DNS setup-AP server.
type Config struct {
	// IfName is the wireless interface name (e.g. "wlan0"). On Linux, the DHCP
	// UDP socket binds to this interface with SO_BINDTODEVICE when non-empty.
	IfName string

	// ServerIP is the AP's IPv4 address, used as DHCP server ID, router, DNS
	// server, and wildcard DNS answer. Defaults to 192.168.4.1.
	ServerIP net.IP

	// Mask is the IPv4 subnet mask. Defaults to 255.255.255.0.
	Mask net.IPMask

	// PoolStart and PoolEnd define the inclusive DHCP address pool.
	// Default to <ServerIP /24>.10 .. <ServerIP /24>.100 (192.168.4.10..100).
	PoolStart net.IP
	PoolEnd   net.IP

	// Lease is the DHCP lease duration. Defaults to 1h.
	Lease time.Duration

	// CaptiveURL is the RFC 8910 Option 114 Captive Portal URI.
	// Defaults to "http://<ServerIP>/".
	CaptiveURL string

	// Log is the logger for diagnostic messages. Defaults to slog.Default().
	Log *slog.Logger

	// Optional overrides for tests (leave empty in production).
	// DHCPAddr defaults to ":67"; DNSAddr defaults to "<ServerIP>:53".
	DHCPAddr      string
	DNSAddr       string
	DHCPReplyAddr DHCPReplyAddrFunc
}

func (c Config) withDefaults() (Config, error) {
	if c.ServerIP == nil {
		c.ServerIP = net.IPv4(192, 168, 4, 1)
	}
	ip4 := c.ServerIP.To4()
	if ip4 == nil {
		return c, fmt.Errorf("apsrv: ServerIP %v is not IPv4", c.ServerIP)
	}
	c.ServerIP = append(net.IP(nil), ip4...)

	if c.Mask == nil {
		c.Mask = net.CIDRMask(24, 32)
	} else if len(c.Mask) != 4 {
		return c, fmt.Errorf("apsrv: Mask %v is not 4-byte IPv4", c.Mask)
	}

	if c.PoolStart == nil {
		c.PoolStart = net.IPv4(ip4[0], ip4[1], ip4[2], 10).To4()
	}
	if c.PoolEnd == nil {
		c.PoolEnd = net.IPv4(ip4[0], ip4[1], ip4[2], 100).To4()
	}
	if c.Lease <= 0 {
		c.Lease = DefaultLease
	}
	if c.CaptiveURL == "" {
		c.CaptiveURL = "http://" + c.ServerIP.String() + "/"
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.DHCPAddr == "" {
		c.DHCPAddr = fmt.Sprintf(":%d", dhcpServerPort)
	}
	if c.DNSAddr == "" {
		c.DNSAddr = net.JoinHostPort(c.ServerIP.String(), "53")
	}
	return c, nil
}

// Server runs both DHCPServer and DNSServer on UDP sockets until ctx is
// cancelled or Close is called.
type Server struct {
	DHCP *DHCPServer
	DNS  *DNSServer

	dhcpConn net.PacketConn
	dnsConn  net.PacketConn
	log      *slog.Logger

	cancel    context.CancelFunc
	closeOnce sync.Once
	closeErr  error
	wg        sync.WaitGroup
}

// Start binds the DHCP and DNS UDP sockets and launches both serving loops.
// Cancelling ctx or calling Close shuts down both listeners and waits for all
// goroutines to exit.
func Start(ctx context.Context, cfg Config) (*Server, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}

	dhcpSrv, err := NewDHCPServer(DHCPConfig{
		ServerIP:   cfg.ServerIP,
		Mask:       cfg.Mask,
		PoolStart:  cfg.PoolStart,
		PoolEnd:    cfg.PoolEnd,
		Lease:      cfg.Lease,
		CaptiveURL: cfg.CaptiveURL,
		Log:        cfg.Log,
		ReplyAddr:  cfg.DHCPReplyAddr,
	})
	if err != nil {
		return nil, err
	}
	dnsSrv, err := NewDNSServer(cfg.ServerIP, cfg.Log)
	if err != nil {
		return nil, err
	}

	dhcpConn, err := listenUDP(ctx, cfg.DHCPAddr, cfg.IfName, true)
	if err != nil {
		return nil, fmt.Errorf("apsrv: listen dhcp %s: %w", cfg.DHCPAddr, err)
	}
	dnsConn, err := listenUDP(ctx, cfg.DNSAddr, "", false)
	if err != nil {
		_ = dhcpConn.Close()
		return nil, fmt.Errorf("apsrv: listen dns %s: %w", cfg.DNSAddr, err)
	}

	srvCtx, cancel := context.WithCancel(ctx)
	s := &Server{
		DHCP:     dhcpSrv,
		DNS:      dnsSrv,
		dhcpConn: dhcpConn,
		dnsConn:  dnsConn,
		log:      cfg.Log,
		cancel:   cancel,
	}

	s.wg.Add(3)
	go func() {
		defer s.wg.Done()
		<-srvCtx.Done()
		s.closeConns()
	}()
	go func() {
		defer s.wg.Done()
		if err := s.DHCP.Serve(s.dhcpConn); err != nil && srvCtx.Err() == nil {
			s.log.Warn("apsrv: dhcp server stopped with error", "err", err)
		}
	}()
	go func() {
		defer s.wg.Done()
		if err := s.DNS.Serve(s.dnsConn); err != nil && srvCtx.Err() == nil {
			s.log.Warn("apsrv: dns server stopped with error", "err", err)
		}
	}()

	return s, nil
}

// DHCPAddr returns the local address bound by the DHCP socket.
func (s *Server) DHCPAddr() net.Addr {
	return s.dhcpConn.LocalAddr()
}

// DNSAddr returns the local address bound by the DNS socket.
func (s *Server) DNSAddr() net.Addr {
	return s.dnsConn.LocalAddr()
}

// Close stops both UDP listeners and waits for all background goroutines to
// exit. Safe to call multiple times.
func (s *Server) Close() error {
	s.cancel()
	s.closeConns()
	s.wg.Wait()
	return s.closeErr
}

func (s *Server) closeConns() {
	s.closeOnce.Do(func() {
		var errs []error
		if err := s.dhcpConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
		if err := s.dnsConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
		s.closeErr = errors.Join(errs...)
	})
}
