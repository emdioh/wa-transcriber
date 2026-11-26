//go:build !linux

package sntp

import (
	"errors"
	"time"
)

func setSystemClock(time.Time) error {
	return errors.New("sntp: setting system clock is only supported on Linux")
}
