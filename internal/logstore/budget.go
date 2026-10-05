package logstore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Log disk budget (ADR-10, ADR-8 3A). Log disk use is bounded by two rules that
// both outrank normal retention age:
//
//   - a byte budget on the log tiers (archive database and WAL, sealed buffers,
//     live buffers), and
//   - headroom: the filesystem holding the data directory keeps a minimum of
//     free space.
//
// Reclamation deletes only completed runs' logs, oldest first: archived
// chunks, then sealed buffers awaiting archival. It never touches live
// buffers or archived chunks of runs that still have a writer, the quarantine,
// SQLite metadata or anything outside the log tiers. If that is not enough the
// pressure stays visible (DiskStatus.Insufficient): running children keep
// following the capture-failure policy (2A) and new work follows the queue
// policy.
//
// Nothing here runs on its own. EnforceDiskBudget is called from existing
// maintenance cadences and from a coalesced hint raised when a chunk rotates,
// a run seals or capture fails (SetPressureHook); the common no-pressure pass
// is one statfs.
const (
	// Reclamation starts when log bytes exceed budgetHighPercent of the budget
	// and then deletes down to budgetLowPercent, so one pass buys a stretch of
	// quiet instead of deleting a little on every check.
	budgetHighPercent = 90
	budgetLowPercent  = 80
	// Headroom pressure starts below the minimum free space and reclaims up to
	// 125% of it (the minimum plus a quarter of it).
	freeLowDivisor = 4
	// A configured headroom is clamped to a quarter of the filesystem so a
	// small disk cannot make every pass delete all history.
	freeMaxShare = 4
)

// DiskPolicy configures the budget. A zero value for a limit disables it.
type DiskPolicy struct {
	// Dir is any directory on the filesystem to protect, normally the data
	// directory; it is measured with statfs.
	Dir string
	// BudgetBytes caps the log tiers (see DiskUsage.LogBytes). Quarantine is
	// outside it and has its own policy.
	BudgetBytes int64
	// MinFreeBytes is the headroom to keep free on Dir's filesystem.
	MinFreeBytes int64
}

// DiskUsage is what the log tiers occupy on disk.
type DiskUsage struct {
	ArchiveBytes    int64 // minicron-logs.db plus its WAL
	ArchiveWALBytes int64 // the WAL part of ArchiveBytes
	SealedBytes     int64 // buffers of runs without a writer, awaiting archival
	HotBytes        int64 // buffers of runs with a live writer
	SealedRuns      int
	HotRuns         int
	// Quarantine is reported but not part of LogBytes: it is evidence kept for
	// repair and is never reclaimed by pressure.
	QuarantineBytes   int64
	QuarantineEntries int
	QuarantineOldest  time.Time // modification time of the oldest entry
	MeasuredAt        time.Time
}

// LogBytes is the footprint the byte budget applies to.
func (u DiskUsage) LogBytes() int64 { return u.ArchiveBytes + u.SealedBytes + u.HotBytes }

// DiskStatus is the outcome of the latest enforcement pass plus cumulative
// counters since the store was created.
type DiskStatus struct {
	Enabled      bool
	CheckedAt    time.Time
	FreeBytes    uint64
	TotalBytes   uint64
	StatfsError  string // set while the filesystem could not be measured
	MinFreeBytes int64  // effective headroom after clamping
	BudgetBytes  int64
	Usage        DiskUsage
	// Pressure is true when the latest pass found a watermark exceeded;
	// Insufficient when pressure remained after every eligible log was deleted.
	Pressure     bool
	Insufficient bool

	Passes, PressurePasses, InsufficientPasses int64
	// Logs deleted before their retention age because of pressure.
	PrunedRuns, PrunedChunks, PrunedBytes int64
	SealedRunsDeleted, SealedBytesDeleted int64
	LastPrunedAt                          time.Time
}

