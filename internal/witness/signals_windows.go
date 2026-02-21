//go:build windows

package witness

import (
	"fmt"
	"os"
)

// nudgeProcess sends an immediate-wakeup signal to the given process.
// Not supported on Windows; returns an error so the caller falls back.
func nudgeProcess(_ *os.Process) error {
	return fmt.Errorf("process wakeup signals not supported on Windows")
}
