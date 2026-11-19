package status

import (
	"testing"
	"time"
)

func TestClockSyncedDefaultAndUpdate(t *testing.T) {
	tr := New(time.Now())
	if !tr.Snapshot().ClockSynced {
		t.Fatal("clock should be assumed synced at start")
	}
	tr.SetClockSynced(false)
	if tr.Snapshot().ClockSynced {
		t.Fatal("SetClockSynced(false) ignored")
	}
	tr.SetClockSynced(true)
	if !tr.Snapshot().ClockSynced {
		t.Fatal("SetClockSynced(true) ignored")
	}
}
