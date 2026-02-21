//go:build windows

package refinery

import (
	"os"
	"os/exec"
	"syscall"
)

// refinerySignals returns the OS signals the refinery daemon handles.
// SIGUSR1 is not available on Windows.
func refinerySignals() []os.Signal {
	return []os.Signal{syscall.SIGINT, syscall.SIGTERM}
}

// isWakeupSignal reports whether sig is the immediate-wakeup signal.
// Always false on Windows (SIGUSR1 not available).
func isWakeupSignal(_ os.Signal) bool {
	return false
}

// setSysProcAttr configures platform-specific process attributes on cmd.
// No-op on Windows (Setpgid not available).
func setSysProcAttr(_ *exec.Cmd) {}