type diskState struct {
	policy atomic.Pointer[DiskPolicy]
	hook   atomic.Pointer[func()]
	// statfs reports free (to unprivileged users) and total bytes; tests
	// replace it.
	statfs func(dir string) (free, total uint64, err error)

	runMu    sync.Mutex // serializes enforcement passes
	statusMu sync.Mutex // guards status, short
	status   DiskStatus

	usageMu    sync.Mutex // coalesces on-demand usage measurements
	usage      DiskUsage
	usageValid bool
	usageAt    time.Time
}

func statfsBytes(dir string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	bsize := uint64(st.Bsize)
	return uint64(st.Bavail) * bsize, uint64(st.Blocks) * bsize, nil
}

// SetDiskPolicy installs (or, with nil, removes) the budget. It applies from
// the next enforcement pass.
func (s *Store) SetDiskPolicy(p *DiskPolicy) {
	if p == nil {
		s.disk.policy.Store(nil)
		return
	}
	cp := *p
	s.disk.policy.Store(&cp)
}

// SetPressureHook registers fn to be called, from capture paths, when disk
// pressure may have changed: a chunk rotated, a run sealed or log capture
// failed. fn must not block (a non-blocking send on a one-slot channel is the
// intended use) and must not call back into the Store. nil removes the hook.
func (s *Store) SetPressureHook(fn func()) {
	if fn == nil {
		s.disk.hook.Store(nil)
		return
	}
	s.disk.hook.Store(&fn)
}

func (s *Store) signalPressure() {
	if h := s.disk.hook.Load(); h != nil {
		(*h)()
	}
}

// DiskStatus returns the latest enforcement result.
func (s *Store) DiskStatus() DiskStatus {
	s.disk.statusMu.Lock()
	defer s.disk.statusMu.Unlock()
	return s.disk.status
}

// DiskPressure reports whether the latest pass could not bring log disk use
// back under its watermarks after deleting everything eligible. New work that
// would produce logs may use it to defer or reject.
func (s *Store) DiskPressure() bool { return s.DiskStatus().Insufficient }

func (s *Store) setDiskStatus(st DiskStatus) {
	s.disk.statusMu.Lock()
	s.disk.status = st
	s.disk.statusMu.Unlock()
}

// MeasureDiskUsage walks the log tiers. The cost is one directory listing and a
// stat per file of every run buffer still on disk (live and awaiting
// archival), plus the quarantine; archived runs have no files.
func (s *Store) MeasureDiskUsage(ctx context.Context) (DiskUsage, error) {
	u := DiskUsage{MeasuredAt: time.Now()}
	var errs []error
	if s.db != nil {
		db, wal, err := s.db.FileBytes()
		if err != nil {
			errs = append(errs, fmt.Errorf("measure log archive: %w", err))
		}
		u.ArchiveBytes, u.ArchiveWALBytes = db+wal, wal
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return u, errors.Join(append(errs, err)...)
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return u, err
		}
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(s.root, e.Name())
		if e.Name() == QuarantineDir {
			u.QuarantineBytes, u.QuarantineEntries, u.QuarantineOldest = quarantineUsage(path)
			continue
		}
		size := dirBytes(path)
		if s.Active(e.Name()) != nil {
			u.HotBytes += size
			u.HotRuns++
		} else {
			u.SealedBytes += size
			u.SealedRuns++
		}
	}
	return u, errors.Join(errs...)
}

// DiskUsageCached is MeasureDiskUsage for on-demand diagnostics: concurrent
// callers share one walk and a result younger than maxAge is reused, so a
// polling dashboard cannot turn into continuous directory scans.
func (s *Store) DiskUsageCached(ctx context.Context, maxAge time.Duration) (DiskUsage, error) {
	d := &s.disk
	d.usageMu.Lock()
	defer d.usageMu.Unlock()
	if d.usageValid && time.Since(d.usageAt) < maxAge {
		return d.usage, nil
	}
	u, err := s.MeasureDiskUsage(ctx)
	if err == nil {
		d.usage, d.usageValid, d.usageAt = u, true, time.Now()
	}
	return u, err
}

