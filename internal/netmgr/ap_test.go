package netmgr

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func writeWatTxt(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "wat.txt")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func testAP(t *testing.T, r *fakeRunner, ops *fakeOps, srv *fakeAPServer) *AP {
	a := NewAP("wlan0", testMAC(), r, ops, testLogger())
	if srv != nil {
		a.StartServer = srv.start()
	}
	a.WatTxtPath = writeWatTxt(t, "setup_ap_key=secret-pass\ncountry=IT\n")
	a.HostapdConfPath = filepath.Join(t.TempDir(), "hostapd.conf")
	return a
}

func TestHostapdConfGolden(t *testing.T) {
	a := testAP(t, newFakeRunner(nil), newFakeOps(), &fakeAPServer{})
	if err := a.WriteHostapdConf(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(a.HostapdConfPath)
	if err != nil {
		t.Fatal(err)
	}
	want := readTestdata(t, "hostapd.conf")
	if string(got) != string(want) {
		t.Fatalf("hostapd.conf differs:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestSetupKeyFallbackAndCountryDefault(t *testing.T) {
	a := NewAP("wlan0", testMAC(), newFakeRunner(nil), newFakeOps(), testLogger())
	a.WatTxtPath = filepath.Join(t.TempDir(), "missing.txt")
	if got := a.SetupKey(); got != "wa-setup-001122" {
		t.Fatalf("fallback key = %q", got)
	}
	if got := a.CountryCode(); got != "IT" {
		t.Fatalf("country = %q", got)
	}
}

func TestSetupKeyTooShortFallsBack(t *testing.T) {
	a := NewAP("wlan0", testMAC(), newFakeRunner(nil), newFakeOps(), testLogger())
	a.WatTxtPath = writeWatTxt(t, "setup_ap_key=short\n")
	if got := a.SetupKey(); got != "wa-setup-001122" {
		t.Fatalf("key = %q", got)
	}
}

func TestAPStations(t *testing.T) {
	r := newFakeRunner(nil)
	ops := newFakeOps()
	ops.stations = 2
	a := testAP(t, r, ops, &fakeAPServer{})
	n, err := a.Stations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("stations = %d, want 2", n)
	}
	if got := ops.snapshot(); len(got) != 1 || got[0] != "station count wlan0" {
		t.Fatalf("ops calls = %v", got)
	}
	if got := r.snapshot(); len(got) != 0 {
		t.Fatalf("unexpected external commands for Stations: %v", got)
	}
}

func TestAPStartCommandSequence(t *testing.T) {
	r := newFakeRunner(nil)
	ops := newFakeOps()
	srv := &fakeAPServer{}
	a := testAP(t, r, ops, srv)
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	wantOps := []string{
		"addr flush dev wlan0",
		"addr add 192.168.4.1/24 dev wlan0",
		"link up wlan0",
	}
	gotOps := ops.snapshot()
	if len(gotOps) != len(wantOps) {
		t.Fatalf("ops calls = %v, want %v", gotOps, wantOps)
	}
	for i := range wantOps {
		if gotOps[i] != wantOps[i] {
			t.Errorf("ops call %d = %q, want %q", i, gotOps[i], wantOps[i])
		}
	}

	// Only hostapd should be exec'd via Runner (never ip, iw, or dnsmasq).
	gotRun := r.snapshot()
	wantRun := []string{"hostapd " + a.HostapdConfPath}
	if len(gotRun) != 1 || gotRun[0] != wantRun[0] {
		t.Fatalf("runner calls = %v, want %v", gotRun, wantRun)
	}

	cfgs := srv.snapshot()
	if len(cfgs) != 1 {
		t.Fatalf("apsrv starts = %d, want 1", len(cfgs))
	}
	if cfgs[0].IfName != "wlan0" || !cfgs[0].ServerIP.Equal(net.IPv4(192, 168, 4, 1)) {
		t.Fatalf("apsrv cfg = %+v", cfgs[0])
	}
	if ones, bits := cfgs[0].Mask.Size(); ones != 24 || bits != 32 {
		t.Fatalf("apsrv mask = %v, want /24", cfgs[0].Mask)
	}

	// Stop closes apsrv, stops hostapd, and flushes the address again.
	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if srv.closeCount() != 1 {
		t.Errorf("apsrv close count = %d, want 1", srv.closeCount())
	}
	if r.stopCount() != 1 {
		t.Errorf("hostapd stop count = %d, want 1", r.stopCount())
	}
	if last := ops.snapshot()[len(ops.snapshot())-1]; last != "addr flush dev wlan0" {
		t.Errorf("last ops call after Stop = %q", last)
	}
}

func TestAPStartCleansUpHostapdWhenServerFails(t *testing.T) {
	r := newFakeRunner(nil)
	ops := newFakeOps()
	srv := &fakeAPServer{startErr: errors.New("bind failed")}
	a := testAP(t, r, ops, srv)
	if err := a.Start(context.Background()); err == nil {
		t.Fatal("expected Start to fail when apsrv fails")
	}
	if r.stopCount() != 1 {
		t.Fatalf("hostapd stop count = %d, want 1", r.stopCount())
	}
}
