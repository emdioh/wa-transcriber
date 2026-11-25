package boot

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// mountSpec is one filesystem to have mounted.
type mountSpec struct {
	source string
	target string
	fstype string
	data   string
	flags  uintptr
}

// mountTable returns the set of mount points in a mountinfo(5) file.
//
// The mount point is the fifth field. The escaping is the kernel's: a space
// inside a path is written as \040, and the only paths this device mounts
// have none, but a test that mounts a temporary directory might.
func mountTable(path string) (map[string]bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool)
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		out[unescapeMount(fields[4])] = true
	}
	return out, nil
}

func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return r.Replace(s)
}

// mounted reports whether target is already mounted. An unreadable mount
// table is reported as "not mounted": mounting something twice is harmless
// (EBUSY below), while skipping a mount the machine needs is not.
func (o Options) mounted(target string) bool {
	tbl, err := mountTable(o.Mounts)
	if err != nil {
		return false
	}
	return tbl[target]
}

// ensureMount mounts m unless its target is already mounted.
//
// Idempotency is what makes a re-exec (an update, a reset) safe: the second
// run of Setup finds every filesystem where the first one left it.
func (o Options) ensureMount(m mountSpec) error {
	if o.mounted(m.target) {
		return nil
	}
	if err := o.MkdirAll(m.target, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("boot: %s: %w", m.target, err)
	}
	err := o.Mount(m.source, m.target, m.fstype, m.flags, m.data)
	switch {
	case err == nil:
		o.Log.Info("mounted", "device", m.source, "target", m.target, "type", m.fstype)
		return nil
	case errors.Is(err, unix.EBUSY):
		return nil
	default:
		return fmt.Errorf("boot: mounting %s on %s: %w", m.source, m.target, err)
	}
}
