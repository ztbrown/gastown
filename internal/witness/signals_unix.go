//go:build !windows

package witness

import (
	"os"
	"syscall"
)

// nudgeProcess sends an immediate-wakeup signal to the given process.
// On Unix this is SIGUSR1.
func nudgeProcess(proc *os.Process) error {
	return proc.Signal(syscall.SIGUSR1)
}
