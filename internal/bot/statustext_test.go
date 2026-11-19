package bot

import (
	"strings"
	"testing"
)

func TestStatusTextWarnsAboutUnsyncedClock(t *testing.T) {
	e := newEnv(t)
	e.bot.Status.SetClockSynced(false)
	if got := e.bot.statusText(); !strings.Contains(got, "not synced") {
		t.Fatalf("no clock warning:\n%s", got)
	}
	e.bot.Status.SetClockSynced(true)
	if got := e.bot.statusText(); strings.Contains(got, "not synced") {
		t.Fatalf("unexpected clock warning:\n%s", got)
	}
}
