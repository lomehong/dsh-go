//go:build windows

package subprocess

import (
	"os"
	"os/exec"
	"syscall"
)

// configureDetached is a no-op on Windows: teardown terminates the tree by
// root pid through taskkill /T instead of a process group.
func configureDetached(cmd *exec.Cmd) error {
	return nil
}

// processGroupAlive is unreachable on Windows (the direct child's exit is
// the observable boundary).
func processGroupAlive(pid int) bool { return false }

// defaultSignalTree terminates one Windows process tree with
// `taskkill /T /F`. Contained like POSIX group signalling — delivery races
// tree exit, so an absent tree (status 128), exit races, or a missing
// taskkill binary are as tolerable here as ESRCH is for a POSIX group
// signal. Any signal value force-terminates (the escalation tiers collapse).
func defaultSignalTree(pid int, signalName string, directChildExited bool) {
	if pid <= 0 {
		return
	}
	// Outcome deliberately unchecked.
	cmd := exec.Command("taskkill", "/PID", itoa(pid), "/T", "/F")
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	_ = cmd.Run()
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var digits [20]byte
	i := len(digits)
	for v > 0 {
		i--
		digits[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		digits[i] = '-'
	}
	return string(digits[i:])
}

// exitSignal: Windows processes die by exit codes, never signals.
func exitSignal(state *os.ProcessState) string {
	_ = state
	_ = syscall.Errno(0)
	return ""
}
