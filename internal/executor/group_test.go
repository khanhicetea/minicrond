package executor

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

// waitZombie waits until pid is a zombie (exited, not yet reaped).
func waitZombie(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err == nil {
			if _, rest, ok := strings.CutLast(string(b), ")"); ok && strings.Fields(rest)[0] == "Z" {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("process %d did not become a zombie", pid)
}

func TestGroupAliveIgnoresZombies(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs only")
	}
	cmd := exec.Command("true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	defer cmd.Wait()
	waitZombie(t, pgid)
	if err := syscall.Kill(-pgid, 0); err != nil {
		t.Fatalf("kill(-pgid, 0) on a zombie-only group: %v", err)
	}
	if groupAlive(pgid) {
		t.Fatal("a group whose only member is a zombie is reported alive")
	}

	live := exec.Command("sleep", "30")
	live.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := live.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { killGroup(live.Process.Pid, syscall.SIGKILL); live.Wait() }()
	if !groupAlive(live.Process.Pid) {
		t.Fatal("a running group is reported dead")
	}
}

// A stop must not wait the full grace for group members that already exited
// but are never reaped (no init reaping orphans).
func TestStopGroupDoesNotWaitForZombieMembers(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs only")
	}
	leader := exec.Command("sleep", "30")
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := leader.Process.Pid
	member := exec.Command("true")
	member.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
	if err := member.Start(); err != nil {
		killGroup(pgid, syscall.SIGKILL)
		leader.Wait()
		t.Fatal(err)
	}
	defer member.Wait()
	waitZombie(t, member.Process.Pid)
	wait := make(chan error, 1)
	go func() { wait <- leader.Wait() }()
	began := time.Now()
	stopGroup(pgid, model.Definition{Grace: 10, StopSignal: "TERM"}, wait, nil)
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("stop waited %v for a zombie group member", took)
	}
}
