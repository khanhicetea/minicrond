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
	"sync"
	"time"

	"github.com/khanhicetea/minicrond/internal/logdb"
)

// A single oversized chunk is allowed, but neither a large buffer nor many
// concurrent completions can accumulate an entire run's blobs in memory.
const (
	archiveBatchBytes  = 8 << 20
	archiveBatchChunks = 64
)

// fileChunkMin is the compressed size from which a sealed chunk is archived
// as a file (renamed into the archive directory) instead of being copied into
// SQLite as a blob (ADR-20). Below it, one filesystem block per file would
// waste more than the copy costs; above it the copy is most of the archive's
// write volume. A full 1 MiB raw chunk of text compresses to roughly
// 90-250 KiB, so long outputs take the file path and short runs do not.
var fileChunkMin int64 = 64 << 10 // a variable only so tests can change it

// archiveRunBatch bounds how many queued short runs one archive transaction
// carries (ADR-20). With the byte and chunk bounds above it caps the memory
// and run locks one batch holds.
const archiveRunBatch = 32

// movedChunk is a file chunk whose buffer file is renamed into the archive
// directory once its row is committed. queued marks a chunk still waiting for
// its deferred seal sync; foreign one written by an earlier process.
type movedChunk struct {
	number          int
	src             string
	queued, foreign bool
}

// classifyMoves decides which file chunks need an fsync before their rows are
// committed. A deferred seal still queued (or in a running pass) stays queued
// and is synced at its archive path by the group sync, within the usual
// window; a chunk this process wrote and already synced needs nothing. Only a
// chunk left by an earlier process (after a crash or restart) has an unknown
// sync state, so it is synced now. This matters because ext4 flushes the
// disk cache on every fsync, even of a clean file.
func (s *Store) classifyMoves(moves []movedChunk) error {
	for i := range moves {
		s.syncMu.Lock()
		queued := s.unsyncedSeals[moves[i].src]
		s.syncMu.Unlock()
		if queued {
			moves[i].queued = true
			continue
		}
		info, err := os.Stat(moves[i].src)
		if err != nil {
			return err
		}
		// A coarse file clock can make a fresh chunk look older than the
		// store: that costs an extra fsync, never a missed one.
		moves[i].foreign = info.ModTime().Before(s.started)
	}
	return nil
}

// syncMoves fsyncs a batch's file chunks before database rows claim they are
// archived: with deferred seal syncs (ADR-20) they may not be on disk yet. The
// fsyncs run concurrently so they can share journal commits.
func (s *Store) syncMoves(moves []movedChunk) error {
	if err := s.classifyMoves(moves); err != nil {
		return err
	}
	var paths []string
	for _, m := range moves {
		if m.foreign {
			paths = append(paths, m.src)
		}
	}
	var mu sync.Mutex
	var errs []error
	parallelSync(paths, func(path string) {
		f, err := os.Open(path)
		if err == nil {
			err = errors.Join(s.fsync(f), f.Close())
		}
		if err != nil {
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
		}
	})
	return errors.Join(errs...)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	return errors.Join(err, d.Close())
}

// renameMove renames one file chunk into the archive directory and, if its
// deferred seal sync is still queued, queues it again at the new path.
func (s *Store) renameMove(dir string, m movedChunk) error {
	if !m.queued {
		return os.Rename(m.src, s.db.ChunkPath(filepath.Base(dir), m.number))
	}
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	if err := os.Rename(m.src, s.db.ChunkPath(filepath.Base(dir), m.number)); err != nil {
		return err
	}
	delete(s.unsyncedSeals, m.src)
	s.deferSealLocked(sealedChunk{dir: dir, number: m.number})
	return nil
}

