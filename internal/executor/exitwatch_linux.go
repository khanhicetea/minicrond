//go:build linux

package executor

import (
	"os"

	"golang.org/x/sys/unix"
)

// exitWatch returns a pollable handle on the child, or nil when the kernel or
// runtime cannot provide one. The caller has not reaped pid yet, so the PID
// still names its child.
func exitWatch(pid int) *os.File {
	fd, err := unix.PidfdOpen(pid, unix.PIDFD_NONBLOCK)
	if err != nil {
		return nil
	}
	return os.NewFile(uintptr(fd), "pidfd")
}

// awaitExit parks the calling goroutine in the runtime poller until the child
// has exited, and closes the handle. Without it, cmd.Wait holds one OS thread
// in waitid for the child's whole life, which for a worker is the daemon's.
// It does not reap: cmd.Wait still collects the status and rusage. On any
// error it returns early and cmd.Wait blocks as before.
func awaitExit(f *os.File) {
	defer f.Close()
	rc, err := f.SyscallConn()
	if err != nil {
		return
	}
	_ = rc.Read(func(fd uintptr) bool {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PIDFD, int(fd), &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil)
		if err == unix.EAGAIN || err == unix.EINTR {
			return false
		}
		// Exited (Signo is SIGCHLD), or an error cmd.Wait should report.
		return err != nil || info.Signo != 0
	})
}
