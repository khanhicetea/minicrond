package logstore

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/logdb"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/sqlite"
)

// smallFileChunks lowers the file-chunk threshold so tests stay small.
func smallFileChunks(t *testing.T, min int64) {
	t.Helper()
	old := fileChunkMin
	fileChunkMin = min
	t.Cleanup(func() { fileChunkMin = old })
}

// randomLines returns n lines that barely compress.
func randomLines(seed uint64, n int) []byte {
	rng := rand.New(rand.NewPCG(seed, 1))
	var b bytes.Buffer
	raw := make([]byte, 48)
	for range n {
		for i := range raw {
			raw[i] = byte(rng.Uint32())
		}
		b.WriteString(hex.EncodeToString(raw))
		b.WriteByte('\n')
	}
	return b.Bytes()
}

func readAll(t *testing.T, s *Store, runID string) []Frame {
	t.Helper()
	var out []Frame
	var after uint64
	for {
		frames, err := s.Read(runID, after, 5000)
		if err != nil {
			t.Fatal(err)
		}
		if len(frames) == 0 {
			return out
		}
		out = append(out, frames...)
		after = frames[len(frames)-1].Sequence
	}
}

func checkSequence(t *testing.T, frames []Frame, want int) {
	t.Helper()
	if len(frames) != want {
		t.Fatalf("got %d frames, want %d", len(frames), want)
	}
	for i, f := range frames {
		if f.Sequence != uint64(i+1) {
			t.Fatalf("frame %d has sequence %d", i, f.Sequence)
		}
	}
}

// ADR-20: a large chunk moves into the archive directory, a small one becomes
// a blob, and reads merge both.
func TestLargeChunksAreArchivedAsFiles(t *testing.T) {
	smallFileChunks(t, 4<<10)
	s, ldb, dir := newArchiveStore(t)
	w, err := s.Open("run", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Pipe(Stdout, bytes.NewReader(randomLines(1, 200))); err != nil {
		t.Fatal(err)
	}
	if err := s.Close("run"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "logs", "run")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("buffer left behind: %v", err)
	}
	var chunks []logdb.Chunk
	if err := ldb.EachChunk(t.Context(), "run", 0, func(c logdb.Chunk) (bool, error) { chunks = append(chunks, c); return true, nil }); err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 || chunks[0].FileBytes == 0 || len(chunks[0].Blob) != 0 {
		t.Fatalf("archived chunks = %d, first file bytes %d", len(chunks), chunks[0].FileBytes)
	}
	info, err := os.Stat(ldb.ChunkPath("run", 1))
	if err != nil || info.Size() != chunks[0].FileBytes {
		t.Fatalf("archive file: %v, %v", info, err)
	}
	checkSequence(t, readAll(t, s, "run"), 200)
	if err := s.Delete("run"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ldb.RunFilesDir("run")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("archive files left after deletion: %v", err)
	}
}

// Worker checkpoints archive full chunks as files and keep reads seamless
// across the archive files, blobs and the live chunk.
func TestWorkerCheckpointArchivesFullChunksAsFiles(t *testing.T) {
	s, ldb, _ := newArchiveStore(t)
	w, err := s.Open("worker", "w", model.KindWorker, WriterOptions{MaxBytes: 100 << 20})
	if err != nil {
		t.Fatal(err)
	}
	// About 2.4 MiB raw: two full 1 MiB chunks, each far above fileChunkMin.
	if err := w.Pipe(Stdout, bytes.NewReader(randomLines(2, 20000))); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(s); err != nil {
		t.Fatal(err)
	}
	files, err := ldb.FileChunks(t.Context(), "worker")
	if err != nil || len(files) < 2 {
		t.Fatalf("file chunks after checkpoint: %v, %v", files, err)
	}
	if err := w.Write(Stdout, []byte("live"), 0); err != nil {
		t.Fatal(err)
	}
	frames := readAll(t, s, "worker")
	checkSequence(t, frames, 20001)
	if string(frames[20000].Payload) != "live" {
		t.Fatalf("last frame %q", frames[20000].Payload)
	}
	if err := s.Close("worker"); err != nil {
		t.Fatal(err)
	}
	checkSequence(t, readAll(t, s, "worker"), 20001)
}

