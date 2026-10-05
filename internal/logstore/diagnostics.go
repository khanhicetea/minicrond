package logstore

import (
	"context"
	"maps"
	"sync"
	"sync/atomic"
	"time"
)

// Everything here is computed on demand or recorded as a by-product of work
// that already happens: no timer, goroutine or cache runs for diagnostics, and
// nothing is retained per viewer.

// lockWaitStats records how long log writers waited for locks. Only waits that
// actually blocked are timed (the uncontended path is a TryLock), so the cost on
// the capture path is one failed-fast atomic operation.
type lockWaitStats struct {
	count, totalNS, maxNS atomic.Int64
}

func (l *lockWaitStats) observe(d time.Duration) {
	l.count.Add(1)
	l.totalNS.Add(int64(d))
	for {
		old := l.maxNS.Load()
		if int64(d) <= old || l.maxNS.CompareAndSwap(old, int64(d)) {
			return
		}
	}
}

// LockWaits summarizes writer lock waits since the store was created.
type LockWaits struct {
	Count   int64 `json:"count"`
	TotalMS int64 `json:"total_ms"`
	MaxMS   int64 `json:"max_ms"`
}

// WriterLockWaits reports how often and how long log writes (pipe lines and
// system lines) blocked on the per-run lock or the writer's own lock: behind an
// archive batch, a read page, a flush or another stream's frame.
func (s *Store) WriterLockWaits() LockWaits {
	return LockWaits{Count: s.lockWaits.count.Load(), TotalMS: s.lockWaits.totalNS.Load() / int64(time.Millisecond), MaxMS: s.lockWaits.maxNS.Load() / int64(time.Millisecond)}
}

// lockTimed takes w.mu, timing the wait only when it blocks.
func (w *Writer) lockTimed() {
	if w.mu.TryLock() {
		return
	}
	start := time.Now()
	w.mu.Lock()
	w.store.lockWaits.observe(time.Since(start))
}

// MaintenanceStat summarizes one maintenance task's runs.
type MaintenanceStat struct {
	Runs    int64     `json:"runs"`
	LastMS  int64     `json:"last_ms"`
	MaxMS   int64     `json:"max_ms"`
	TotalMS int64     `json:"total_ms"`
	LastAt  time.Time `json:"last_at"`
}

type maintenanceStats struct {
	mu    sync.Mutex
	tasks map[string]MaintenanceStat
}

// RecordMaintenance notes that a named maintenance task (retention sweep, log
// prune, worker flush, disk budget pass, ...) just ran for d. Names must be a
// small fixed set.
func (s *Store) RecordMaintenance(name string, d time.Duration) {
	ms := d.Milliseconds()
	s.maint.mu.Lock()
	defer s.maint.mu.Unlock()
	if s.maint.tasks == nil {
		s.maint.tasks = make(map[string]MaintenanceStat)
	}
	t := s.maint.tasks[name]
	t.Runs++
	t.LastMS, t.TotalMS, t.LastAt = ms, t.TotalMS+ms, time.Now()
	t.MaxMS = max(t.MaxMS, ms)
	s.maint.tasks[name] = t
}

// MaintenanceStats returns a copy of the per-task statistics.
func (s *Store) MaintenanceStats() map[string]MaintenanceStat {
	s.maint.mu.Lock()
	defer s.maint.mu.Unlock()
	return maps.Clone(s.maint.tasks)
}

// DegradedRun names a run whose capture is currently failing.
type DegradedRun struct {
	RunID         string `json:"run_id"`
	DroppedFrames int64  `json:"dropped_frames"`
	DroppedBytes  int64  `json:"dropped_bytes"`
	Error         string `json:"error,omitempty"`
}

// CaptureDiagnostics reports log-capture health (ADR-8 2A).
type CaptureDiagnostics struct {
	// FailuresTotal counts run writers that entered degraded capture since the
	// daemon started; DroppedFrames/DroppedBytes and SyncFailures total the
	// output discarded and the failed fsyncs over the same period.
	FailuresTotal int64 `json:"failures_total"`
	DroppedFrames int64 `json:"dropped_frames"`
	DroppedBytes  int64 `json:"dropped_bytes"`
	SyncFailures  int64 `json:"sync_failures"`
	ActiveWriters int   `json:"active_writers"`
	// Degraded is the number of live writers discarding output right now;
	// DegradedRuns lists at most maxDegradedRuns of them. Unavailable counts
	// writers skipped because they were busy (diagnostics never wait for them).
	Degraded     int           `json:"degraded"`
	DegradedRuns []DegradedRun `json:"degraded_runs,omitempty"`
	Unavailable  int           `json:"unavailable,omitempty"`
}

const maxDegradedRuns = 10

// CaptureDiagnostics summarizes capture health from the store's counters and
// the live writers' Capture state. It never blocks on a writer.
func (s *Store) CaptureDiagnostics() CaptureDiagnostics {
	d := CaptureDiagnostics{
		FailuresTotal: s.captureFailures.Load(),
		DroppedFrames: s.droppedFrames.Load(),
		DroppedBytes:  s.droppedBytes.Load(),
		SyncFailures:  s.syncFailures.Load(),
	}
	s.mu.Lock()
	writers := make([]*Writer, 0, len(s.writers))
	for _, w := range s.writers {
		writers = append(writers, w)
	}
	s.mu.Unlock()
	d.ActiveWriters = len(writers)
	for _, w := range writers {
		c, ok := w.captureNoWait()
		if !ok {
			d.Unavailable++
			continue
		}
		if !c.Degraded {
			continue
		}
		d.Degraded++
		if len(d.DegradedRuns) < maxDegradedRuns {
			r := DegradedRun{RunID: w.runID, DroppedFrames: c.DroppedFrames, DroppedBytes: c.DroppedBytes}
			if c.Err != nil {
				r.Error = c.Err.Error()
			}
			d.DegradedRuns = append(d.DegradedRuns, r)
		}
	}
	return d
}

