package logstore

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/logdb"
	"github.com/khanhicetea/minicrond/internal/model"
)

// fakeDisk is a filesystem of fixed capacity whose used space is the real size
// of everything under dir, so pruning changes what statfs reports.
type fakeDisk struct {
	dir      string
	capacity int64
	err      error
	calls    int
}

func (f *fakeDisk) statfs(string) (uint64, uint64, error) {
	f.calls++
	if f.err != nil {
		return 0, 0, f.err
	}
	var used int64
	_ = filepath.WalkDir(f.dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				used += info.Size()
			}
		}
		return nil
	})
	return uint64(max(f.capacity-used, 0)), uint64(f.capacity), nil
}

// budgetFixture is a data directory with an archive and a fake disk.
type budgetFixture struct {
	t    *testing.T
	s    *Store
	ldb  *logdb.LogDB
	dir  string
	disk *fakeDisk
}

func newBudgetFixture(t *testing.T) *budgetFixture {
	t.Helper()
	s, ldb, dir := newArchiveStore(t)
	disk := &fakeDisk{dir: dir, capacity: 1 << 40}
	s.disk.statfs = disk.statfs
	return &budgetFixture{t: t, s: s, ldb: ldb, dir: dir, disk: disk}
}

// payload is incompressible so a run's archived size is predictable.
func payload(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// completedRun writes size bytes in 2 frames, closes the run and so archives it
// (no background archiver is running, so Close archives inline).
func (f *budgetFixture) completedRun(id string, size int) {
	f.t.Helper()
	w, err := f.s.Open(id, "job", model.KindJob, WriterOptions{})
	if err != nil {
		f.t.Fatal(err)
	}
	for range 2 {
		if err := w.Write(Stdout, payload(size/2), 0); err != nil {
			f.t.Fatal(err)
		}
	}
	if err := f.s.Close(id); err != nil {
		f.t.Fatal(err)
	}
}

// readable reports whether the run has any frame left in any tier.
func (f *budgetFixture) readable(id string) bool {
	f.t.Helper()
	frames, err := f.s.Read(id, 0, 10)
	if err != nil {
		f.t.Fatal(err)
	}
	return len(frames) > 0
}

// compact normalizes the archive's WAL so size accounting is predictable.
func (f *budgetFixture) compact() {
	f.t.Helper()
	if err := f.ldb.Compact(f.t.Context()); err != nil {
		f.t.Fatal(err)
	}
}

func (f *budgetFixture) used() int64 {
	free, total, _ := f.disk.statfs("")
	return int64(total - free)
}

func (f *budgetFixture) policy(budget, minFree int64) {
	f.s.SetDiskPolicy(&DiskPolicy{Dir: f.dir, BudgetBytes: budget, MinFreeBytes: minFree})
}

func (f *budgetFixture) enforce() DiskStatus {
	f.t.Helper()
	st, err := f.s.EnforceDiskBudget(f.t.Context())
	if err != nil {
		f.t.Fatalf("EnforceDiskBudget: %v", err)
	}
	return st
}

// ADR-8 3A: under pressure the oldest completed runs' logs go first, long
// before any retention age, until the headroom is restored.
func TestDiskPressurePrunesOldestArchivedLogsFirst(t *testing.T) {
	logs := captureLogs(t)
	f := newBudgetFixture(t)
	ids := []string{"r1", "r2", "r3", "r4", "r5"}
	for _, id := range ids {
		f.completedRun(id, 200<<10)
	}
	f.compact()
	// Free space is 100 KiB against a 400 KiB minimum: about 400 KiB of the
	// oldest logs must go to reach the 500 KiB low watermark.
	f.disk.capacity = f.used() + 100<<10
	f.policy(0, 400<<10)

	st := f.enforce()
	if st.Pressure != true || st.Insufficient {
		t.Fatalf("status = %+v; want pressure that was relieved", st)
	}
	if st.FreeBytes < 400<<10 {
		t.Fatalf("free space after the pass = %d, below the minimum", st.FreeBytes)
	}
	// Survivors form a suffix of the age order: oldest first, never a middle run.
	deleted := 0
	for i, id := range ids {
		if !f.readable(id) {
			if i != deleted {
				t.Fatalf("%s was deleted while an older run survived", id)
			}
			deleted++
		}
	}
	if deleted < 1 || deleted > 3 || !f.readable("r5") {
		t.Fatalf("deleted %d runs (newest must survive)", deleted)
	}
	if st.PrunedRuns != int64(deleted) || st.PrunedBytes <= 0 || st.PrunedChunks <= 0 || st.LastPrunedAt.IsZero() {
		t.Fatalf("counters %+v do not match %d deleted runs", st, deleted)
	}
	if n := logs.count("before their retention age"); n != 1 {
		t.Fatalf("pressure deletion must be logged once per pass, got %d", n)
	}
}

// A pass that finds no pressure deletes nothing and does not even walk the log
// directories (one statfs only).
func TestNoPressureDeletesNothingAndIsCheap(t *testing.T) {
	f := newBudgetFixture(t)
	f.completedRun("r1", 100<<10)
	f.policy(0, 1<<20) // 1 TiB capacity: plenty free
	st := f.enforce()
	if st.Pressure || st.Insufficient || st.PrunedRuns != 0 || !f.readable("r1") {
		t.Fatalf("status = %+v", st)
	}
	if !st.Usage.MeasuredAt.IsZero() {
		t.Fatal("an idle pass walked the log tiers")
	}
	if f.disk.calls != 1 {
		t.Fatalf("statfs called %d times in an idle pass", f.disk.calls)
	}
}

// Live buffers, archived chunks of runs that still have a writer, and the
// quarantine are never reclaimed, however severe the pressure; only completed
// runs' logs are.
func TestDiskPressureProtectsActiveRunsAndQuarantine(t *testing.T) {
	f := newBudgetFixture(t)
	// The worker is the oldest archived data and still running.
	w, err := f.s.Open("worker", "w", model.KindWorker, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(Stdout, payload(100<<10), 0); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(f.s); err != nil { // archives the first chunk, run stays active
		t.Fatal(err)
	}
	if err := w.Write(Stdout, []byte("hot tail"), 0); err != nil {
		t.Fatal(err)
	}
	f.completedRun("done", 10<<10)
	quarantined := filepath.Join(f.dir, "logs", QuarantineDir, "run-000001.zst.corrupt")
	if err := os.MkdirAll(filepath.Dir(quarantined), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(quarantined, payload(50<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	f.compact()
	f.disk.capacity = f.used() // nothing free at all
	f.policy(0, 10<<20)        // effective headroom is a quarter of the disk

	st := f.enforce()
	if !st.Insufficient || !f.s.DiskPressure() {
		t.Fatalf("status = %+v; pressure cannot be relieved from protected data alone", st)
	}
	if f.readable("done") {
		t.Fatal("the only eligible run should have been reclaimed")
	}
	frames, err := f.s.Read("worker", 0, 10)
	if err != nil || len(frames) != 2 || string(frames[1].Payload) != "hot tail" {
		t.Fatalf("active run lost data: %d frames, %v", len(frames), err)
	}
	if _, err := os.Stat(quarantined); err != nil {
		t.Fatalf("quarantine was touched: %v", err)
	}
	if err := w.Write(Stdout, []byte("still writing"), 0); err != nil {
		t.Fatalf("active writer disturbed: %v", err)
	}
	_ = f.s.Close("worker")
}

// If nothing eligible is left the outcome is explicit, logged once per episode
// rather than per pass, and clears when space returns.
func TestInsufficientReclamationIsReportedOncePerEpisode(t *testing.T) {
	logs := captureLogs(t)
	f := newBudgetFixture(t)
	w, err := f.s.Open("live", "j", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer f.s.Close("live")
	if err := w.Write(Stdout, payload(100<<10), 0); err != nil {
		t.Fatal(err)
	}
	f.compact()
	f.disk.capacity = f.used() + 1<<10
	f.policy(0, 100<<20)

	for range 3 {
		st := f.enforce()
		if !st.Insufficient || !st.Pressure {
			t.Fatalf("status = %+v", st)
		}
	}
	st := f.s.DiskStatus()
	if st.InsufficientPasses != 3 || st.PressurePasses != 3 {
		t.Fatalf("counters = %+v", st)
	}
	if n := logs.count("could not be relieved"); n != 1 {
		t.Fatalf("insufficient outcome logged %d times, want once per episode", n)
	}
	f.disk.capacity = 1 << 40
	st = f.enforce()
	if st.Insufficient || st.Pressure || f.s.DiskPressure() {
		t.Fatalf("pressure persisted after space returned: %+v", st)
	}
	if logs.count("pressure relieved") != 1 {
		t.Fatal("relief was not logged")
	}
}

// The byte budget counts every tier: reclamation starts above 90% of it and
// stops at 80%.
func TestByteBudgetCountsAllTiersAndUsesWatermarks(t *testing.T) {
	f := newBudgetFixture(t)
	for i := range 6 {
		f.completedRun(fmt.Sprintf("r%d", i+1), 200<<10)
	}
	hot, err := f.s.Open("hot", "j", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer f.s.Close("hot")
	if err := hot.Write(Stdout, payload(100<<10), 0); err != nil {
		t.Fatal(err)
	}
	f.compact()
	u, err := f.s.MeasureDiskUsage(t.Context())
	if err != nil || u.ArchiveBytes < 1200<<10 || u.HotBytes < 100<<10 || u.HotRuns != 1 || u.LogBytes() != u.ArchiveBytes+u.SealedBytes+u.HotBytes {
		t.Fatalf("usage = %+v, %v", u, err)
	}
	// Just under the budget: above the 90% high watermark, below the budget.
	budget := u.LogBytes() * 100 / 95
	f.policy(budget, 0)
	st := f.enforce()
	if !st.Pressure || st.Insufficient {
		t.Fatalf("status = %+v", st)
	}
	if got := st.Usage.LogBytes(); got > budget/100*budgetHighPercent {
		t.Fatalf("log bytes %d still above the high watermark of budget %d", got, budget)
	}
	if st.PrunedRuns < 1 || f.readable("r1") != false || !f.readable("r6") || st.PrunedBytes <= 0 {
		t.Fatalf("expected the oldest runs reclaimed, newest kept: %+v", st)
	}
	// Below the high watermark nothing further happens.
	before := st.PrunedChunks
	if st := f.enforce(); st.Pressure || st.PrunedChunks != before {
		t.Fatalf("a quiet pass deleted data: %+v", st)
	}
}

// When the archive is exhausted, sealed buffers of completed runs awaiting
// archival go next, oldest first; archived data is always reclaimed before them.
func TestDiskPressureReclaimsArchiveThenSealedBuffersOldestFirst(t *testing.T) {
	f := newBudgetFixture(t)
	f.completedRun("archived", 100<<10)
	// Sealed buffers: runs finished while no archive is attached stay on disk.
	f.s.AttachDB(nil)
	for _, id := range []string{"sealed-a", "sealed-b", "sealed-c"} {
		w, err := f.s.Open(id, "j", model.KindJob, WriterOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Write(Stdout, payload(100<<10), 0); err != nil {
			t.Fatal(err)
		}
		if err := f.s.Close(id); err != nil {
			t.Fatal(err)
		}
	}
	f.s.AttachDB(f.ldb)
	// Age order is set explicitly, not left to creation timing.
	base := time.Now().Add(-time.Hour)
	for i, id := range []string{"sealed-b", "sealed-a", "sealed-c"} { // b is oldest
		at := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(filepath.Join(f.dir, "logs", id), at, at); err != nil {
			t.Fatal(err)
		}
	}
	live, err := f.s.Open("live", "j", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer f.s.Close("live")
	if err := live.Write(Stdout, []byte("live"), 0); err != nil {
		t.Fatal(err)
	}
	f.compact()
	// Headroom 140 KiB with 5 KiB free: the archived run (~100 KiB) cannot
	// restore it alone, one sealed buffer (~100 KiB) finishes the job, and the
	// newer sealed buffers must survive.
	f.disk.capacity = f.used() + 5<<10
	f.policy(0, 140<<10)

	st := f.enforce()
	if st.Insufficient {
		t.Fatalf("status = %+v", st)
	}
	if f.readable("archived") {
		t.Fatal("archived logs must be reclaimed first")
	}
	if f.readable("sealed-b") {
		t.Fatal("the oldest sealed buffer should have been deleted")
	}
	if !f.readable("sealed-a") || !f.readable("sealed-c") {
		t.Fatal("newer sealed buffers deleted although the oldest one was enough")
	}
	if st.SealedRunsDeleted != 1 || st.SealedBytesDeleted < 100<<10 {
		t.Fatalf("sealed counters = %+v", st)
	}
	if !f.readable("live") {
		t.Fatal("live run buffer was reclaimed")
	}
	// An archiver that still has the deleted buffer queued must cope.
	if err := f.s.ArchiveOrphans(); err != nil {
		t.Fatalf("sweep after pressure deletion: %v", err)
	}
}

// A statfs failure is reported, not fatal: headroom is skipped (nothing is
// deleted on a guess), the byte budget keeps working, and recovery is noticed.
func TestStatfsFailureIsReportedAndOnlyBudgetEnforced(t *testing.T) {
	logs := captureLogs(t)
	f := newBudgetFixture(t)
	for i := range 4 {
		f.completedRun(fmt.Sprintf("r%d", i+1), 200<<10)
	}
	f.compact()
	f.disk.err = errors.New("statfs: input/output error")
	f.policy(0, 1<<40)
	for range 2 {
		st := f.enforce()
		if st.StatfsError == "" || st.Pressure || st.PrunedRuns != 0 || !f.readable("r1") {
			t.Fatalf("headroom acted on an unknown disk: %+v", st)
		}
	}
	if logs.count("statfs failed") != 1 {
		t.Fatalf("statfs failure logged %d times, want once", logs.count("statfs failed"))
	}
	u, err := f.s.MeasureDiskUsage(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f.policy(u.LogBytes()*100/95, 1<<40)
	st := f.enforce()
	if st.StatfsError == "" || st.PrunedRuns == 0 || f.readable("r1") {
		t.Fatalf("byte budget not enforced while statfs fails: %+v", st)
	}
	f.disk.err = nil
	if st := f.enforce(); st.StatfsError != "" {
		t.Fatalf("statfs recovery not noticed: %+v", st)
	}
	if logs.count("check works again") != 1 {
		t.Fatal("recovery was not logged")
	}
}

// A headroom larger than the disk can hold is clamped, so a tiny filesystem
// cannot turn every pass into "delete all history".
func TestMinFreeIsClampedToAShareOfTheFilesystem(t *testing.T) {
	f := newBudgetFixture(t)
	f.completedRun("r1", 100<<10)
	f.compact()
	f.disk.capacity = 8 << 20
	f.policy(0, 512<<20)
	st := f.enforce()
	if st.MinFreeBytes != (8<<20)/freeMaxShare {
		t.Fatalf("effective min free = %d", st.MinFreeBytes)
	}
	if st.Pressure || !f.readable("r1") {
		t.Fatalf("clamped headroom still deleted logs: %+v", st)
	}
}

// With no policy the store behaves as before.
func TestEnforceWithoutPolicyDoesNothing(t *testing.T) {
	f := newBudgetFixture(t)
	f.completedRun("r1", 10<<10)
	st, err := f.s.EnforceDiskBudget(t.Context())
	if err != nil || st.Enabled || f.disk.calls != 0 || !f.readable("r1") {
		t.Fatalf("status %+v, statfs calls %d, err %v", st, f.disk.calls, err)
	}
}

func TestEnforceStopsWhenContextEnds(t *testing.T) {
	f := newBudgetFixture(t)
	for i := range 3 {
		f.completedRun(fmt.Sprintf("r%d", i+1), 100<<10)
	}
	f.compact()
	f.disk.capacity = f.used()
	f.policy(0, 10<<20)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.s.EnforceDiskBudget(ctx); err == nil {
		t.Fatal("a canceled pass reported success")
	}
	if !f.readable("r1") {
		t.Fatal("a canceled pass deleted logs")
	}
}

// The hint reaches the daemon from the capture paths and only from them.
func TestPressureHookFiresOnRotateSealAndCaptureFailure(t *testing.T) {
	setCaptureRetry(t, time.Hour, time.Hour)
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var hints int
	s.SetPressureHook(func() { hints++ })
	w, err := s.Open("run", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	opened := hints
	if opened == 0 {
		t.Fatal("opening a chunk raised no hint")
	}
	for range 3 { // three ~1 MiB frames force a rotation
		if err := w.Write(Stdout, payload(600<<10), 0); err != nil {
			t.Fatal(err)
		}
	}
	rotated := hints
	if rotated <= opened {
		t.Fatal("chunk rotation raised no hint")
	}
	breakStorage(t, w)
	w.capture(Stdout, []byte("lost"), 0)
	failed := hints
	if failed <= rotated {
		t.Fatal("capture failure raised no hint")
	}
	if err := s.Close("run"); err != nil {
		t.Fatal(err)
	}
	if hints <= failed {
		t.Fatal("sealing the run raised no hint")
	}
	s.SetPressureHook(nil)
	n := hints
	w2, err := s.Open("run2", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = w2
	if hints != n {
		t.Fatal("hook called after removal")
	}
}

// Review NIT 1: an archive in progress commits in several batches; pressure must
// not delete the batches already committed (a mid-run gap). Owned runs are
// skipped in both stages and reclaimed once released.
func TestPressureSkipsArchivedChunksOfRunsOwnedByAnotherOperation(t *testing.T) {
	f := newBudgetFixture(t)
	f.completedRun("owned", 100<<10) // oldest
	f.completedRun("other", 100<<10)
	f.compact()
	f.disk.capacity = f.used()
	f.policy(0, 10<<20)
	if !f.s.claim("owned") {
		t.Fatal("claim failed")
	}
	f.enforce()
	if !f.readable("owned") {
		t.Fatal("pressure deleted archived chunks of a run another operation owns")
	}
	if f.readable("other") {
		t.Fatal("the unowned run should have been reclaimed")
	}
	f.s.release("owned")
	f.compact()
	f.disk.capacity = f.used() // the disk is full again
	f.enforce()
	if f.readable("owned") {
		t.Fatal("a released run stays unreclaimable")
	}
}

// Review NIT 2: a pass interrupted by shutdown keeps the previous verdict, so
// the next pass does not report relief and re-log the episode's error.
func TestCanceledPassKeepsTheInsufficientVerdict(t *testing.T) {
	logs := captureLogs(t)
	f := newBudgetFixture(t)
	w, err := f.s.Open("live", "j", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer f.s.Close("live")
	if err := w.Write(Stdout, payload(100<<10), 0); err != nil {
		t.Fatal(err)
	}
	f.compact()
	f.disk.capacity = f.used() + 1<<10
	f.policy(0, 100<<20)
	if st := f.enforce(); !st.Insufficient {
		t.Fatalf("setup: %+v", st)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.s.EnforceDiskBudget(ctx); err == nil {
		t.Fatal("a canceled pass reported success")
	}
	if st := f.s.DiskStatus(); !st.Insufficient || !st.Pressure || !f.s.DiskPressure() {
		t.Fatalf("a canceled pass published %+v", st)
	}
	f.enforce()
	if n := logs.count("could not be relieved"); n != 1 {
		t.Fatalf("episode error logged %d times, want 1", n)
	}
	if logs.count("pressure relieved") != 0 {
		t.Fatal("relief logged for a pressure that never ended")
	}
}

// Review NIT 4: hidden directories other than the quarantine are not run
// buffers (the sweeps skip them), so their bytes are not counted as sealed ones
// that pressure could never reclaim.
func TestHiddenDirectoriesAreNotCountedAsSealedBuffers(t *testing.T) {
	f := newBudgetFixture(t)
	hidden := filepath.Join(f.dir, "logs", ".scratch")
	if err := os.MkdirAll(hidden, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hidden, "x"), payload(64<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	u, err := f.s.MeasureDiskUsage(t.Context())
	if err != nil || u.SealedBytes != 0 || u.SealedRuns != 0 || u.HotBytes != 0 {
		t.Fatalf("usage = %+v, %v", u, err)
	}
}