// placeFiles moves committed file chunks into the archive directory. Their
// rows are already committed (commit before move, like commit before unlink):
// until the rename, readers find the chunk at its buffer path, and a crash
// before it leaves the buffer for the orphan sweep, which upserts the same
// row and renames again. If a rename fails (for example a data directory
// split across filesystems), the chunks still in the buffer are stored as
// blobs instead, so the buffer can be removed.
func (s *Store) placeFiles(ctx context.Context, runID, job, kind string, at time.Time, chunks []logdb.Chunk, moves []movedChunk) error {
	if len(moves) == 0 {
		return nil
	}
	dir := s.db.RunFilesDir(runID)
	err := os.MkdirAll(dir, 0o700)
	foreign := false
	if err == nil {
		for _, m := range moves {
			if err = s.renameMove(dir, m); err != nil {
				break
			}
			foreign = foreign || m.foreign
		}
	}
	if err == nil {
		// Chunks this process wrote follow the deferred durability of their
		// seal: the group sync makes the renames durable. Until then a reader
		// still finds an un-renamed chunk at its buffer path. Chunks left by an
		// earlier process were synced above and their renames are synced now.
		if !foreign {
			s.syncMu.Lock()
			s.deferSealLocked(sealedChunk{dir: dir})
			s.syncMu.Unlock()
			return nil
		}
		if err = syncDir(dir); err == nil {
			return nil
		}
	}
	slog.Warn("moving log chunks into the archive directory failed; storing them in the database instead", "run", runID, "error", err)
	src := make(map[int]string, len(moves))
	for _, m := range moves {
		src[m.number] = m.src
	}
	var blobs []logdb.Chunk
	for _, c := range chunks {
		path, ok := src[c.Number]
		if !ok {
			continue
		}
		blob, readErr := os.ReadFile(path)
		if errors.Is(readErr, fs.ErrNotExist) {
			continue // this one was moved before the failure
		}
		if readErr != nil {
			return errors.Join(err, readErr)
		}
		c.Blob, c.FileBytes = blob, 0
		blobs = append(blobs, c)
	}
	if putErr := s.db.PutChunks(ctx, runID, job, kind, at, blobs); putErr != nil {
		return fmt.Errorf("%w: %w", errArchiveDB, errors.Join(err, putErr))
	}
	return nil
}

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
	var moves []movedChunk
	var size int64
	for _, m := range w.idx.Chunks {
		if m.Number > through || len(pending) >= archiveBatchChunks {
			break
		}
		path := w.chunkPath(m.Number)
		if m.Bytes >= fileChunkMin {
			pending = append(pending, logdb.Chunk{Number: m.Number, First: m.First, Last: m.Last, RawBytes: m.Raw, FileBytes: m.Bytes})
			moves = append(moves, movedChunk{number: m.Number, src: path})
			continue
		}
		if len(pending) > 0 && size+m.Bytes > archiveBatchBytes {
			break
		}
		blob, err := os.ReadFile(path)
		if err != nil {
			return false, err
		}
		pending = append(pending, logdb.Chunk{Number: m.Number, First: m.First, Last: m.Last, RawBytes: m.Raw, Blob: blob})
		size += int64(len(blob))
	}
	if len(pending) == 0 {
		return false, nil
	}
	if err := s.syncMoves(moves); err != nil {
		return false, err
	}
	at := time.Now()
	if err := s.db.PutChunks(ctx, w.runID, w.job, w.kind, at, pending); err != nil {
		return false, err
	}
	if err := s.placeFiles(ctx, w.runID, w.job, w.kind, at, pending, moves); err != nil {
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

// readOrphanIndex loads a buffer's index and its sealed chunk metadata. A
// missing index, or an empty one (an index renamed into place whose data was
// not yet synced when the OS crashed, ADR-20), means every chunk is salvaged.
func readOrphanIndex(dir string) (index, map[int]chunkMeta, error) {
	sealed := make(map[int]chunkMeta)
	var idx index
	b, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return idx, nil, fmt.Errorf("read orphan log index: %w", err)
	}
	if err != nil || len(b) == 0 {
		return idx, sealed, nil
	}
	if err := json.Unmarshal(b, &idx); err != nil {
		return idx, nil, fmt.Errorf("decode orphan log index: %w", err)
	}
	if idx.Version != int(Version) {
		return idx, nil, fmt.Errorf("unsupported orphan log index version %d", idx.Version)
	}
	for _, m := range idx.Chunks {
		if m.Number <= 0 || m.First == 0 || m.Last < m.First || m.Bytes < 0 || m.Raw < 0 {
			return idx, nil, fmt.Errorf("invalid orphan log chunk %d metadata", m.Number)
		}
		if _, exists := sealed[m.Number]; exists {
			return idx, nil, fmt.Errorf("duplicate orphan log chunk %d", m.Number)
		}
		sealed[m.Number] = m
	}
	return idx, sealed, nil
}

