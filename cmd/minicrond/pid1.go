package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// pid1ChildEnv marks the daemon started by the PID 1 init below; pid1Env set
// to "0" disables the init (for example under `docker run --init` or tini).
const (
	pid1ChildEnv = "MINICROND_INIT_CHILD"
	pid1Env      = "MINICROND_INIT"
)

// forwardedSignals reach the daemon unchanged. SIGKILL and SIGSTOP cannot be
// caught; everything else keeps its default meaning for the daemon.
var forwardedSignals = []os.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGUSR1, syscall.SIGUSR2}

// wantPID1Init reports whether this process should act as the container init:
// it is PID 1 in its namespace, runs the daemon, and was not told otherwise.
func wantPID1Init(args []string) bool {
	if os.Getpid() != 1 || os.Getenv(pid1ChildEnv) != "" || os.Getenv(pid1Env) == "0" {
		return false
	}
	return len(args) == 0 || args[0] == "daemon"
}

// runPID1Init is a minimal init for containers that start minicrond as PID 1.
// The kernel reparents every orphaned process of the PID namespace to PID 1,
// and PID 1 gets no default signal handling. Jobs that background or
// double-fork children would otherwise leave zombies that are never reaped
// and eventually exhaust the PID limit, and a plain SIGTERM could be ignored.
//
// Reaping with wait4(-1) inside the daemon would race os/exec's own Wait of
// each run, so the daemon runs as this process's only direct child instead:
// orphans reparent here, never to the daemon. This process forwards signals,
// reaps everything, and exits with the daemon's status once it is gone; the
// kernel then kills whatever is left in the namespace. It holds no other state,
// so its cost is one idle process woken only by SIGCHLD and forwarded signals.
func runPID1Init() int {
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, forwardedSignals...)
	children := make(chan os.Signal, 1)
	signal.Notify(children, syscall.SIGCHLD)
	env := append(os.Environ(), pid1ChildEnv+"=1")
	// Prefer the resolved path so the daemon keeps its process name; the
	// kernel link still works if the binary was replaced since.
	exe, err := os.Executable()
	if err != nil {
		exe = "/proc/self/exe"
	}
	proc, err := os.StartProcess(exe, os.Args, &os.ProcAttr{
		Env:   env,
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	})
	if err != nil {
		slog.Error("pid 1: starting the daemon failed", "error", err)
		return 1
	}
	daemonPID := proc.Pid
	for {
		select {
		case sig := <-sigs:
			if err := syscall.Kill(daemonPID, sig.(syscall.Signal)); err != nil && err != syscall.ESRCH {
				slog.Warn("pid 1: forwarding signal failed", "signal", sig.String(), "error", err)
			}
		case <-children:
			if code, exited := reapAll(daemonPID); exited {
				stopLeftovers(children)
				return code
			}
		}
	}
}

// leftoverGrace is how long processes that escaped the daemon's process
// groups (daemonized helpers) get after SIGTERM before PID 1 exits and the
// kernel kills them.
const leftoverGrace = 2 * time.Second

// stopLeftovers asks every remaining process in the namespace to terminate and
// reaps them for at most leftoverGrace, so they can flush their own state
// instead of being killed outright when PID 1 exits.
func stopLeftovers(children <-chan os.Signal) {
	if err := syscall.Kill(-1, syscall.SIGTERM); err == syscall.ESRCH {
		return // nothing left
	}
	deadline := time.NewTimer(leftoverGrace)
	defer deadline.Stop()
	for {
		reapAll(-1)
		if err := syscall.Kill(-1, 0); err == syscall.ESRCH {
			return
		}
		select {
		case <-children:
		case <-deadline.C:
			return
		}
	}
}

// reapAll collects every exited child without blocking. It reports the
// daemon's exit code once the daemon itself has been reaped.
func reapAll(daemonPID int) (code int, daemonExited bool) {
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if err == syscall.EINTR {
			continue
		}
		if pid <= 0 || err != nil {
			return code, daemonExited
		}
		if pid != daemonPID {
			continue // an orphan: reaping it is the whole point
		}
		daemonExited = true
		switch {
		case status.Exited():
			code = status.ExitStatus()
		case status.Signaled():
			code = 128 + int(status.Signal())
			fmt.Fprintf(os.Stderr, "minicrond: daemon terminated by %s\n", status.Signal())
		default:
			code = 1
		}
	}
}
