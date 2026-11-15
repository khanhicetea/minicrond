package logstore

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Flat buffer layout (ADR-24). A run's buffer is a set of files directly
// under the log root, named after the run:
//
//	<root>/<run>.json          index (sealed chunks, final flag, evidence)
//	<root>/<run>.json.tmp      index being replaced (crash leftover only)
//	<root>/<run>.NNNNNN.zst    chunk files
//
// A directory per run cost a mkdir, an inode and a directory block per run,
// a directory fsync per run at the group sync and about a dozen syscalls to
// remove; flat files need none of it, and one fsync of the root covers every
// run sealed in a group sync pass. In exchange nothing lists a run's files by
// itself: they are found from the index (see bufferChunks), and only the
// sweeps (orphans, disk usage, pressure pruning) list the root, as they did.

const (
	indexSuffix = ".json"
	tmpSuffix   = ".json.tmp"
	chunkSuffix = ".zst"
)

// validRunID reports whether runID can name flat buffer files: no path
// separator, no dot (the separator of the file name parts) and therefore no
// hidden name, which keeps the quarantine apart from run buffers.
func validRunID(runID string) bool {
	return runID != "" && !strings.ContainsAny(runID, `./\`)
}

func (s *Store) base(runID string) string { return filepath.Join(s.root, runID) }

func indexPath(base string) string    { return base + indexSuffix }
func indexTmpPath(base string) string { return base + tmpSuffix }
func chunkPath(base string, n int) string {
	return base + "." + fmt.Sprintf("%06d", n) + chunkSuffix
}

// bufferEntry classifies a root entry name. kind is 'i' (index), 't'
// (temporary index) or 'c' (chunk, with its number); ok is false for anything
// else, including hidden entries and directories.
func bufferEntry(name string) (runID string, kind byte, number int, ok bool) {
	runID, rest, found := strings.Cut(name, ".")
	if !found || !validRunID(runID) {
		return "", 0, 0, false
	}
	switch rest {
	case "json":
		return runID, 'i', 0, true
	case "json.tmp":
		return runID, 't', 0, true
	}
	digits, isChunk := strings.CutSuffix(rest, chunkSuffix)
	if !isChunk {
		return "", 0, 0, false
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n <= 0 {
		return "", 0, 0, false
	}
	return runID, 'c', n, true
}

// runFiles is one run's buffer as found by a root listing.
type runFiles struct {
	id         string
	chunks     []chunkFile
	index, tmp bool
	// With info: bytes of all its files, of its chunks, and the newest
	// modification time.
	bytes, chunkBytes int64
	modTime           time.Time
}

// listBuffers lists the root once and groups the buffer files by run, in run
// id order. withInfo adds sizes and modification times (an lstat per file).
// Files vanishing during the walk (archival) are skipped.
func (s *Store) listBuffers(withInfo bool) ([]*runFiles, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*runFiles)
	var runs []*runFiles
	for _, e := range entries {
		id, kind, number, ok := bufferEntry(e.Name())
		if !ok || !e.Type().IsRegular() {
			continue
		}
		var size int64
		var mod time.Time
		if withInfo {
			info, err := e.Info()
			if err != nil {
				continue
			}
			size, mod = info.Size(), info.ModTime()
		}
		r := byID[id]
		if r == nil {
			r = &runFiles{id: id}
			byID[id] = r
			runs = append(runs, r)
		}
		switch kind {
		case 'i':
			r.index = true
		case 't':
			r.tmp = true
		case 'c':
			r.chunks = append(r.chunks, chunkFile{number: number, path: filepath.Join(s.root, e.Name())})
			r.chunkBytes += size
		}
		r.bytes += size
		if mod.After(r.modTime) {
			r.modTime = mod
		}
	}
	slices.SortFunc(runs, func(a, b *runFiles) int { return strings.Compare(a.id, b.id) })
	for _, r := range runs {
		sortChunkFiles(r.chunks)
	}
	return runs, nil
}

// listRun is listBuffers for one run: every file of runID found in the root.
// Only rare paths use it (quarantining a whole buffer).
func (s *Store) listRun(runID string) (*runFiles, error) {
	runs, err := s.listBuffers(false)
	if err != nil {
		return nil, err
	}
	for _, r := range runs {
		if r.id == runID {
			return r, nil
		}
	}
	return &runFiles{id: runID}, nil
}

func sortChunkFiles(files []chunkFile) {
	// Zero padding is a minimum width, not a maximum: 1000000 follows 999999.
	slices.SortFunc(files, func(a, b chunkFile) int { return cmp.Compare(a.number, b.number) })
}

// readIndexFile loads a buffer's index. ok is false when it is missing,
// empty, unreadable or of another version; callers that only need chunk
// numbers then fall back to probing.
func readIndexFile(base string) (idx index, ok bool) {
	b, err := os.ReadFile(indexPath(base))
	if err != nil || len(b) == 0 {
		return idx, false
	}
	if json.Unmarshal(b, &idx) != nil || idx.Version != int(Version) {
		return index{}, false
	}
	return idx, true
}

// bufferChunks finds a run's chunk files without listing the root. Chunk
// numbers are consecutive per run: the index lists the sealed ones and the
// chunk open when it was written, archival and drop_old eviction remove the
// lowest numbers, and a chunk newer than the index (a crash before the next
// seal) or an abandoned empty chunk sits right next to a known one. So the
// candidates are the index's chunks and open chunk, extended by probing
// downwards and upwards until a number is missing; without a usable index,
// probing starts at 1. Each candidate is checked with a stat, since a crash
// can leave an index naming removed chunks. extra adds chunks found by a root
// listing, which a sweep passes so stray files are never missed.
//
// The index is returned when it was usable.
func bufferChunks(base string, extra []chunkFile) ([]chunkFile, *index, error) {
	idx, ok := readIndexFile(base)
	if !ok {
		files, err := bufferChunksFrom(base, nil, extra)
		return files, nil, err
	}
	files, err := bufferChunksFrom(base, &idx, extra)
	return files, &idx, err
}

// bufferChunksFrom is bufferChunks with the index already loaded (nil when
// there is none), for callers that read and validate it themselves.
func bufferChunksFrom(base string, ip *index, extra []chunkFile) ([]chunkFile, error) {
	var idx index
	if ip != nil {
		idx = *ip
	}
	known := make(map[int]bool, len(idx.Chunks)+len(extra)+1)
	for _, m := range idx.Chunks {
		if m.Number > 0 {
			known[m.Number] = true
		}
	}
	if idx.Open > 0 {
		known[idx.Open] = true
	}
	var files []chunkFile
	exists := func(n int) (bool, error) {
		_, err := os.Lstat(chunkPath(base, n))
		if err == nil {
			return true, nil
		}
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	lo, hi := 0, 0
	for n := range known {
		found, err := exists(n)
		if err != nil {
			return nil, err
		}
		if found {
			files = append(files, chunkFile{number: n, path: chunkPath(base, n)})
		}
		if lo == 0 || n < lo {
			lo = n
		}
		hi = max(hi, n)
	}
	if lo == 0 {
		lo, hi = 1, 0 // nothing known: probe upwards from chunk 1
	} else {
		for n := lo - 1; n > 0; n-- {
			found, err := exists(n)
			if err != nil {
				return nil, err
			}
			if !found {
				break
			}
			files = append(files, chunkFile{number: n, path: chunkPath(base, n)})
		}
	}
	for n := hi + 1; ; n++ {
		found, err := exists(n)
		if err != nil {
			return nil, err
		}
		if !found {
			break
		}
		files = append(files, chunkFile{number: n, path: chunkPath(base, n)})
		known[n] = true
	}
	for _, f := range extra {
		if !known[f.number] && !slices.ContainsFunc(files, func(c chunkFile) bool { return c.number == f.number }) {
			files = append(files, f)
		}
	}
	sortChunkFiles(files)
	return files, nil
}

// removeRun deletes a run's buffer: its chunk files (found by bufferChunks,
// plus extra from a listing), then the temporary index and the index, last,
// so a removal cut short by a crash still leaves an index for the files
// left. Missing files are not errors.
func removeRun(base string, extra []chunkFile) error {
	files, _, err := bufferChunks(base, extra)
	if err != nil {
		return err
	}
	for _, f := range files {
		if err := removeFile(f.path); err != nil {
			return err
		}
	}
	if err := removeFile(indexTmpPath(base)); err != nil {
		return err
	}
	return removeFile(indexPath(base))
}

func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// quarantineBuffer moves every file of a run's buffer, under the same names,
// into target, a directory per run inside the quarantine, so a quarantined
// buffer is one entry for the quarantine policy. Callers hold the run's
// exclusive lock. The root is listed so stray files are moved too; this is
// rare.
func (s *Store) quarantineBuffer(runID, target string) error {
	r, err := s.listRun(runID)
	if err != nil {
		return err
	}
	base := s.base(runID)
	paths := make([]string, 0, len(r.chunks)+2)
	for _, c := range r.chunks {
		paths = append(paths, c.path)
	}
	if r.tmp {
		paths = append(paths, indexTmpPath(base))
	}
	if r.index {
		paths = append(paths, indexPath(base))
	}
	if len(paths) == 0 {
		return fmt.Errorf("no buffer files for run %s", runID)
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}
	for _, path := range paths {
		if err := os.Rename(path, filepath.Join(target, filepath.Base(path))); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