// archiveOrphan recovers an inactive buffer. Callers own the run and hold an
// archive slot and its exclusive run lock. Indexed chunks are copied verbatim
// (or, from fileChunkMin, moved into the archive directory); unindexed chunks
// are salvaged up to the last intact frame. Every committed batch is removed
// before loading the next one, including during startup recovery.
func (s *Store) archiveOrphan(ctx context.Context, runID, dir string) error {
	idx, sealed, err := readOrphanIndex(dir)
	if err != nil {
		return err
	}
	files, err := chunkFiles(dir)
	if err != nil {
		return err
	}
	var chunks []logdb.Chunk
	var paths []string
	var moves []movedChunk
	var size int64
	flush := func() error {
		if len(chunks) == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.syncMoves(moves); err != nil {
			return err
		}
		at := time.Now()
		if err := s.db.PutChunks(ctx, runID, idx.Job, idx.Kind, at, chunks); err != nil {
			return fmt.Errorf("%w: %w", errArchiveDB, err)
		}
		if err := s.placeFiles(ctx, runID, idx.Job, idx.Kind, at, chunks, moves); err != nil {
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
		moves = moves[:0]
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
		if m, ok := sealed[file.number]; ok && info.Size() >= fileChunkMin {
			if len(chunks) >= archiveBatchChunks {
				if err := flush(); err != nil {
					return err
				}
			}
			chunks = append(chunks, logdb.Chunk{Number: file.number, First: m.First, Last: m.Last, RawBytes: m.Raw, FileBytes: info.Size()})
			paths = append(paths, file.path)
			moves = append(moves, movedChunk{number: file.number, src: file.path})
			continue
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

// SweepArchiveFiles removes archived file chunks that no row references: a
// deletion that committed but crashed before unlinking leaves them (ADR-20).
// The daemon runs it once at startup, after the orphan sweep. Runs being
// archived or deleted are skipped; the next start looks again.
func (s *Store) SweepArchiveFiles(ctx context.Context) error {
	if s.db == nil {
		return nil
	}
	runs, err := os.ReadDir(s.db.FilesDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	removed := 0
	for _, r := range runs {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if !r.IsDir() || s.Active(r.Name()) != nil || !s.claim(r.Name()) {
			continue
		}
		n, err := s.sweepRunFiles(ctx, r.Name())
		s.release(r.Name())
		removed += n
		if err != nil {
			errs = append(errs, fmt.Errorf("run %s: %w", r.Name(), err))
		}
	}
	if removed > 0 {
		slog.Warn("removed archived log files left by an interrupted deletion", "files", removed)
	}
	return errors.Join(errs...)
}

func (s *Store) sweepRunFiles(ctx context.Context, runID string) (int, error) {
	lock := s.lockRun(runID, true)
	defer s.unlockRun(runID, lock, true)
	keep, err := s.db.FileChunks(ctx, runID)
	if err != nil {
		return 0, err
	}
	dir := s.db.RunFilesDir(runID)
	files, err := chunkFiles(dir)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, f := range files {
		if keep[f.number] {
			continue
		}
		if err := os.Remove(f.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return removed, err
		}
		removed++
	}
	_ = os.Remove(dir) // fails harmlessly while chunks remain
	return removed, nil
}