// captureNoWait is Capture that gives up instead of waiting: an archive batch
// holds w.mu across a database transaction.
func (w *Writer) captureNoWait() (CaptureStatus, bool) {
	if !w.mu.TryLock() {
		return CaptureStatus{}, false
	}
	defer w.mu.Unlock()
	return CaptureStatus{Degraded: w.degraded, DroppedFrames: w.droppedFrames, DroppedBytes: w.droppedBytes, SyncFailures: w.syncFailures, Err: w.degradedErr}, true
}

// DiskDiagnostics is the disk-budget view for the diagnostics endpoint.
type DiskDiagnostics struct {
	// Policy and the latest enforcement pass.
	BudgetEnabled   bool   `json:"budget_enabled"`
	BudgetBytes     int64  `json:"budget_bytes"`
	MinFreeBytes    int64  `json:"min_free_bytes"`
	FreeBytes       uint64 `json:"free_bytes"`
	TotalBytes      uint64 `json:"total_bytes"`
	StatfsError     string `json:"statfs_error,omitempty"`
	Pressure        bool   `json:"pressure"`
	Insufficient    bool   `json:"insufficient"`
	Passes          int64  `json:"passes"`
	PressurePasses  int64  `json:"pressure_passes"`
	InsufficientAt  int64  `json:"insufficient_passes"`
	PrunedRuns      int64  `json:"pruned_runs"`
	PrunedChunks    int64  `json:"pruned_chunks"`
	PrunedBytes     int64  `json:"pruned_bytes"`
	SealedRunsLost  int64  `json:"sealed_runs_deleted"`
	SealedBytesLost int64  `json:"sealed_bytes_deleted"`
	// Footprint of each log tier, measured on demand (at most a few seconds old).
	ArchiveBytes      int64  `json:"archive_bytes"`
	ArchiveWALBytes   int64  `json:"archive_wal_bytes"`
	SealedBytes       int64  `json:"sealed_bytes"`
	SealedRuns        int    `json:"sealed_runs"`
	HotBytes          int64  `json:"hot_bytes"`
	HotRuns           int    `json:"hot_runs"`
	LogBytes          int64  `json:"log_bytes"`
	QuarantineBytes   int64  `json:"quarantine_bytes"`
	QuarantineEntries int    `json:"quarantine_entries"`
	QuarantineOldestS int64  `json:"quarantine_oldest_age_s"`
	QuarantinePurged  int64  `json:"quarantine_purged_entries"`
	QuarantinePurgedB int64  `json:"quarantine_purged_bytes"`
	UsageError        string `json:"usage_error,omitempty"`
}

// Diagnostics is the log-storage section of GET /api/v1/daemon.
type Diagnostics struct {
	Disk            DiskDiagnostics            `json:"disk"`
	Capture         CaptureDiagnostics         `json:"capture"`
	Maintenance     map[string]MaintenanceStat `json:"maintenance"`
	WriterLockWaits LockWaits                  `json:"writer_lock_waits"`
}

// usageMaxAge bounds how stale the on-demand tier measurement may be, so a
// scraper hitting the endpoint cannot cause a directory walk per request.
const usageMaxAge = 5 * time.Second

// Diagnostics gathers the log-storage observations. Footprints are measured on
// demand, shared between concurrent callers and cached for a few seconds.
func (s *Store) Diagnostics(ctx context.Context) Diagnostics {
	st := s.DiskStatus()
	u, err := s.DiskUsageCached(ctx, usageMaxAge)
	disk := DiskDiagnostics{
		BudgetEnabled: st.Enabled, BudgetBytes: st.BudgetBytes, MinFreeBytes: st.MinFreeBytes,
		FreeBytes: st.FreeBytes, TotalBytes: st.TotalBytes, StatfsError: st.StatfsError,
		Pressure: st.Pressure, Insufficient: st.Insufficient,
		Passes: st.Passes, PressurePasses: st.PressurePasses, InsufficientAt: st.InsufficientPasses,
		PrunedRuns: st.PrunedRuns, PrunedChunks: st.PrunedChunks, PrunedBytes: st.PrunedBytes,
		SealedRunsLost: st.SealedRunsDeleted, SealedBytesLost: st.SealedBytesDeleted,
		ArchiveBytes: u.ArchiveBytes, ArchiveWALBytes: u.ArchiveWALBytes,
		SealedBytes: u.SealedBytes, SealedRuns: u.SealedRuns, HotBytes: u.HotBytes, HotRuns: u.HotRuns,
		LogBytes: u.LogBytes(), QuarantineBytes: u.QuarantineBytes, QuarantineEntries: u.QuarantineEntries,
		QuarantinePurged: s.quarantinePurgedEntries.Load(), QuarantinePurgedB: s.quarantinePurgedBytes.Load(),
	}
	if !u.QuarantineOldest.IsZero() {
		disk.QuarantineOldestS = int64(time.Since(u.QuarantineOldest).Seconds())
	}
	if err != nil {
		disk.UsageError = err.Error()
	}
	return Diagnostics{Disk: disk, Capture: s.CaptureDiagnostics(), Maintenance: s.MaintenanceStats(), WriterLockWaits: s.WriterLockWaits()}
}
