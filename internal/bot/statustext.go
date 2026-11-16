package bot

import (
	"fmt"
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
	if b.Transcriber != nil {
		configured = tr.Provider
		if tr.Model != "" {
			configured += " / " + tr.Model
		}
	}
	fmt.Fprintf(&sb, "• Transcription: %s, global %s, %d chat override(s)\n",
		configured, onoff(tr.Global), len(tr.Chats))
	fmt.Fprintf(&sb, "• Uptime: %s\n", now.Sub(s.Started).Round(time.Second))

	var urls []string
	if cfg.PublicURL != "" {
		urls = append(urls, cfg.PublicURL)
	}
	if b.Addrs != nil {
		for _, a := range b.Addrs() {
			urls = append(urls, fmt.Sprintf("http://%s (%s)", a.IP, a.Interface))
		}
	}
	if len(urls) == 0 {
		urls = append(urls, "no network address")
	}
	fmt.Fprintf(&sb, "• Web UI: %s", strings.Join(urls, ", "))
	return sb.String()
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
