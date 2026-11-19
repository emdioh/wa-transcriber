package netmgr

import (
	"bytes"
	"context"
	"os/exec"
)

// Runner executes external commands. It exists so tests can script command
// sequences without touching real binaries or hardware.
type Runner interface {
	// Run runs name with args and returns its combined output when it exits 0.
	// A non-zero exit is returned as an error carrying the output.
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	// Start launches a long-lived command. Stop terminates it; Wait blocks
	// until it exits and returns its error.
	Start(ctx context.Context, name string, args ...string) (Stop func(), Wait func() error, err error)
}

// execRunner runs real commands on the device.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.Bytes(), err
}

func (execRunner) Start(ctx context.Context, name string, args ...string) (Stop func(), Wait func() error, err error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	stop := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}
	wait := cmd.Wait
	return stop, wait, nil
}