// sealedBuffer writes a sealed run buffer with no archive attached.
func sealedBuffer(t *testing.T, root, runID string, lines []byte) {
	t.Helper()
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Open(runID, "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Pipe(Stdout, bytes.NewReader(lines)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(runID); err != nil {
		t.Fatal(err)
	}
	s.SyncAll()
}

// Commit before move: a crash after the row commit but before the rename
// leaves the chunk in the buffer, where reads still find it (once), and the
// orphan sweep finishes the move.
func TestCrashBetweenFileRowAndRename(t *testing.T) {
	smallFileChunks(t, 4<<10)
	s, ldb, dir := newArchiveStore(t)
	root := filepath.Join(dir, "logs")
	sealedBuffer(t, root, "run", randomLines(3, 200))
	idx, sealed, err := readOrphanIndex(filepath.Join(root, "run"))
	if err != nil || len(sealed) != 1 {
		t.Fatalf("index %v, %v", sealed, err)
	}
	m := idx.Chunks[0]
	// A later chunk already archived as a blob: the reader must not skip
	// past the file chunk that is still in the buffer.
	tail, err := encodeFrames([]Frame{{Sequence: 201, Timestamp: time.Now(), Stream: Stdout, Payload: []byte("tail")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := ldb.PutChunks(t.Context(), "run", "job", "job", time.Now(), []logdb.Chunk{
		{Number: m.Number, First: m.First, Last: m.Last, RawBytes: m.Raw, FileBytes: m.Bytes},
		{Number: 2, First: 201, Last: 201, RawBytes: 28, Blob: tail},
	}); err != nil {
		t.Fatal(err)
	}
	checkSequence(t, readAll(t, s, "run"), 201)
	if err := s.ArchiveOrphans(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ldb.ChunkPath("run", 1)); err != nil {
		t.Fatalf("sweep did not move the chunk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "run")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("buffer left behind: %v", err)
	}
	checkSequence(t, readAll(t, s, "run"), 201)
}

// If the rename into the archive directory fails, the chunk is stored as a
// blob instead and the buffer is still released.
func TestFileChunkFallsBackToBlobWhenMoveFails(t *testing.T) {
	smallFileChunks(t, 4<<10)
	s, ldb, dir := newArchiveStore(t)
	root := filepath.Join(dir, "logs")
	sealedBuffer(t, root, "run", randomLines(4, 200))
	// A regular file where the archive directory should be.
	if err := os.WriteFile(ldb.FilesDir(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.ArchiveOrphans(); err != nil {
		t.Fatal(err)
	}
	var c logdb.Chunk
	if err := ldb.EachChunk(t.Context(), "run", 0, func(got logdb.Chunk) (bool, error) { c = got; return false, nil }); err != nil {
		t.Fatal(err)
	}
	if c.FileBytes != 0 || len(c.Blob) == 0 {
		t.Fatalf("chunk after failed move: file bytes %d, blob %d", c.FileBytes, len(c.Blob))
	}
	if _, err := os.Stat(filepath.Join(root, "run")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("buffer left behind: %v", err)
	}
	checkSequence(t, readAll(t, s, "run"), 200)
}

// Queued short runs share one archive transaction; a run with a file chunk
// or a damaged buffer takes the one-by-one path in the same batch.
func TestArchiverBatchesShortRuns(t *testing.T) {
	smallFileChunks(t, 4<<10)
	s, ldb, dir := newArchiveStore(t)
	root := filepath.Join(dir, "logs")
	var ids []string
	for i := range 5 {
		id := fmt.Sprintf("short-%d", i)
		sealedBuffer(t, root, id, []byte("one\ntwo\n"))
		ids = append(ids, id)
	}
	sealedBuffer(t, root, "large", randomLines(5, 200))
	sealedBuffer(t, root, "noindex", []byte("salvaged\n"))
	if err := os.Remove(filepath.Join(root, "noindex", "index.json")); err != nil {
		t.Fatal(err)
	}
	ids = append(ids, "large", "noindex")
	s.archiveSealedBatch(context.Background(), ids)

	rdb, err := sqlite.OpenReader(filepath.Join(dir, "minicron-logs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	archived := make(map[int64]int)
	for _, id := range ids[:5] {
		var us int64
		if err := rdb.QueryRowContext(t.Context(), "SELECT archived_us FROM log_chunks WHERE run_id=?", id).Scan(&us); err != nil {
			t.Fatal(id, err)
		}
		archived[us]++
		if frames := readAll(t, s, id); len(frames) != 2 {
			t.Fatalf("%s: %d frames", id, len(frames))
		}
	}
	if len(archived) != 1 {
		t.Fatalf("short runs archived in %d transactions, want 1", len(archived))
	}
	if files, err := ldb.FileChunks(t.Context(), "large"); err != nil || len(files) != 1 {
		t.Fatalf("large run file chunks: %v, %v", files, err)
	}
	checkSequence(t, readAll(t, s, "large"), 200)
	if frames := readAll(t, s, "noindex"); len(frames) != 1 {
		t.Fatalf("salvaged run: %d frames", len(frames))
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("buffers left: %v, %v", entries, err)
	}
}

// An index renamed into place but never synced can come back empty after an
// OS crash (ADR-20); the sweep then salvages the chunks as if unindexed.
func TestEmptyIndexIsSalvagedLikeAMissingOne(t *testing.T) {
	s, _, dir := newArchiveStore(t)
	root := filepath.Join(dir, "logs")
	sealedBuffer(t, root, "run", []byte("a\nb\nc\n"))
	if err := os.WriteFile(filepath.Join(root, "run", "index.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.ArchiveOrphans(); err != nil {
		t.Fatal(err)
	}
	checkSequence(t, readAll(t, s, "run"), 3)
}

// Files left by a deletion that crashed after its commit are removed at
// startup; referenced files stay.
func TestSweepArchiveFilesRemovesUnreferencedFiles(t *testing.T) {
	smallFileChunks(t, 4<<10)
	s, ldb, dir := newArchiveStore(t)
	sealedBuffer(t, filepath.Join(dir, "logs"), "kept", randomLines(6, 200))
	if err := s.ArchiveOrphans(); err != nil {
		t.Fatal(err)
	}
	stray := ldb.ChunkPath("deleted", 1)
	if err := os.MkdirAll(filepath.Dir(stray), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stray, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	extra := ldb.ChunkPath("kept", 9)
	if err := os.WriteFile(extra, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.SweepArchiveFiles(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ldb.RunFilesDir("deleted")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreferenced run directory kept: %v", err)
	}
	if _, err := os.Stat(extra); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreferenced chunk kept: %v", err)
	}
	checkSequence(t, readAll(t, s, "kept"), 200)
}

// The disk budget counts archived files and its pressure pass removes them.
func TestDiskUsageCountsArchivedFiles(t *testing.T) {
	smallFileChunks(t, 4<<10)
	s, ldb, dir := newArchiveStore(t)
	sealedBuffer(t, filepath.Join(dir, "logs"), "run", randomLines(7, 200))
	if err := s.ArchiveOrphans(); err != nil {
		t.Fatal(err)
	}
	u, err := s.MeasureDiskUsage(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	db, wal, err := ldb.FileBytes()
	if err != nil {
		t.Fatal(err)
	}
	if files := ldb.ChunkFileBytes(); files == 0 || u.ArchiveBytes != db+wal+files {
		t.Fatalf("archive bytes %d, want db %d + wal %d + files %d", u.ArchiveBytes, db, wal, files)
	}
}

// File chunks follow their seal's deferred durability (ADR-20): a chunk still
// waiting for the group sync is synced at its archive path by that sync, an
// already synced one is not synced again, and one left by an earlier process
// is synced before its row is committed.
func TestFileChunkSyncFollowsSealState(t *testing.T) {
	smallFileChunks(t, 4<<10)
	type syncLog struct{ paths []string }
	setup := func(t *testing.T) (*Store, *logdb.LogDB, string, *syncLog) {
		s, ldb, dir := newArchiveStore(t)
		s.SetGroupSync(time.Hour, 0)
		log := &syncLog{}
		s.syncFile = func(f *os.File) error {
			hookMu.Lock()
			log.paths = append(log.paths, f.Name())
			hookMu.Unlock()
			return f.Sync()
		}
		return s, ldb, dir, log
	}
	contains := func(paths []string, want string) bool {
		for _, p := range paths {
			if p == want {
				return true
			}
		}
		return false
	}
	t.Run("queued", func(t *testing.T) {
		s, ldb, _, log := setup(t)
		w, err := s.Open("run", "job", model.KindJob, WriterOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Pipe(Stdout, bytes.NewReader(randomLines(8, 200))); err != nil {
			t.Fatal(err)
		}
		if err := s.Close("run"); err != nil { // archives inline, before any group sync
			t.Fatal(err)
		}
		if len(log.paths) != 0 {
			t.Fatalf("synced before the group sync: %v", log.paths)
		}
		s.SyncAll()
		if !contains(log.paths, ldb.ChunkPath("run", 1)) {
			t.Fatalf("group sync did not sync the archived chunk: %v", log.paths)
		}
		s.syncMu.Lock()
		left := len(s.unsyncedSeals)
		s.syncMu.Unlock()
		if left != 0 {
			t.Fatalf("%d seals still tracked after the group sync", left)
		}
	})
	t.Run("already_synced", func(t *testing.T) {
		s, ldb, dir, log := setup(t)
		// File times come from a coarse kernel clock that can trail time.Now
		// by milliseconds; ambiguity resolves to an extra sync, never fewer.
		s.started = time.Now().Add(-time.Second)
		s.AttachDB(nil) // seal and sync now, archive later
		w, err := s.Open("run", "job", model.KindJob, WriterOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Pipe(Stdout, bytes.NewReader(randomLines(9, 200))); err != nil {
			t.Fatal(err)
		}
		if err := s.Close("run"); err != nil {
			t.Fatal(err)
		}
		s.SyncAll()
		chunk := filepath.Join(dir, "logs", "run", "000001.zst")
		if !contains(log.paths, chunk) {
			t.Fatalf("group sync skipped the sealed chunk: %v", log.paths)
		}
		log.paths = nil
		s.AttachDB(ldb)
		if err := s.ArchiveOrphans(); err != nil {
			t.Fatal(err)
		}
		if len(log.paths) != 0 {
			t.Fatalf("already synced chunk synced again: %v", log.paths)
		}
		checkSequence(t, readAll(t, s, "run"), 200)
	})
	t.Run("earlier_process", func(t *testing.T) {
		s, ldb, dir, log := setup(t)
		sealedBuffer(t, filepath.Join(dir, "logs"), "run", randomLines(10, 200))
		s.started = time.Now().Add(time.Hour) // every existing file predates this store
		if err := s.ArchiveOrphans(); err != nil {
			t.Fatal(err)
		}
		if !contains(log.paths, filepath.Join(dir, "logs", "run", "000001.zst")) {
			t.Fatalf("chunk of an earlier process archived without a sync: %v", log.paths)
		}
		checkSequence(t, readAll(t, s, "run"), 200)
		if _, err := os.Stat(ldb.ChunkPath("run", 1)); err != nil {
			t.Fatal(err)
		}
	})
}

// A batched run whose buffer cannot be removed is counted like any other
// archive failure and eventually quarantined, without deadlocking on the run
// lock the batch held.
func TestBatchedRemoveFailureQuarantinesWithoutDeadlock(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	s, _, dir := newArchiveStore(t)
	root := filepath.Join(dir, "logs")
	sealedBuffer(t, root, "stuck", []byte("a\n"))
	// An unremovable subdirectory makes RemoveAll fail while the buffer
	// itself can still be moved into the quarantine.
	sub := filepath.Join(root, "stuck", "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.Chmod(sub, 0o700)
		os.Chmod(filepath.Join(root, QuarantineDir, "stuck", "sub"), 0o700)
	})
	// The batch's own failure is the one that reaches the quarantine.
	s.mu.Lock()
	s.orphanFailures["stuck"] = orphanQuarantineAfter - 1
	s.mu.Unlock()
	sealedBuffer(t, root, "other", []byte("b\n"))
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.archiveSealedBatch(context.Background(), []string{"stuck", "other"})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("batched archival deadlocked")
	}
	if _, err := os.Stat(filepath.Join(root, QuarantineDir, "stuck")); err != nil {
		t.Fatalf("stuck buffer not quarantined: %v", err)
	}
}