// dirBytes sums the sizes of the regular files directly in dir. Run buffers are
// flat. Files vanishing during the walk (archival) are not errors.
func dirBytes(dir string) int64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var total int64
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
	}
	return total
}

// quarantineUsage totals the quarantine: bytes under it, number of top-level
// entries and the oldest entry's modification time.
func quarantineUsage(dir string) (bytes int64, entries int, oldest time.Time) {
	for _, q := range listQuarantine(dir) {
		bytes += q.size
		entries++
		if oldest.IsZero() || q.modTime.Before(oldest) {
			oldest = q.modTime
		}
	}
	return bytes, entries, oldest
}

// reclaimProtected reports whether a run's archived chunks must not be
// reclaimed now: it has a live writer, or another operation owns it (a
// checkpoint, an archive in progress that commits in several batches, or a
// deletion). Stage 2 skips owned runs the same way, so both stages treat them alike.
func (s *Store) reclaimProtected(runID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, active := s.writers[runID]
	return active || s.owners[runID]
}

// pressure evaluates the watermarks. trigger is true when one is exceeded; want
// is how many bytes must be freed to reach the low watermarks.
func pressure(p *DiskPolicy, st *DiskStatus) (trigger bool, want int64) {
	if st.headroomLow() {
		trigger = true
		target := st.MinFreeBytes + st.MinFreeBytes/freeLowDivisor
		want = max(want, target-st.freeInt())
	}
	if p.BudgetBytes > 0 {
		used := st.Usage.LogBytes()
		if used > p.BudgetBytes/100*budgetHighPercent {
			trigger = true
			want = max(want, used-p.BudgetBytes/100*budgetLowPercent)
		}
	}
	return trigger, want
}

func (st *DiskStatus) freeInt() int64 { return int64(min(st.FreeBytes, 1<<62)) }

// headroomLow reports whether free space is below the (effective) minimum. It
// is false while the filesystem cannot be measured.
func (st *DiskStatus) headroomLow() bool {
	return st.MinFreeBytes > 0 && st.StatfsError == "" && st.freeInt() < st.MinFreeBytes
}

// probe fills the filesystem part of st. A statfs failure is recorded, not
// fatal: the byte budget still works and headroom is skipped until it recovers.
func (s *Store) probe(p *DiskPolicy, st *DiskStatus) {
	st.CheckedAt = time.Now()
	st.BudgetBytes = p.BudgetBytes
	statfs := s.disk.statfs
	if statfs == nil {
		statfs = statfsBytes
	}
	free, total, err := statfs(p.Dir)
	if err != nil {
		st.StatfsError = err.Error()
		st.FreeBytes, st.TotalBytes, st.MinFreeBytes = 0, 0, p.MinFreeBytes
		return
	}
	st.StatfsError = ""
	st.FreeBytes, st.TotalBytes = free, total
	st.MinFreeBytes = p.MinFreeBytes
	if limit := int64(min(total, 1<<62)) / freeMaxShare; st.MinFreeBytes > limit {
		st.MinFreeBytes = limit
	}
}

// EffectiveDiskPolicy reports the installed policy as passes will apply it: the
// headroom after clamping to a quarter of the filesystem, and the filesystem's
// size. ok is false without a policy. A statfs failure is returned with the
// unclamped values, so callers can still describe the configuration.
func (s *Store) EffectiveDiskPolicy() (p DiskPolicy, totalBytes uint64, ok bool, err error) {
	cur := s.disk.policy.Load()
	if cur == nil {
		return DiskPolicy{}, 0, false, nil
	}
	p = *cur
	var st DiskStatus
	s.probe(cur, &st)
	if st.StatfsError != "" {
		return p, 0, true, errors.New(st.StatfsError)
	}
	p.MinFreeBytes = st.MinFreeBytes
	return p, st.TotalBytes, true, nil
}

