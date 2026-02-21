//go:build !windows

package refinery

import (
	"os"
	"os/exec"
	"syscall"
)

// refinerySignals returns the OS signals the refinery daemon handles.
func refinerySignals() []os.Signal {
	return []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1}
}

// isWakeupSignal reports whether sig is the immediate-wakeup signal.
func isWakeupSignal(sig os.Signal) bool {
	return sig == syscall.SIGUSR1
}

// setSysProcAttr configures platform-specific process attributes on cmd.
// On Unix, sets Setpgid to detach the daemon from the parent's process group.
func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
