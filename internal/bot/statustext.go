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
	fmt.Fprintf(&sb, "📟 wa-transcriber %s\n", b.Version)
	wa := "offline"
	if s.Connected {
		wa = "online"
	}
	if s.Account != "" {
		wa += " as +" + s.Account
	}
	fmt.Fprintf(&sb, "• WhatsApp: %s\n", wa)
	fmt.Fprintf(&sb, "• Last message seen: %s\n", ago(s.LastMessage, now))
	fmt.Fprintf(&sb, "• Last transcription: %s\n", ago(s.LastTranscription, now))

	tr := cfg.Transcription
	configured := "not configured"
	if b.Transcriber != nil && b.Transcriber.Configured() {
		configured = b.Transcriber.Describe()
	}
	fmt.Fprintf(&sb, "• Transcription: %s, global %s, %d chat override(s)\n",
		configured, onoff(tr.Global), len(tr.Chats))
	fmt.Fprintf(&sb, "• Uptime: %s\n", now.Sub(s.Started).Round(time.Second))
	if b.Coord != nil {
		claims, stoodDown, tookOver, mode := b.Coord.Counters()
		fmt.Fprintf(&sb, "• Coordination: %s, claims %d, stood down %d, took over %d\n",
			mode, claims, stoodDown, tookOver)
	}
	if !s.ClockSynced {
		sb.WriteString("• Clock: ⚠ not synced when WhatsApp was connected\n")
	}

	if b.Network != nil {
		if line := ifaceLine(b.Network.Interfaces()); line != "" {
			fmt.Fprintf(&sb, "• Interfaces: %s\n", line)
		}
	}

	if b.WebPort == "" {
		sb.WriteString("• Web UI: disabled")
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
		urls = append(urls, "no network address")
	}
	fmt.Fprintf(&sb, "• Web UI:\n%s", strings.Join(urls, "\n"))
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

func ago(t, now time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%s (%s ago)", t.Local().Format("2006-01-02 15:04:05"), d.Round(time.Second))
}
