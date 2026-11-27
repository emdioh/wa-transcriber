package netmgr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// startFakeWPAServer binds a real unixgram socket at <ctrlDir>/<ifName> and
// dispatches received datagrams to handle until the test finishes.
func startFakeWPAServer(t *testing.T, ifName string, handle func(conn *net.UnixConn, peer *net.UnixAddr, cmd string)) string {
	t.Helper()

	ctrlDir := t.TempDir()
	sockPath := filepath.Join(ctrlDir, ifName)
	if len(sockPath) >= maxUnixSocketPathLen {
		t.Fatalf("server socket path too long (%d bytes): %s", len(sockPath), sockPath)
	}

	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sockPath, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen fake wpa_supplicant %s: %v", sockPath, err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, peer, err := conn.ReadFromUnix(buf)
			if err != nil {
				return
			}
			handle(conn, peer, string(buf[:n]))
		}
	}()

	t.Cleanup(func() {
		_ = conn.Close()
		<-done
	})

	return ctrlDir
}

func TestWPACtrlStatus(t *testing.T) {
	ctrlDir := startFakeWPAServer(t, "wlan0", func(conn *net.UnixConn, peer *net.UnixAddr, cmd string) {
		if cmd != "STATUS" {
			t.Errorf("unexpected cmd %q", cmd)
			return
		}
		fi, err := os.Stat(peer.Name)
		if err != nil {
			t.Errorf("stat client socket %s: %v", peer.Name, err)
		} else if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("client socket mode = %o, want 0600", perm)
		}
		_, _ = conn.WriteToUnix([]byte("wpa_state=COMPLETED\nssid=Casa\n"), peer)
	})

	clientDir := t.TempDir()
	t.Setenv("TMPDIR", clientDir)

	ctrl := &SocketWPACtrl{CtrlDir: ctrlDir, IfName: "wlan0"}
	out, err := ctrl.Command(context.Background(), "STATUS")
	if err != nil {
		t.Fatalf("Command(STATUS): %v", err)
	}

	state, ssid := parseWPACliStatus(out)
	if state != "COMPLETED" || ssid != "Casa" {
		t.Fatalf("parseWPACliStatus = (%q, %q), want (COMPLETED, Casa)", state, ssid)
	}

	entries, err := os.ReadDir(clientDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", clientDir, err)
	}
	if len(entries) != 0 {
		t.Fatalf("leftover client socket files in %s: %v", clientDir, entries)
	}
}

func TestWPACtrlScan(t *testing.T) {
	scanTable := readTestdata(t, "scan_results.txt")
	ctrlDir := startFakeWPAServer(t, "wlan0", func(conn *net.UnixConn, peer *net.UnixAddr, cmd string) {
		switch cmd {
		case "SCAN":
			_, _ = conn.WriteToUnix([]byte("OK\n"), peer)
		case "SCAN_RESULTS":
			_, _ = conn.WriteToUnix(scanTable, peer)
		default:
			t.Errorf("unexpected cmd %q", cmd)
		}
	})

	ctrl := &SocketWPACtrl{CtrlDir: ctrlDir, IfName: "wlan0"}
	ack, err := ctrl.Command(context.Background(), "SCAN")
	if err != nil {
		t.Fatalf("Command(SCAN): %v", err)
	}
	if string(ack) != "OK\n" {
		t.Fatalf("SCAN reply = %q, want %q", string(ack), "OK\n")
	}

	raw, err := ctrl.Command(context.Background(), "SCAN_RESULTS")
	if err != nil {
		t.Fatalf("Command(SCAN_RESULTS): %v", err)
	}

	nets := parseScanResults(raw)
	if len(nets) != 3 {
		t.Fatalf("nets = %+v, want 3 networks", nets)
	}
	if nets[0].SSID != "home-wifi" || nets[0].Signal != -45 || !nets[0].Secured {
		t.Errorf("nets[0] = %+v", nets[0])
	}
	if nets[1].SSID != "office" || nets[1].Signal != -80 || !nets[1].Secured {
		t.Errorf("nets[1] = %+v", nets[1])
	}
	if nets[2].SSID != "cafe-open" || nets[2].Signal != -90 || nets[2].Secured {
		t.Errorf("nets[2] = %+v", nets[2])
	}
}

