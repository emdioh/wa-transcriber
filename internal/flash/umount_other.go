//go:build !linux

package flash

import "errors"

// unmount is not needed off Linux: the flash path only runs on the board.
func unmount(string) error { return errors.New("flash: unmount is only implemented on Linux") }
