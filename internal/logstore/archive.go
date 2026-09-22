package logstore

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/khanhicetea/minicrond/internal/logdb"
)

// A single oversized chunk is allowed, but neither a large buffer nor many
// concurrent completions can accumulate an entire run's blobs in memory.
const (
	archiveBatchBytes  = 8 << 20
	archiveBatchChunks = 64
)

// archiveWriter archives only chunks sealed at the start of the checkpoint.
// Callers hold archiveMu. Release the run and writer locks between batches so
// a busy worker can continue writing and readers can make progress.
func (s *Store) archiveWriter(w *Writer, through int) error {
	for {
		unlock := s.lockRun(w.runID, true)
		w.mu.Lock()
		more, err := s.archiveBatchLocked(w, through)
		w.mu.Unlock()
		unlock()
		if err != nil || !more {
			return err
		}
	}
}

// archiveBatchLocked commits before unlinking. Only successfully removed
// files release buffer capacity; a failed removal is retried idempotently.
// Callers hold archiveMu, the exclusive run lock, and w.mu.
func (s *Store) archiveBatchLocked(w *Writer, through int) (bool, error) {
	var pending []logdb.Chunk
	var size int64
	for _, m := range w.idx.Chunks {
		if m.Number > through || len(pending) >= archiveBatchChunks ||
			(len(pending) > 0 && size+m.Bytes > archiveBatchBytes) {
			break
		}
		blob, err := os.ReadFile(w.chunkPath(m.Number))
		if err != nil {
			return false, err
		}
		pending = append(pending, logdb.Chunk{Number: m.Number, First: m.First, Last: m.Last, RawBytes: m.Raw, Blob: blob})
		size += int64(len(blob))
	}
	if len(pending) == 0 {
		return false, nil
	}
	if err := s.db.PutChunks(context.Background(), w.runID, w.job, w.kind, time.Now(), pending); err != nil {
		return false, err
	}
	removed := 0
	var removeErr error
	for _, c := range pending {
		if err := os.Remove(w.chunkPath(c.Number)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			removeErr = err
			break
		}
		w.buffered -= c.RawBytes
		w.discardHistoryThrough(c.Last)
		removed++
	}
	w.idx.Chunks = slices.Delete(w.idx.Chunks, 0, removed)
	err := errors.Join(removeErr, w.writeIndex())
	more := len(w.idx.Chunks) > 0 && w.idx.Chunks[0].Number <= through
	return more, err
}

// archiveOrphan recovers an inactive buffer while holding archiveMu and its
// exclusive run lock. Indexed chunks are copied verbatim; unindexed chunks
// are salvaged up to the last intact frame. Every committed batch is removed
// before loading the next one, including during startup recovery.
func (s *Store) archiveOrphan(runID, dir string) error {
	sealed := make(map[int]chunkMeta)
	var idx index
	if b, err := os.ReadFile(filepath.Join(dir, "index.json")); err == nil {
		if json.Unmarshal(b, &idx) == nil {
			for _, m := range idx.Chunks {
				sealed[m.Number] = m
			}
		}
	}
	files, err := chunkFiles(dir)
	if err != nil {
		return err
	}
	var chunks []logdb.Chunk
	var paths []string
	var size int64
	flush := func() error {
		if len(chunks) == 0 {
			return nil
		}
		if err := s.db.PutChunks(context.Background(), runID, idx.Job, idx.Kind, time.Now(), chunks); err != nil {
			return err
		}
		for _, path := range paths {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		clear(chunks)
		chunks = chunks[:0]
		paths = paths[:0]
		size = 0
		return nil
	}
	for _, file := range files {
		info, err := os.Stat(file.path)
		if err != nil {
			return err
		}
		if len(chunks) >= archiveBatchChunks || (len(chunks) > 0 && size+info.Size() > archiveBatchBytes) {
			if err := flush(); err != nil {
				return err
			}
		}
		blob, err := os.ReadFile(file.path)
		if err != nil {
			return err
		}
		var c logdb.Chunk
		if m, ok := sealed[file.number]; ok {
			c = logdb.Chunk{Number: file.number, First: m.First, Last: m.Last, RawBytes: m.Raw, Blob: blob}
		} else {
			frames := salvageFrames(blob)
			if len(frames) == 0 {
				continue
			}
			var raw int64
			for _, f := range frames {
				raw += int64(24 + len(f.Payload))
			}
			reenc, err := encodeFrames(frames)
			if err != nil {
				return err
			}
			c = logdb.Chunk{Number: file.number, First: frames[0].Sequence, Last: frames[len(frames)-1].Sequence, RawBytes: raw, Blob: reenc}
		}
		// Salvage can change compressed size; recheck the batch budget.
		if len(chunks) > 0 && size+int64(len(c.Blob)) > archiveBatchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		chunks = append(chunks, c)
		paths = append(paths, file.path)
		size += int64(len(c.Blob))
	}
	if err := flush(); err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

type chunkFile struct {
	number int
	path   string
}

func chunkFiles(dir string) ([]chunkFile, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.zst"))
	if err != nil {
		return nil, err
	}
	files := make([]chunkFile, 0, len(paths))
	for _, path := range paths {
		number, err := strconv.Atoi(strings.TrimSuffix(filepath.Base(path), ".zst"))
		if err == nil && number > 0 {
			files = append(files, chunkFile{number: number, path: path})
		}
	}
	// Zero padding is a minimum width, not a maximum: 1000000 follows 999999.
	slices.SortFunc(files, func(a, b chunkFile) int { return cmp.Compare(a.number, b.number) })
	return files, nil
}
