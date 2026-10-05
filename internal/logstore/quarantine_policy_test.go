package logstore

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// quarantineEntry creates a quarantined file of size bytes whose age is ago.
func addQuarantined(t *testing.T, s *Store, name string, size int, ago time.Duration) string {
	t.Helper()
	dir := filepath.Join(s.root, QuarantineDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, payload(size), 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-ago)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
	return path
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// Default policy: keep everything; the quarantine is only measured.
func TestQuarantineIsKeptByDefaultAndReported(t *testing.T) {
	s, _, _ := newArchiveStore(t)
	old := addQuarantined(t, s, "run-000001.zst.corrupt", 4000, 400*24*time.Hour)
	res, err := s.PurgeQuarantine(t.Context(), QuarantinePolicy{}, time.Now())
	if err != nil || res.Entries != 0 || !exists(old) {
		t.Fatalf("zero policy purged: %+v, %v", res, err)
	}
	u, err := s.MeasureDiskUsage(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if u.QuarantineBytes != 4000 || u.QuarantineEntries != 1 || time.Since(u.QuarantineOldest) < 399*24*time.Hour {
		t.Fatalf("quarantine usage = %+v", u)
	}
	if u.LogBytes() != u.ArchiveBytes+u.SealedBytes+u.HotBytes {
		t.Fatal("quarantine must not count toward the log byte budget")
	}
}

func TestQuarantineAgePolicyPurgesOnlyOldEntriesAndLogsThem(t *testing.T) {
	logs := captureLogs(t)
	s, _, _ := newArchiveStore(t)
	old := addQuarantined(t, s, "old-000001.zst.corrupt", 1000, 40*24*time.Hour)
	recent := addQuarantined(t, s, "new-000001.zst.corrupt", 1000, 24*time.Hour)
	// A whole quarantined buffer directory is purged as one entry.
	dir := filepath.Join(s.root, QuarantineDir, "buffer")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "000001.zst"), payload(500), 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-60 * 24 * time.Hour)
	if err := os.Chtimes(dir, at, at); err != nil {
		t.Fatal(err)
	}
	res, err := s.PurgeQuarantine(t.Context(), QuarantinePolicy{KeepFor: 30 * 24 * time.Hour}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if res.Entries != 2 || res.ByAge != 2 || res.ByBytes != 0 || res.Bytes != 1500 {
		t.Fatalf("result = %+v", res)
	}
	if exists(old) || exists(dir) || !exists(recent) {
		t.Fatal("age policy removed the wrong entries")
	}
	if logs.count("purged by policy") != 3 { // two entries and the summary
		t.Fatalf("purge was not logged per entry plus a summary: %d lines", logs.count("purged by policy"))
	}
	if s.quarantinePurgedEntries.Load() != 2 || s.quarantinePurgedBytes.Load() != 1500 {
		t.Fatal("purge counters not updated")
	}
}

func TestQuarantineSizePolicyDeletesOldestFirstUntilItFits(t *testing.T) {
	s, _, _ := newArchiveStore(t)
	a := addQuarantined(t, s, "a.corrupt", 1000, 3*time.Hour)
	b := addQuarantined(t, s, "b.corrupt", 1000, 2*time.Hour)
	c := addQuarantined(t, s, "c.corrupt", 1000, time.Hour)
	res, err := s.PurgeQuarantine(t.Context(), QuarantinePolicy{MaxBytes: 1500}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if res.Entries != 2 || res.ByBytes != 2 || exists(a) || exists(b) || !exists(c) {
		t.Fatalf("result %+v; a=%v b=%v c=%v", res, exists(a), exists(b), exists(c))
	}
	// Already within the cap: nothing more.
	if res, _ := s.PurgeQuarantine(t.Context(), QuarantinePolicy{MaxBytes: 1500}, time.Now()); res.Entries != 0 {
		t.Fatalf("second purge: %+v", res)
	}
}

// Age and size combine: age first, then the cap over what remains.
func TestQuarantinePoliciesCombine(t *testing.T) {
	s, _, _ := newArchiveStore(t)
	ancient := addQuarantined(t, s, "ancient", 100, 90*24*time.Hour)
	older := addQuarantined(t, s, "older", 1000, 5*time.Hour)
	newer := addQuarantined(t, s, "newer", 1000, time.Hour)
	res, err := s.PurgeQuarantine(t.Context(), QuarantinePolicy{KeepFor: 30 * 24 * time.Hour, MaxBytes: 1200}, time.Now())
	if err != nil || res.ByAge != 1 || res.ByBytes != 1 || exists(ancient) || exists(older) || !exists(newer) {
		t.Fatalf("result %+v, %v", res, err)
	}
}

// Disk pressure never reclaims the quarantine, whatever the policy is.
func TestPressurePassNeverPurgesQuarantine(t *testing.T) {
	f := newBudgetFixture(t)
	q := addQuarantined(t, f.s, "x.corrupt", 100<<10, 1000*24*time.Hour)
	f.completedRun("r1", 50<<10)
	f.compact()
	f.disk.capacity = f.used()
	f.policy(1, 1<<20)
	f.enforce()
	if !exists(q) {
		t.Fatal("a pressure pass deleted quarantined evidence")
	}
}
