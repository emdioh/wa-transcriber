package imageupdate

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// The boot file is the whole slot choice. On the BeagleBone Black it is
// extlinux/extlinux.conf with "append … root=/dev/mmcblk0p2 …"; on the Pi 2 it
// is cmdline.txt with one line of the same form. Rewriting it is the one
// moment an image update decides which root the next boot gets, so it is done
// in one narrow way: replace exactly one root= token and keep every other
// byte.

// rootToken is the kernel argument that names the root partition.
const rootToken = "root="

// setRoot writes the boot file of l with root=<device> in place of the single
// root=<old> token it has now. Everything else stays byte-identical, including
// the line endings and the trailing newline (or its absence).
//
// The write is atomic: a temp file next to it, fsync, rename, fsync of the
// directory (the /data scheme, D9). A power cut leaves either the old file or
// the new one, never half of it — which is why the FAT boot partition is
// fsynced rather than written in place.
//
// The caller remounts the boot partition rw when it is read-only: this
// function only reports the failure (EROFS from the temp file), so nothing
// here knows about mounts.
func setRoot(bootFile, oldDev, newDev string) error {
	if oldDev == "" || newDev == "" || oldDev == newDev {
		return fmt.Errorf("imageupdate: rewriting %s: %q → %q is not a slot change", bootFile, oldDev, newDev)
	}
	f, err := os.Open(bootFile)
	if err != nil {
		return fmt.Errorf("imageupdate: open boot file: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("imageupdate: stat boot file: %w", err)
	}
	buf, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		return fmt.Errorf("imageupdate: read boot file: %w", err)
	}

	out, err := replaceRoot(buf, oldDev, newDev)
	if err != nil {
		return err
	}
	return writeAtomic(bootFile, out, fi.Mode().Perm())
}

// replaceRoot swaps exactly one root=<old> token for root=<new> in b. It
// refuses when none or more than one is present: with two roots in the file
// there is no way to know which one the bootloader keeps.
func replaceRoot(b []byte, oldDev, newDev string) ([]byte, error) {
	// The token carries the device as the bootloader spells it (/dev/x); the
	// device node the running root resolves to has to be the same partition,
	// whatever directory it is reached through.
	if oldDev != "" && !strings.HasPrefix(oldDev, "/") {
		return nil, fmt.Errorf("imageupdate: %q is not a device path", oldDev)
	}
	want := rootToken + devToken(oldDev)
	var idx []int
	for i := 0; ; {
		j := bytes.Index(b[i:], []byte(want))
		if j < 0 {
			break
		}
		start := i + j
		// A token ends at whitespace or at the end of the file; root=/dev/x
		// must not match root=/dev/x2.
		end := start + len(want)
		if end == len(b) || isSpace(b[end]) {
			idx = append(idx, start)
		}
		i = end
	}
	switch len(idx) {
	case 1:
	case 0:
		return nil, fmt.Errorf("imageupdate: boot file does not name %s exactly once", want)
	default:
		return nil, fmt.Errorf("imageupdate: boot file names %s %d times", want, len(idx))
	}
	at := idx[0]
	newTok := rootToken + devToken(newDev)
	out := make([]byte, 0, len(b)+len(newTok)-len(want))
	out = append(out, b[:at]...)
	out = append(out, newTok...)
	out = append(out, b[at+len(want):]...)
	return out, nil
}

// devToken is how a boot file spells a device: always the /dev/ node the
// bootloader mounts from, whatever path the running system reached the
// partition through (a test's temp file, a bind mount).
func devToken(path string) string { return "/dev/" + filepath.Base(path) }

// isSpace reports a kernel command line separator.
func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// writeAtomic replaces path with b: temp file in the same directory, fsync,
// chmod to the old mode (the temp file is 0600 until here), rename, fsync the
// directory.
func writeAtomic(path string, b []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("imageupdate: create temp boot file: %w", err)
	}
	name := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		removeQuiet(name)
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		return fail(fmt.Errorf("imageupdate: write temp boot file: %w", err))
	}
	if err := tmp.Chmod(mode); err != nil {
		return fail(fmt.Errorf("imageupdate: chmod temp boot file: %w", err))
	}
	if err := tmp.Sync(); err != nil {
		return fail(fmt.Errorf("imageupdate: fsync temp boot file: %w", err))
	}
	if err := tmp.Close(); err != nil {
		removeQuiet(name)
		return fmt.Errorf("imageupdate: close temp boot file: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		removeQuiet(name)
		return fmt.Errorf("imageupdate: rename boot file: %w", err)
	}
	if err := SyncDir(dir); err != nil {
		return err
	}
	return nil
}

// SyncDir fsyncs a directory so a rename inside it survives a power cut. It
// is exported for main's boot-partition helper; a filesystem that cannot
// fsync a directory (some FAT setups) is not an error worth failing on,
// because the file itself was already fsynced.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return nil
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !isUnsupported(err) {
		return fmt.Errorf("imageupdate: fsync %s: %w", dir, err)
	}
	return nil
}

// bootRoot reads the root= device the boot file names, "" when it names none
// (a wiped boot file, a container image without one).
func bootRoot(bootFile string) string {
	b, err := os.ReadFile(bootFile)
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(b))
	var found string
	for _, f := range fields {
		if v, ok := strings.CutPrefix(f, rootToken); ok && v != "" {
			if found != "" {
				return "" // ambiguous: treat as absent, never guess
			}
			found = v
		}
	}
	return found
}

// isUnsupported reports whether fsync on a directory failed because the
// filesystem does not do it (EINVAL on some FAT kernel versions).
func isUnsupported(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP)
}

// removeQuiet deletes a temp file whose absence is as good as its deletion;
// the real failure is always reported by the caller as well.
func removeQuiet(path string) {
	if path != "" {
		os.Remove(path)
	}
}