// EnforceDiskBudget runs one pass: it measures, and if a watermark is exceeded
// it deletes the oldest eligible logs until the low watermarks are met or
// nothing eligible is left. It is safe to call concurrently; passes are
// serialized. A canceled ctx stops reclamation between batches.
func (s *Store) EnforceDiskBudget(ctx context.Context) (DiskStatus, error) {
	d := &s.disk
	p := d.policy.Load()
	if p == nil {
		return s.DiskStatus(), nil
	}
	d.runMu.Lock()
	defer d.runMu.Unlock()
	prev := s.DiskStatus()
	st := prev
	st.Enabled = true
	st.Pressure, st.Insufficient = false, false
	st.Passes++
	var errs []error
	defer func() { s.setDiskStatus(st) }()

	s.probe(p, &st)
	if st.StatfsError != "" && prev.StatfsError == "" {
		slog.Error("log disk headroom cannot be checked: statfs failed; only the byte budget is enforced", "dir", p.Dir, "error", st.StatfsError)
	} else if st.StatfsError == "" && prev.StatfsError != "" {
		slog.Info("log disk headroom check works again", "dir", p.Dir)
	}
	if !st.headroomLow() && p.BudgetBytes <= 0 {
		// The common idle pass: one statfs, no directory walk.
		s.finishPass(&st, prev, false)
		return st, nil
	}
	if err := s.measureInto(ctx, &st); err != nil {
		if ctx.Err() != nil {
			return st, ctx.Err()
		}
		errs = append(errs, err)
	}
	trigger, want := pressure(p, &st)
	if !trigger {
		s.finishPass(&st, prev, false)
		return st, errors.Join(errs...)
	}
	st.Pressure = true
	st.PressurePasses++

	// Stage 1: archived chunks of completed runs, oldest first.
	if s.db != nil {
		res, err := s.db.PruneOldest(ctx, want, s.reclaimProtected)
		if err != nil {
			errs = append(errs, fmt.Errorf("prune archived logs under disk pressure: %w", err))
		}
		if res.Chunks > 0 {
			// Space returns to the filesystem only after the freed pages are
			// vacuumed and the WAL truncated.
			if err := s.db.Compact(ctx); err != nil {
				errs = append(errs, fmt.Errorf("compact log archive after pressure prune: %w", err))
			}
			st.PrunedRuns += res.Runs
			st.PrunedChunks += res.Chunks
			st.PrunedBytes += res.Bytes
			st.LastPrunedAt = time.Now()
			slog.Warn("log disk pressure: deleted the oldest archived logs before their retention age",
				"runs_removed", res.Runs, "chunks", res.Chunks, "bytes", res.Bytes, "wanted_bytes", want,
				"free_bytes", st.FreeBytes, "log_bytes", st.Usage.LogBytes(), "budget_bytes", p.BudgetBytes, "min_free_bytes", st.MinFreeBytes)
		}
	}
	if ctx.Err() != nil {
		return st, errors.Join(append(errs, ctx.Err())...)
	}
	s.probe(p, &st)
	if err := s.measureInto(ctx, &st); err != nil && ctx.Err() == nil {
		errs = append(errs, err)
	}
	trigger, want = pressure(p, &st)

	// Stage 2: sealed buffers of completed runs still waiting for archival.
	if trigger {
		runs, bytes, err := s.pruneSealed(ctx, want)
		if err != nil {
			errs = append(errs, fmt.Errorf("delete sealed log buffers under disk pressure: %w", err))
		}
		if runs > 0 {
			st.SealedRunsDeleted += int64(runs)
			st.SealedBytesDeleted += bytes
			st.LastPrunedAt = time.Now()
			slog.Warn("log disk pressure: deleted sealed log buffers of completed runs before they were archived",
				"runs", runs, "bytes", bytes, "wanted_bytes", want)
		}
		if ctx.Err() != nil {
			return st, errors.Join(append(errs, ctx.Err())...)
		}
		s.probe(p, &st)
		if err := s.measureInto(ctx, &st); err != nil && ctx.Err() == nil {
			errs = append(errs, err)
		}
		trigger, _ = pressure(p, &st)
	}
	s.finishPass(&st, prev, trigger)
	return st, errors.Join(errs...)
}

