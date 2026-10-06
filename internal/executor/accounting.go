package executor

import (
	"os"
	"runtime"
	"syscall"

	"github.com/khanhicetea/minicrond/internal/model"
)

// exitUsage reads accounting already collected by the kernel during Wait.
// Linux reports Maxrss in KiB; Darwin reports bytes. It is not a tree peak.
func exitUsage(state *os.ProcessState) *model.ResourceUsage {
	if state == nil {
		return nil
	}
	ru, ok := state.SysUsage().(*syscall.Rusage)
	if !ok || ru == nil {
		return nil
	}
	rss := ru.Maxrss
	if runtime.GOOS == "linux" {
		rss *= 1024
	}
	return &model.ResourceUsage{
		UserCPUUS:    state.UserTime().Microseconds(),
		SystemCPUUS:  state.SystemTime().Microseconds(),
		PeakRSSBytes: rss,
	}
}
