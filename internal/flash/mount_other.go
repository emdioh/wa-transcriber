//go:build !linux

package flash

import "errors"

// mountFS is not needed off Linux: the flash path only runs on the board.
func mountFS(string, string, string) error {
	return errors.New("flash: mount is only implemented on Linux")
}