// measureInto stores a fresh usage measurement in st and the shared cache.
func (s *Store) measureInto(ctx context.Context, st *DiskStatus) error {
	u, err := s.MeasureDiskUsage(ctx)
	st.Usage = u
	if err == nil {
		s.disk.usageMu.Lock()
		s.disk.usage, s.disk.usageValid, s.disk.usageAt = u, true, time.Now()
		s.disk.usageMu.Unlock()
	}
	return err
}

// finishPass records whether pressure remains and logs the transitions once,
// not on every pass of a long episode.
func (s *Store) finishPass(st *DiskStatus, prev DiskStatus, unresolved bool) {
	st.Insufficient = unresolved
	switch {
	case unresolved:
		st.InsufficientPasses++
		if !prev.Insufficient {
			slog.Error("log disk pressure could not be relieved: nothing eligible is left to delete; running jobs will lose log output, new work may be refused (see docs/adr/0010-log-disk-budget.md)",
				"free_bytes", st.FreeBytes, "min_free_bytes", st.MinFreeBytes, "log_bytes", st.Usage.LogBytes(), "budget_bytes", st.BudgetBytes,
				"hot_bytes", st.Usage.HotBytes, "sealed_bytes", st.Usage.SealedBytes, "quarantine_bytes", st.Usage.QuarantineBytes)
		}
	case prev.Insufficient:
		slog.Info("log disk pressure relieved", "free_bytes", st.FreeBytes, "log_bytes", st.Usage.LogBytes())
	}
}

// pruneSealed deletes buffers of completed runs, oldest first, until at least
// want bytes are freed. Runs with a live writer or another owner (archive in
// progress, deletion) are skipped. Deleting a buffer that is queued for
// archival is safe: the archiver finds nothing to move.
func (s *Store) pruneSealed(ctx context.Context, want int64) (runs int, freed int64, err error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return 0, 0, err
	}
	type candidate struct {
		id      string
		modTime time.Time
	}
	var candidates []candidate
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || s.Active(e.Name()) != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		candidates = append(candidates, candidate{e.Name(), info.ModTime()})
	}
	slices.SortFunc(candidates, func(a, b candidate) int {
		if c := a.modTime.Compare(b.modTime); c != 0 {
			return c
		}
		return strings.Compare(a.id, b.id)
	})
	var errs []error
	for _, c := range candidates {
		if freed >= want {
			break
		}
		if err := ctx.Err(); err != nil {
			return runs, freed, errors.Join(append(errs, err)...)
		}
		if !s.claim(c.id) {
			continue
		}
		size, err := s.removeSealed(c.id)
		s.release(c.id)
		if err != nil {
			errs = append(errs, fmt.Errorf("run %s: %w", c.id, err))
			continue
		}
		if size >= 0 {
			runs++
			freed += size
		}
	}
	return runs, freed, errors.Join(errs...)
}

// removeSealed deletes one inactive buffer under its exclusive run lock and
// returns the bytes freed, or -1 if the run became active meanwhile. Callers
// own the run.
func (s *Store) removeSealed(runID string) (int64, error) {
	lock := s.lockRun(runID, true)
	defer s.unlockRun(runID, lock, true)
	if s.Active(runID) != nil {
		return -1, nil
	}
	dir := filepath.Join(s.root, runID)
	size := dirBytes(dir)
	if err := os.RemoveAll(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	return size, nil
}
