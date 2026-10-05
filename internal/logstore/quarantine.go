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
	"time"
)

// Quarantine purge policy (ADR-10). Quarantined files are evidence kept for
// manual repair, so by default they are kept forever and only reported (see
// DiskUsage). An operator may opt in to an age limit and/or a byte cap; both
// delete whole top-level entries (a <run>-<chunk>.zst.corrupt file or a <run>
// directory), oldest first, and every deletion is logged. Disk pressure never
// purges the quarantine.

// QuarantinePolicy limits the quarantine. Zero values keep everything.
type QuarantinePolicy struct {
	KeepFor  time.Duration // delete entries older than this
	MaxBytes int64         // then delete the oldest entries until the rest fits
}

// Enabled reports whether the policy deletes anything.
func (p QuarantinePolicy) Enabled() bool { return p.KeepFor > 0 || p.MaxBytes > 0 }

// QuarantineResult describes one purge.
type QuarantineResult struct {
	Entries, ByAge, ByBytes int
	Bytes                   int64
}

type quarantineEntry struct {
	name    string
	path    string
	size    int64
	modTime time.Time
}

// listQuarantine returns the top-level entries of the quarantine directory with
// their total size and modification time, oldest first. Unreadable entries are
// skipped; a missing directory is empty.
func listQuarantine(dir string) []quarantineEntry {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	list := make([]quarantineEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		q := quarantineEntry{name: e.Name(), path: filepath.Join(dir, e.Name()), modTime: info.ModTime()}
		if e.IsDir() {
			q.size = dirBytes(q.path)
		} else if info.Mode().IsRegular() {
			q.size = info.Size()
		}
		list = append(list, q)
	}
	slices.SortFunc(list, func(a, b quarantineEntry) int {
		if c := a.modTime.Compare(b.modTime); c != 0 {
			return c
		}
		return strings.Compare(a.name, b.name)
	})
	return list
}

// maxPurgeLogLines bounds the per-entry log lines of one purge; a summary
// always follows.
const maxPurgeLogLines = 50

// PurgeQuarantine applies the policy once. It is never silent: each deleted
// entry is logged at warn level (up to maxPurgeLogLines, then summarized) and
// the totals are kept for the diagnostics endpoint. now is passed in so tests
// control age.
func (s *Store) PurgeQuarantine(ctx context.Context, p QuarantinePolicy, now time.Time) (QuarantineResult, error) {
	var res QuarantineResult
	if !p.Enabled() {
		return res, nil
	}
	list := listQuarantine(filepath.Join(s.root, QuarantineDir))
	var total int64
	for _, q := range list {
		total += q.size
	}
	var errs []error
	logged := 0
	purge := func(q quarantineEntry, reason string) bool {
		if err := os.RemoveAll(q.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("purge quarantined %s: %w", q.name, err))
			return false
		}
		res.Entries++
		res.Bytes += q.size
		total -= q.size
		if logged < maxPurgeLogLines {
			logged++
			slog.Warn("quarantined log data purged by policy", "entry", q.name, "bytes", q.size, "age", now.Sub(q.modTime).Round(time.Second).String(), "reason", reason)
		}
		return true
	}
	remaining := list[:0:0]
	for _, q := range list {
		if err := ctx.Err(); err != nil {
			return res, errors.Join(append(errs, err)...)
		}
		if p.KeepFor > 0 && now.Sub(q.modTime) > p.KeepFor {
			if purge(q, "logs.quarantine_keep_for") {
				res.ByAge++
				continue
			}
		}
		remaining = append(remaining, q)
	}
	for _, q := range remaining {
		if p.MaxBytes <= 0 || total <= p.MaxBytes {
			break
		}
		if err := ctx.Err(); err != nil {
			return res, errors.Join(append(errs, err)...)
		}
		if purge(q, "logs.quarantine_max_size") {
			res.ByBytes++
		}
	}
	if res.Entries > 0 {
		s.quarantinePurgedEntries.Add(int64(res.Entries))
		s.quarantinePurgedBytes.Add(res.Bytes)
		slog.Warn("quarantined log data was purged by policy", "entries", res.Entries, "bytes", res.Bytes, "by_age", res.ByAge, "by_size", res.ByBytes, "unlogged_entries", res.Entries-logged)
	}
	return res, errors.Join(errs...)
}
