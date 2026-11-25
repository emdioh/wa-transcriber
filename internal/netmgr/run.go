package netmgr

import (
	"context"
	"os/exec"

	"github.com/emdioh/wa-transcriber/internal/proc"
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
	out, err := proc.CombinedOutput(cmd)
	return out, err
}

func (execRunner) Start(ctx context.Context, name string, args ...string) (Stop func(), Wait func() error, err error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if err := proc.Start(cmd); err != nil {
		return nil, nil, err
	}
	stop := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}
	// Through the table like every other child: as init, a pid nobody waits
	// for is a zombie for the lifetime of the machine.
	wait := func() error { return proc.Wait(cmd) }
	return stop, wait, nil
}
