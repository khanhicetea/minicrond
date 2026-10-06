package api

import (
	"context"
	"net/http"
	"os"
	"runtime"
	"runtime/metrics"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/khanhicetea/minicrond/internal/executor"
	"github.com/khanhicetea/minicrond/internal/procstats"
)

const monitorLimit = 256
const monitorTTL = 3 * time.Second

type monitoredRun struct {
	executor.ProcessTarget
	Stats *procstats.Sample `json:"stats"`
}

type monitorSnapshot struct {
	Supported  bool              `json:"supported"`
	SampledAt  time.Time         `json:"sampled_at"`
	Daemon     *procstats.Sample `json:"daemon"`
	DaemonPID  int               `json:"daemon_pid"`
	HeapBytes  uint64            `json:"heap_bytes"`
	Goroutines int               `json:"goroutines"`
	Items      []monitoredRun    `json:"items"`
	Active     int               `json:"active"`
	Truncated  bool              `json:"truncated"`
}

// A single bounded, immutable snapshot coalesces viewers for three seconds.
// No ticker, sampler goroutine, historical samples, or per-viewer state.
// One request-triggered expiry timer releases the cache after the last read.
// TryLock refuses concurrent collection rather than building a wait queue.
type monitorShare struct {
	mu       sync.Mutex
	at       time.Time
	snapshot monitorSnapshot
	expiry   *time.Timer
}

func (s *Server) monitor(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	release, err := s.gate().acquire(ctx, 1<<20, 0)
	if err != nil {
		s.writeReadFailure(w, r, err)
		return
	}
	snapshot, err := s.monitoring.get(ctx, s.exec)
	release() // slow response writers must not hold collection capacity
	if err != nil {
		s.writeReadFailure(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	extendWriteDeadline(w, responseWriteTimeout)
	writeJSON(w, http.StatusOK, snapshot)
}

func (m *monitorShare) get(ctx context.Context, ex *executor.Service) (monitorSnapshot, error) {
	if !m.mu.TryLock() {
		return monitorSnapshot{}, errReadBusy
	}
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return monitorSnapshot{}, err
	}
	now := time.Now()
	if !m.at.IsZero() && now.Sub(m.at) < monitorTTL {
		return m.snapshot, nil
	}
	targets, total := ex.MonitorTargets(monitorLimit)
	snapshot := monitorSnapshot{Supported: procstats.Supported, SampledAt: now.UTC(), DaemonPID: os.Getpid(), Items: make([]monitoredRun, 0, len(targets)), Active: total, Truncated: total > len(targets)}
	// Missing procfs/permissions and processes exiting during a read are normal:
	// null means unavailable, never fabricated zero usage.
	snapshot.Daemon, _ = procstats.Read(os.Getpid(), "")
	samples := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(samples)
	snapshot.HeapBytes = samples[0].Value.Uint64()
	snapshot.Goroutines = runtime.NumGoroutine()
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return monitorSnapshot{}, err
		}
		var stats *procstats.Sample
		if target.PID > 0 && target.StartID != "" {
			stats, _ = procstats.Read(target.PID, target.StartID)
		}
		snapshot.Items = append(snapshot.Items, monitoredRun{ProcessTarget: target, Stats: stats})
	}
	slices.SortFunc(snapshot.Items, func(a, b monitoredRun) int {
		if n := strings.Compare(a.Job, b.Job); n != 0 {
			return n
		}
		return strings.Compare(a.RunID, b.RunID)
	})
	if err := ctx.Err(); err != nil {
		return monitorSnapshot{}, err
	}
	m.at, m.snapshot = now, snapshot
	if m.expiry != nil {
		m.expiry.Stop()
	}
	m.expiry = time.AfterFunc(max(0, monitorTTL-time.Since(now)), func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.at.Equal(now) {
			m.at, m.snapshot, m.expiry = time.Time{}, monitorSnapshot{}, nil
		}
	})
	return snapshot, nil
}
