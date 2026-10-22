package main

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestWantPID1InitOnlyForTheDaemonAsPID1(t *testing.T) {
	// The test binary is never PID 1, so only the negative cases are reachable.
	for _, args := range [][]string{nil, {"daemon"}, {"status"}} {
		if wantPID1Init(args) {
			t.Fatalf("wantPID1Init(%v) = true outside PID 1", args)
		}
	}
}

// reapAll collects orphans and reports the daemon's own exit status, including
// a death by signal, without blocking.
func TestReapAllReportsDaemonStatusAndReapsOthers(t *testing.T) {
	orphan := exec.Command("/bin/true")
	if err := orphan.Start(); err != nil {
		t.Fatal(err)
	}
	daemon := exec.Command("/bin/sh", "-c", "exit 7")
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var code int
	exited := false
	for !exited && time.Now().Before(deadline) {
		code, exited = reapAll(daemon.Process.Pid)
		time.Sleep(10 * time.Millisecond)
	}
	if !exited || code != 7 {
		t.Fatalf("code=%d exited=%v, want 7 true", code, exited)
	}
	var status syscall.WaitStatus
	if pid, _ := syscall.Wait4(orphan.Process.Pid, &status, syscall.WNOHANG, nil); pid > 0 {
		t.Fatal("the other child was left unreaped")
	}

	killed := exec.Command("/bin/sleep", "30")
	if err := killed.Start(); err != nil {
		t.Fatal(err)
	}
	_ = killed.Process.Signal(syscall.SIGTERM)
	exited = false
	for deadline = time.Now().Add(5 * time.Second); !exited && time.Now().Before(deadline); {
		code, exited = reapAll(killed.Process.Pid)
		time.Sleep(10 * time.Millisecond)
	}
	if !exited || code != 128+int(syscall.SIGTERM) {
		t.Fatalf("signaled code=%d exited=%v", code, exited)
	}
}