func TestWPACtrlSkipUnsolicited(t *testing.T) {
	ctrlDir := startFakeWPAServer(t, "wlan0", func(conn *net.UnixConn, peer *net.UnixAddr, cmd string) {
		if cmd != "STATUS" {
			t.Errorf("unexpected cmd %q", cmd)
			return
		}
		_, _ = conn.WriteToUnix([]byte("<3>CTRL-EVENT-SCAN-STARTED"), peer)
		_, _ = conn.WriteToUnix([]byte("<3>CTRL-EVENT-SCAN-RESULTS"), peer)
		_, _ = conn.WriteToUnix([]byte("wpa_state=COMPLETED\nssid=Casa\n"), peer)
	})

	ctrl := &SocketWPACtrl{CtrlDir: ctrlDir, IfName: "wlan0"}
	out, err := ctrl.Command(context.Background(), "STATUS")
	if err != nil {
		t.Fatalf("Command(STATUS): %v", err)
	}

	state, ssid := parseWPACliStatus(out)
	if state != "COMPLETED" || ssid != "Casa" {
		t.Fatalf("parseWPACliStatus = (%q, %q), want (COMPLETED, Casa)", state, ssid)
	}
}

func TestWPACtrlMissingServer(t *testing.T) {
	ctrlDir := t.TempDir() // empty directory, no wlan0 socket
	clientDir := t.TempDir()
	t.Setenv("TMPDIR", clientDir)

	ctrl := &SocketWPACtrl{
		CtrlDir: ctrlDir,
		IfName:  "wlan0",
		Timeout: time.Second,
	}

	if _, err := ctrl.Command(context.Background(), "STATUS"); err == nil {
		t.Fatal("expected error for missing server socket")
	}

	entries, err := os.ReadDir(clientDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", clientDir, err)
	}
	if len(entries) != 0 {
		t.Fatalf("leftover client socket files in %s: %v", clientDir, entries)
	}
}

func TestWPACtrlCancel(t *testing.T) {
	received := make(chan struct{}, 1)
	ctrlDir := startFakeWPAServer(t, "wlan0", func(_ *net.UnixConn, _ *net.UnixAddr, _ string) {
		select {
		case received <- struct{}{}:
		default:
		}
		// Intentionally do not reply.
	})

	clientDir := t.TempDir()
	t.Setenv("TMPDIR", clientDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		<-received
		cancel()
	}()

	ctrl := &SocketWPACtrl{CtrlDir: ctrlDir, IfName: "wlan0", Timeout: 5 * time.Second}
	start := time.Now()
	_, err := ctrl.Command(ctx, "STATUS")
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed > time.Second {
		t.Fatalf("cancel took %s, expected prompt unblock", elapsed)
	}

	entries, err := os.ReadDir(clientDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", clientDir, err)
	}
	if len(entries) != 0 {
		t.Fatalf("leftover client socket files after cancel: %v", entries)
	}
}

func TestWPACtrlTimeout(t *testing.T) {
	ctrlDir := startFakeWPAServer(t, "wlan0", func(_ *net.UnixConn, _ *net.UnixAddr, _ string) {
		// Intentionally do not reply.
	})

	t.Run("ctx deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		ctrl := &SocketWPACtrl{CtrlDir: ctrlDir, IfName: "wlan0", Timeout: 5 * time.Second}
		_, err := ctrl.Command(ctx, "STATUS")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
	})

	t.Run("client timeout", func(t *testing.T) {
		ctrl := &SocketWPACtrl{CtrlDir: ctrlDir, IfName: "wlan0", Timeout: 50 * time.Millisecond}
		_, err := ctrl.Command(context.Background(), "STATUS")
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("err = %v, want os.ErrDeadlineExceeded", err)
		}
	})
}

func TestWPACtrlConcurrent(t *testing.T) {
	ctrlDir := startFakeWPAServer(t, "wlan0", func(conn *net.UnixConn, peer *net.UnixAddr, cmd string) {
		_, _ = conn.WriteToUnix([]byte("reply:"+cmd), peer)
	})

	ctrl := &SocketWPACtrl{CtrlDir: ctrlDir, IfName: "wlan0", Timeout: 2 * time.Second}

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := range workers {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			cmd := fmt.Sprintf("PING-%d", id)
			out, err := ctrl.Command(context.Background(), cmd)
			if err != nil {
				errs <- fmt.Errorf("worker %d: %w", id, err)
				return
			}
			want := "reply:" + cmd
			if string(out) != want {
				errs <- fmt.Errorf("worker %d: got %q, want %q", id, string(out), want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
