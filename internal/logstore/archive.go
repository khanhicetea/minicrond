package logstore

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
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
// Callers own the run. Release the archive slot and the run and writer locks
// between batches so a busy worker can continue writing, readers can make
// progress, and one large checkpoint cannot hold up other runs' archival.
func (s *Store) archiveWriter(ctx context.Context, w *Writer, through int) error {
	for {
		if err := s.acquireArchiveSlot(ctx); err != nil {
			return err
		}
		lock := s.lockRun(w.runID, true)
		w.mu.Lock()
		more, err := s.archiveBatchLocked(ctx, w, through)
		w.mu.Unlock()
		s.unlockRun(w.runID, lock, true)
		<-s.archiveSlots
		if err != nil || !more {
			return err
		}
	}
}

// archiveBatchLocked commits before unlinking. Only successfully removed
// files release buffer capacity; a failed removal is retried idempotently.
// Callers own the run and hold an archive slot, the exclusive run lock, and
// w.mu.
func (s *Store) archiveBatchLocked(ctx context.Context, w *Writer, through int) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
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
	if err := s.db.PutChunks(ctx, w.runID, w.job, w.kind, time.Now(), pending); err != nil {
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
		removed++
	}
	w.idx.Chunks = slices.Delete(w.idx.Chunks, 0, removed)
	err := errors.Join(removeErr, w.writeIndex())
	more := len(w.idx.Chunks) > 0 && w.idx.Chunks[0].Number <= through
	return more, err
}

// errArchiveDB marks failures of the archive database itself, as opposed to
// problems with a buffer's own files.
var errArchiveDB = errors.New("archive database")

// archiveOrphan recovers an inactive buffer. Callers own the run and hold an
// archive slot and its exclusive run lock. Indexed chunks are copied verbatim; unindexed chunks
// are salvaged up to the last intact frame. Every committed batch is removed
// before loading the next one, including during startup recovery.
func (s *Store) archiveOrphan(ctx context.Context, runID, dir string) error {
	sealed := make(map[int]chunkMeta)
	var idx index
	b, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read orphan log index: %w", err)
	}
	if err == nil {
		if err := json.Unmarshal(b, &idx); err != nil {
			return fmt.Errorf("decode orphan log index: %w", err)
		}
		if idx.Version != int(Version) {
			return fmt.Errorf("unsupported orphan log index version %d", idx.Version)
		}
		for _, m := range idx.Chunks {
			if m.Number <= 0 || m.First == 0 || m.Last < m.First || m.Bytes < 0 || m.Raw < 0 {
				return fmt.Errorf("invalid orphan log chunk %d metadata", m.Number)
			}
			if _, exists := sealed[m.Number]; exists {
				return fmt.Errorf("duplicate orphan log chunk %d", m.Number)
			}
			sealed[m.Number] = m
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
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.db.PutChunks(ctx, runID, idx.Job, idx.Kind, time.Now(), chunks); err != nil {
			return fmt.Errorf("%w: %w", errArchiveDB, err)
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
	// A chunk that cannot be decoded at all is moved into the quarantine at
	// once, so it neither disappears (nonempty data is never deleted as if
	// recovered) nor makes the run unreadable while it waits there: readers
	// fail on an undecodable buffer file. The valid chunks around it are still
	// archived and the sweep reports the quarantine. Only if the move fails does
	// the chunk stay in place and keep failing the sweep.
	var corrupt, quarantined []error
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
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
			frames, salvageErr := salvageFrames(blob)
			if len(frames) == 0 {
				if salvageErr != nil {
					cause := fmt.Errorf("log chunk %06d has no decodable frames: %w", file.number, salvageErr)
					if err := s.preserveCorrupt(runID, file.path, blob); err != nil {
						corrupt = append(corrupt, fmt.Errorf("%w; it could not be quarantined and was kept: %w", cause, err))
						continue
					}
					if err := os.Remove(file.path); err != nil {
						corrupt = append(corrupt, fmt.Errorf("%w; its quarantine copy was made but the original could not be removed: %w", cause, err))
						continue
					}
					slog.Error("undecodable log chunk moved to the quarantine", "run", runID, "chunk", file.number, "error", salvageErr)
					quarantined = append(quarantined, fmt.Errorf("%w; the original was moved to the quarantine", cause))
				}
				// Otherwise a genuinely empty chunk: nothing to archive.
				continue
			}
			if salvageErr != nil {
				if errors.Is(salvageErr, io.ErrUnexpectedEOF) {
					// A torn tail is what a crash leaves behind.
					slog.Warn("salvaged a torn log chunk up to its last intact frame", "run", runID, "chunk", file.number, "frames", len(frames))
				} else {
					// Corruption inside the stream: keep the original bytes, since
					// the frames after the damage are not recoverable here.
					if err := s.preserveCorrupt(runID, file.path, blob); err != nil {
						corrupt = append(corrupt, fmt.Errorf("log chunk %06d is partly corrupt and could not be preserved: %w", file.number, errors.Join(salvageErr, err)))
						continue
					}
					slog.Error("log chunk is partly corrupt; the readable prefix was archived and the original preserved in the quarantine", "run", runID, "chunk", file.number, "frames", len(frames), "error", salvageErr)
				}
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
	if len(corrupt) > 0 {
		return fmt.Errorf("orphan log buffer kept: %w", errors.Join(append(corrupt, quarantined...)...))
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if len(quarantined) > 0 {
		return fmt.Errorf("%w: %w", errChunkQuarantined, errors.Join(quarantined...))
	}
	return nil
}

// errChunkQuarantined reports a sweep that archived everything decodable and
// moved undecodable chunks aside. The buffer is gone, so it does not count
// toward quarantining the whole buffer.
var errChunkQuarantined = errors.New("corrupt log chunk quarantined")

// preserveCorrupt copies a partly corrupt chunk into the quarantine before the
// readable prefix is archived and the original removed.
func (s *Store) preserveCorrupt(runID, path string, blob []byte) error {
	dir := filepath.Join(s.root, QuarantineDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, runID+"-"+filepath.Base(path)+".corrupt"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(blob)
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
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
