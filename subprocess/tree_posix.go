//go:build !windows

package subprocess

import (
	"os"
	"os/exec"
	"syscall"
)

// configureDetached gives teardown a tree root on POSIX (its own process
// group).
func configureDetached(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil
}

// processGroupAlive probes the detached POSIX group; only absence (ESRCH)
// reads as dead — EPERM and other failures are platform defenses reading
// alive.
func processGroupAlive(pid int) bool {
	err := syscall.Kill(-pid, 0)
	return err == nil || err == syscall.EPERM
}

// defaultSignalTree sends sig to a detached POSIX process group, falling
// back to the direct child when the group is gone. Never panics; delivery
// races process exit so failures are contained and a non-positive pid is a
// no-op. POSIX honours the tier: SIGTERM then SIGKILL.
func defaultSignalTree(pid int, signalName string, directChildExited bool) {
	if pid <= 0 {
		return
	}
	sig := signalByName(signalName)
	if err := syscall.Kill(-pid, sig); err == nil {
		return
	}
	if directChildExited {
		return
	}
	// The group is gone but the direct child may survive (EPERM-style
	// races); the swallow keeps teardown idempotent.
	_ = syscall.Kill(pid, sig)
}

func signalByName(name string) syscall.Signal {
	switch name {
	case "SIGKILL":
		return syscall.SIGKILL
	case "SIGINT":
		return syscall.SIGINT
	case "SIGHUP":
		return syscall.SIGHUP
	case "SIGQUIT":
		return syscall.SIGQUIT
	default:
		return syscall.SIGTERM
	}
}

// exitSignal extracts the terminating signal name from the process state.
func exitSignal(state *os.ProcessState) string {
	if state == nil {
		return ""
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return status.Signal().String()
	}
	return ""
}
