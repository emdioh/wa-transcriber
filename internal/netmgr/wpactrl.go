package netmgr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

const (
	defaultWPACtrlTimeout = 5 * time.Second
	wpaCtrlBufSize        = 64 * 1024
	// Keep the local unixgram path comfortably under the POSIX sun_path limit
	// (108 bytes on Linux, 104 bytes on Darwin, including NUL terminator).
	maxUnixSocketPathLen = 100
)

var wpaCtrlSeq atomic.Uint64

// WPACtrl sends commands to a wpa_supplicant control interface and returns the
// raw reply payload.
type WPACtrl interface {
	Command(ctx context.Context, cmd string) ([]byte, error)
}

var _ WPACtrl = (*SocketWPACtrl)(nil)

// SocketWPACtrl implements WPACtrl directly over wpa_supplicant's Unix datagram
// control socket (<CtrlDir>/<IfName>), eliminating the need for wpa_cli.
//
// Each Command call binds a short-lived private unixgram socket for the reply
// and removes it before returning, so SocketWPACtrl is safe for concurrent use.
type SocketWPACtrl struct {
	// CtrlDir is the wpa_supplicant ctrl_interface directory (e.g. /run/wat/wpa).
	CtrlDir string
	// IfName is the wireless interface name (e.g. wlan0).
	IfName string
	// Timeout caps how long a single command waits for a reply. Zero defaults to 5s.
	Timeout time.Duration
}

// Command sends cmd to <CtrlDir>/<IfName> and returns the reply, skipping any
// unsolicited <N>... event datagrams received before the reply.
func (c *SocketWPACtrl) Command(ctx context.Context, cmd string) ([]byte, error) {
	if c.CtrlDir == "" || c.IfName == "" {
		return nil, fmt.Errorf("wpactrl: CtrlDir and IfName are required")
	}
	if cmd == "" {
		return nil, fmt.Errorf("wpactrl: empty command")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	serverPath := filepath.Join(c.CtrlDir, c.IfName)
	localPath := wpaLocalSocketPath()

	_ = os.Remove(localPath)
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: localPath, Net: "unixgram"})
	if err != nil {
		return nil, fmt.Errorf("wpactrl: listen %s: %w", localPath, err)
	}
	defer func() {
		_ = conn.Close()
		_ = os.Remove(localPath)
	}()

	if err := os.Chmod(localPath, 0o600); err != nil {
		return nil, fmt.Errorf("wpactrl: chmod %s: %w", localPath, err)
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultWPACtrlTimeout
	}
	deadline := time.Now().Add(timeout)
	usedCtxDeadline := false
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
		usedCtxDeadline = true
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("wpactrl: set deadline: %w", err)
	}

	// Unblock WriteToUnix / ReadFromUnix promptly when ctx is canceled.
	cancelDone := make(chan struct{})
	stopCancel := context.AfterFunc(ctx, func() {
		_ = conn.SetDeadline(time.Unix(1, 0))
		close(cancelDone)
	})
	defer func() {
		if !stopCancel() {
			<-cancelDone
		}
	}()

	serverAddr := &net.UnixAddr{Name: serverPath, Net: "unixgram"}
	if _, err := conn.WriteToUnix([]byte(cmd), serverAddr); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if usedCtxDeadline && errors.Is(err, os.ErrDeadlineExceeded) {
			return nil, context.DeadlineExceeded
		}
		return nil, fmt.Errorf("wpactrl: send %q to %s: %w", cmd, serverPath, err)
	}

	buf := make([]byte, wpaCtrlBufSize)
	for {
		n, _, err := conn.ReadFromUnix(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if usedCtxDeadline && errors.Is(err, os.ErrDeadlineExceeded) {
				return nil, context.DeadlineExceeded
			}
			return nil, fmt.Errorf("wpactrl: read reply for %q from %s: %w", cmd, serverPath, err)
		}
		msg := buf[:n]
		if isUnsolicitedWPAEvent(msg) {
			continue
		}
		out := make([]byte, n)
		copy(out, msg)
		return out, nil
	}
}

func wpaLocalSocketPath() string {
	dir := os.TempDir()
	name := fmt.Sprintf("wat-wpa-%d-%d.sock", os.Getpid(), wpaCtrlSeq.Add(1))
	p := filepath.Join(dir, name)
	if len(p) > maxUnixSocketPathLen && dir != "/tmp" {
		p = filepath.Join("/tmp", name)
	}
	return p
}

// isUnsolicitedWPAEvent reports whether msg is an unsolicited wpa_supplicant
// monitor event (prefixed with "<N>" or "IFNAME="), rather than a command reply.
func isUnsolicitedWPAEvent(msg []byte) bool {
	if len(msg) >= 3 && msg[0] == '<' {
		i := 1
		for i < len(msg) && msg[i] >= '0' && msg[i] <= '9' {
			i++
		}
		if i > 1 && i < len(msg) && msg[i] == '>' {
			return true
		}
	}
	return bytes.HasPrefix(msg, []byte("IFNAME="))
}
