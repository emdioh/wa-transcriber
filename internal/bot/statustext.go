package bot

import (
	"fmt"
	"net"
	"strings"
	"time"
)

func (b *Bot) statusText() string {
	now := b.now()
	s := b.Status.Snapshot()
	cfg := b.Config.Get()

	var sb strings.Builder
	fmt.Fprintf(&sb, b.t(msgStatusHeader), b.Version)
	wa := b.t(msgStatusOffline)
	if s.Connected {
		wa = b.t(msgStatusOnline)
	}
	if s.Account != "" {
		wa = b.t(msgStatusOnlineAs, s.Account)
	}
	fmt.Fprintf(&sb, b.t(msgStatusWhatsApp), wa)
	fmt.Fprintf(&sb, b.t(msgStatusLastMessage), b.ago(s.LastMessage, now))
	fmt.Fprintf(&sb, b.t(msgStatusLastTranscription), b.ago(s.LastTranscription, now))

	tr := cfg.Transcription
	configured := b.t(msgStatusNotConfigured)
	if b.Transcriber != nil && b.Transcriber.Configured() {
		configured = b.Transcriber.Describe()
	}
	fmt.Fprintf(&sb, b.t(msgStatusTranscription),
		configured, b.onoff(tr.Global), len(tr.Chats))
	fmt.Fprintf(&sb, b.t(msgStatusUptime), now.Sub(s.Started).Round(time.Second))
	if b.Coord != nil {
		claims, stoodDown, tookOver, mode := b.Coord.Counters()
		modeText := b.t(msgModeObserve)
		if mode == "claim" {
			modeText = b.t(msgModeClaim)
		}
		fmt.Fprintf(&sb, b.t(msgStatusCoordination),
			modeText, claims, stoodDown, tookOver)
	}
	if !s.ClockSynced {
		sb.WriteString(b.t(msgStatusClock))
	}

	if b.Network != nil {
		if line := ifaceLine(b.Network.Interfaces()); line != "" {
			fmt.Fprintf(&sb, b.t(msgStatusInterfaces), line)
		}
	}

	if b.WebPort == "" {
		sb.WriteString(b.t(msgStatusWebDisabled))
		return sb.String()
	}
	var urls []string
	if cfg.PublicURL != "" {
		urls = append(urls, cfg.PublicURL)
	}
	if b.Addrs != nil {
		for _, a := range b.Addrs() {
			// Inline code (`…`): WhatsApp does not link IP URLs properly,
			// so make them at least easy to copy with a long press.
			urls = append(urls, fmt.Sprintf("%s:\n`%s`", a.Interface, webURL(a.IP, b.WebPort)))
		}
	}
	if len(urls) == 0 {
		urls = append(urls, b.t(msgStatusNoAddress))
	}
	fmt.Fprintf(&sb, b.t(msgStatusWebHeader), strings.Join(urls, "\n"))
	return sb.String()
}

// ifaceLine renders the interfaces like "eth0 192.168.1.40, wlan0 AP WA-Setup-1A2B".
func ifaceLine(ifaces []NetIface) string {
	var parts []string
	for _, i := range ifaces {
		part := i.Name
		if i.APClients > 0 || i.SSID == "WA-Setup" {
			part += " AP " + i.SSID
		} else if i.SSID != "" {
			part += " " + i.SSID
		}
		if len(i.IPs) > 0 {
			part += " " + strings.Join(i.IPs, ", ")
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

// webURL is http://ip:port/, without the port when it is the default 80.
func webURL(ip, port string) string {
	if port == "80" {
		return "http://" + ip + "/"
	}
	return "http://" + net.JoinHostPort(ip, port) + "/"
}

func (b *Bot) ago(t, now time.Time) string {
	if t.IsZero() {
		return b.t(msgNever)
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	return b.t(msgAgo, t.Local().Format("2006-01-02 15:04:05"), d.Round(time.Second))
}
