package netmgr

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func testClockLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestClockSane(t *testing.T) {
	build := time.Unix(1700000000, 0)
	cases := []struct {
		name string
		c    ClockCheck
		want bool
	}{
		{"after build", ClockCheck{Now: func() time.Time { return build.Add(time.Hour) }, BuildTime: build}, true},
		{"before build", ClockCheck{Now: func() time.Time { return build.Add(-time.Hour) }, BuildTime: build}, false},
		{"no build time", ClockCheck{Now: func() time.Time { return build }}, true},
		{"managed but not synced", ClockCheck{Managed: true, Now: func() time.Time { return build.Add(time.Hour) },
			BuildTime: build, Synced: func() bool { return false }}, false},
		{"managed and synced", ClockCheck{Managed: true, Now: func() time.Time { return build.Add(time.Hour) },
			BuildTime: build, Synced: func() bool { return true }}, true},
		{"unmanaged ignores sync", ClockCheck{Now: func() time.Time { return build.Add(time.Hour) },
			BuildTime: build, Synced: func() bool { return false }}, true},
	}
	for _, tc := range cases {
		if got := clockSane(tc.c); got != tc.want {
			t.Errorf("%s: clockSane = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestWaitClockReturnsImmediatelyWhenSane(t *testing.T) {
	c := ClockCheck{Now: time.Now, Poll: time.Millisecond, Log: testClockLog()}
	if !WaitClock(context.Background(), c) {
		t.Fatal("sane clock reported as insane")
	}
}

func TestWaitClockGivesUpAfterDeadline(t *testing.T) {
	// A clock stuck before the build time: WaitClock must return false after
	// ClockWait. We shorten it through a cancelled context instead of waiting
	// two minutes.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := ClockCheck{
		Now:       func() time.Time { return time.Unix(1000, 0) },
		BuildTime: time.Unix(2000, 0),
		Poll:      time.Millisecond,
		Log:       testClockLog(),
	}
	if WaitClock(ctx, c) {
		t.Fatal("insane clock reported as sane")
	}
}

func TestWaitClockThenSucceeds(t *testing.T) {
	build := time.Unix(1700000000, 0)
	var sane atomic.Bool
	c := ClockCheck{
		Now: func() time.Time {
			if sane.Load() {
				return build.Add(time.Hour)
			}
			return build.Add(-time.Hour)
		},
		BuildTime: build,
		Poll:      time.Millisecond,
		Log:       testClockLog(),
	}
	go func() {
		time.Sleep(5 * time.Millisecond)
		sane.Store(true)
	}()
	if !WaitClock(context.Background(), c) {
		t.Fatal("clock did not become sane")
	}
}
