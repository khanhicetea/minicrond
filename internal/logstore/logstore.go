package logstore

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"

	"github.com/khanhicetea/minicrond/internal/logdb"
)

const Version byte = 1
const chunkLimit = 1 << 20
const maxFramePayload = 16 << 20

// The live tail is bounded by both frames and bytes. The byte limit prevents
// a handful of valid maximum-size lines from retaining gigabytes per run.
const (
	historyLimit     = 5000
	historyByteLimit = 16 << 20
)

type Stream byte

const (
	Stdout Stream = 1
	Stderr Stream = 2
	System Stream = 3
)

type Flags byte

const (
	FlagPartial Flags = 1 << iota
	FlagInvalidUTF8
	FlagTruncated
)

type Frame struct {
	Sequence  uint64    `json:"sequence"`
	Timestamp time.Time `json:"timestamp"`
	Stream    Stream    `json:"stream"`
	Flags     Flags     `json:"flags"`
	Payload   []byte    `json:"payload"`
}
type chunkMeta struct {
	Number   int    `json:"number"`
	First    uint64 `json:"first"`
	Last     uint64 `json:"last"`
	Bytes    int64  `json:"bytes"`
	Raw      int64  `json:"raw"`
	Archived bool   `json:"archived,omitempty"`
}
type index struct {
	Job       string      `json:"job,omitempty"`
	Kind      string      `json:"kind,omitempty"`
	Version   int         `json:"version"`
	Chunks    []chunkMeta `json:"chunks"`
	Final     bool        `json:"final"`
	Truncated bool        `json:"truncated"`
}

// Store keeps live run logs in compressed chunk files under root. When a
// logdb archive is attached, sealed chunks are also copied into the separate
// SQLite log database: finished runs archive wholesale on Close, long-running
// workers archive incrementally via FlushActive, and leftover buffers from a
// crash are swept into the archive at startup by ArchiveOrphans. Reads merge
// the database, the buffer files, and the in-memory tail transparently, so
// callers never need to know where a frame currently lives.
type Store struct {
	root     string
	db       *logdb.LogDB
	mu       sync.Mutex
	writers  map[string]*Writer
	runLocks map[string]*runLock
	// Serialize archive transfers to bound aggregate batch memory. Acquire
	// before any per-run lock; readers and writers never take archiveMu.
	archiveMu sync.Mutex
	tailBytes int64
	tailLimit int64
}

func New(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &Store{root: root, writers: make(map[string]*Writer), runLocks: make(map[string]*runLock), tailLimit: 64 << 20}, nil
}

// AttachDB enables SQLite archival into the given log database.
func (s *Store) AttachDB(db *logdb.LogDB) { s.db = db }

// WriterOptions configures one run's log buffer.
type WriterOptions struct {
	// MaxBytes caps raw frame bytes in the file buffer, not the archive.
	// Successful archival frees capacity. 0 means unbounded.
	MaxBytes int64
	// MaxLine truncates single lines longer than this many bytes. 0 disables.
	MaxLine int
	// DropNew selects the log_on_full policy: true refuses new frames once
	// MaxBytes is reached; false (drop_old) evicts the oldest sealed chunks
	// instead.
	DropNew bool
}

func (s *Store) Open(runID, job, kind string, opt WriterOptions) (*Writer, error) {
	lock := s.lockRun(runID, true)
	defer s.unlockRun(runID, lock, true)
	dir := filepath.Join(s.root, runID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	w := &Writer{store: s, runID: runID, job: job, kind: kind, dir: dir, maxBytes: opt.MaxBytes, maxLine: opt.MaxLine, dropNew: opt.DropNew, subs: make(map[chan Frame]chan struct{})}
	if err := w.rotate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.writers[runID] = w
	s.mu.Unlock()
	return w, nil
}
func (s *Store) Active(runID string) *Writer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writers[runID]
}

// Close finalizes a run's buffer. With an attached archive it then moves every
// chunk into the SQLite log database and removes the buffer directory; the log
// stays queryable through Read afterwards. Without an archive the files remain
// on disk (pure file backend).
func (s *Store) Close(runID string) error {
	s.archiveMu.Lock()
	defer s.archiveMu.Unlock()
	w := s.Active(runID)
	if w == nil {
		return nil
	}
	err := w.Close()
	if s.db != nil && err == nil {
		err = s.archiveWriter(w, w.chunk)
	}
	lock := s.lockRun(runID, true)
	defer s.unlockRun(runID, lock, true)
	if s.db != nil && err == nil {
		err = os.RemoveAll(w.dir)
	}
	// Failed finalization leaves a durable buffer for the next orphan sweep.
	// Keep the writer registered until archival finishes so it cannot be
	// mistaken for an orphan between batches.
	s.mu.Lock()
	delete(s.writers, runID)
	s.mu.Unlock()
	return err
}

// FlushActive seals and archives the on-disk chunks of every open writer.
// It is the periodic checkpoint for long-running worker logs: the live buffer
// stays bounded while older output remains readable from the archive.
func (s *Store) FlushActive() {
	if s.db == nil {
		return
	}
	s.mu.Lock()
	ids := slices.Collect(maps.Keys(s.writers))
	s.mu.Unlock()
	for _, id := range ids {
		w := s.Active(id)
		if w == nil {
			continue
		}
		if err := w.Flush(s); err != nil {
			slog.Error("log archive flush failed", "run", id, "error", err)
		}
	}
}

// ArchiveOrphans sweeps buffers without an active writer into the archive.
// It handles both crash recovery and retries of failed final archival. Chunk
// files that were mid-write are salvaged up to the last intact frame.
func (s *Store) ArchiveOrphans() error {
	s.archiveMu.Lock()
	defer s.archiveMu.Unlock()
	if s.db == nil {
		return nil
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		runID := e.Name()
		lock := s.lockRun(runID, true)
		if s.Active(runID) == nil {
			if err := s.archiveOrphan(runID, filepath.Join(s.root, runID)); err != nil {
				errs = append(errs, fmt.Errorf("run %s: %w", runID, err))
			}
		}
		s.unlockRun(runID, lock, true)
	}
	return errors.Join(errs...)
}

type Writer struct {
	mu           sync.Mutex
	store        *Store
	runID        string
	job          string
	kind         string
	dir          string
	file         *os.File
	enc          *zstd.Encoder
	chunk        int
	chunkRaw     int
	chunkFirst   uint64
	seq          uint64
	total        int64 // accepted raw bytes, less output evicted before archival
	buffered     int64 // raw bytes still occupying the file buffer
	maxBytes     int64
	maxLine      int
	dropNew      bool
	truncated    bool
	idx          index
	subs         map[chan Frame]chan struct{}
	history      []Frame // circular storage; only historyCount entries are live
	historyHead  int
	historyCount int
	historyBytes int
	header       [24]byte
	closed       bool
	closeErr     error
	chunkErr     error // finalization failures leave the stream unusable
}

func (w *Writer) chunkPath(n int) string { return filepath.Join(w.dir, fmt.Sprintf("%06d.zst", n)) }

// Flush seals the current chunk (if it has data) and archives all sealed
// chunks of this writer.
func (w *Writer) Flush(s *Store) error {
	s.archiveMu.Lock()
	defer s.archiveMu.Unlock()
	through, err := w.sealForArchive()
	if err != nil || through == 0 || s.db == nil {
		return err
	}
	return s.archiveWriter(w, through)
}

func (w *Writer) sealForArchive() (int, error) {
	lock := w.store.lockRun(w.runID, true)
	defer w.store.unlockRun(w.runID, lock, true)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.store.db == nil {
		return 0, nil
	}
	if w.chunkRaw > 0 {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	return w.chunk - 1, nil
}

func (w *Writer) rotate() error {
	if w.chunkErr != nil {
		return w.chunkErr
	}
	// Acquire the replacement before closing the current stream. A full disk,
	// permission error, or conflicting path must leave the current chunk usable.
	next := w.chunk + 1
	path := w.chunkPath(next)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("open log chunk %d: %w", next, err)
	}
	if w.enc != nil {
		if err := w.finishChunk(); err != nil {
			w.chunkErr = errors.Join(err, f.Close(), os.Remove(path))
			return w.chunkErr
		}
	}
	enc := w.enc
	if enc == nil {
		// Most chunks are at most 1 MiB. Reserve a larger match window only
		// when the configured line limit permits an oversized single frame.
		window := chunkLimit
		if w.maxLine <= 0 || w.maxLine > 8<<20 {
			window = 8 << 20
		} else {
			for window < w.maxLine {
				window *= 2
			}
		}
		enc, err = zstd.NewWriter(f,
			zstd.WithEncoderLevel(zstd.SpeedFastest),
			zstd.WithEncoderConcurrency(1),
			zstd.WithWindowSize(window),
			zstd.WithLowerEncoderMem(true),
		)
		if err != nil {
			return errors.Join(fmt.Errorf("create log encoder: %w", err), f.Close(), os.Remove(path))
		}
	} else {
		// Close seals the old stream; Reset starts an independent chunk while
		// retaining the encoder's match window and compression buffers.
		enc.Reset(f)
	}
	w.chunk = next
	w.file, w.enc, w.chunkRaw, w.chunkFirst = f, enc, 0, w.seq+1
	return nil
}
func (w *Writer) finishChunk() error {
	if w.file == nil {
		return w.chunkErr
	}
	encErr := w.enc.Close()
	syncErr := w.file.Sync()
	info, statErr := w.file.Stat()
	closeErr := w.file.Close()
	w.file = nil
	if err := errors.Join(encErr, syncErr, statErr, closeErr); err != nil {
		w.chunkErr = fmt.Errorf("finalize log chunk %d: %w", w.chunk, err)
		return w.chunkErr
	}
	if w.seq >= w.chunkFirst {
		w.idx.Chunks = append(w.idx.Chunks, chunkMeta{w.chunk, w.chunkFirst, w.seq, info.Size(), int64(w.chunkRaw), false})
	}
	if err := w.writeIndex(); err != nil {
		w.chunkErr = fmt.Errorf("index log chunk %d: %w", w.chunk, err)
	}
	return w.chunkErr
}
func (w *Writer) writeIndex() error {
	w.idx.Job, w.idx.Kind = w.job, w.kind
	w.idx.Version = int(Version)
	w.idx.Truncated = w.truncated
	b, err := json.Marshal(w.idx)
	if err != nil {
		return err
	}
	tmp := filepath.Join(w.dir, "index.json.tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, filepath.Join(w.dir, "index.json")); err != nil {
		return err
	}
	dir, err := os.Open(w.dir)
	if err != nil {
		return err
	}
	err = dir.Sync()
	if closeErr := dir.Close(); err == nil {
		err = closeErr
	}
	return err
}
func (w *Writer) Write(stream Stream, payload []byte, flags Flags) error {
	lock := w.store.lockRun(w.runID, true)
	defer w.store.unlockRun(w.runID, lock, true)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errors.New("log writer closed")
	}
	if w.chunkErr != nil {
		return w.chunkErr
	}
	if len(payload) > w.maxLine && w.maxLine > 0 {
		payload = payload[:w.maxLine]
		flags |= FlagTruncated
		w.truncated = true
	}
	frameSize := 24 + len(payload)
	if w.maxBytes > 0 && w.buffered+int64(frameSize) > w.maxBytes {
		if w.dropNew || int64(frameSize) > w.maxBytes {
			w.truncated = true
			return nil
		}
		// Make the current segment evictable before dropping old data. This is
		// essential when maxBytes is smaller than the normal chunk threshold.
		if w.chunkRaw > 0 {
			if err := w.rotate(); err != nil {
				return err
			}
		}
		w.truncated = true
		for len(w.idx.Chunks) > 0 && w.buffered+int64(frameSize) > w.maxBytes {
			oldest := w.idx.Chunks[0]
			if err := os.Remove(w.chunkPath(oldest.Number)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			w.total -= oldest.Raw
			w.buffered -= oldest.Raw
			w.idx.Chunks = w.idx.Chunks[1:]
			w.discardHistoryThrough(oldest.Last)
		}
	}
	if w.chunkRaw+frameSize > chunkLimit && w.chunkRaw > 0 {
		if err := w.rotate(); err != nil {
			return err
		}
	}
	w.seq++
	f := Frame{w.seq, time.Now().UTC(), stream, flags, payload}
	if err := encodeWithHeader(w.enc, f, w.header[:]); err != nil {
		return err
	}
	// A successful Write is a durability boundary. Flush compressed bytes and
	// sync the hot chunk so a daemon crash cannot lose sparse accepted output.
	if err := w.enc.Flush(); err != nil {
		return err
	}
	if err := w.file.Sync(); err != nil {
		return err
	}
	w.chunkRaw += frameSize
	w.total += int64(frameSize)
	w.buffered += int64(frameSize)
	// Encoding and syncing are complete before the caller can reuse payload.
	// Copy only when a live tail or subscriber will retain it after return.
	reservedHistory := w.store.reserveTail(frameSize)
	if (reservedHistory && frameSize <= historyByteLimit) || len(w.subs) > 0 {
		f.Payload = slices.Clone(payload)
	}
	if reservedHistory {
		w.appendReservedHistory(f)
	}
	for ch, dropped := range w.subs {
		select {
		case ch <- f:
		default:
			close(dropped)
			close(ch)
			delete(w.subs, ch)
		}
	}
	return nil
}

func (w *Writer) appendHistory(f Frame) {
	historySize := len(f.Payload) + 24
	if !w.store.reserveTail(historySize) {
		return
	}
	w.appendReservedHistory(f)
}

// appendReservedHistory accepts a frame whose bytes were already charged to
// the store's tail quota. The payload must be owned by the caller.
func (w *Writer) appendReservedHistory(f Frame) {
	historySize := len(f.Payload) + 24
	if w.historyCount == historyLimit {
		w.store.releaseTail(w.evictHistory())
	}
	if w.historyCount == len(w.history) {
		size := min(max(64, 2*len(w.history)), historyLimit)
		grown := make([]Frame, size)
		for i := range w.historyCount {
			grown[i] = w.history[(w.historyHead+i)%len(w.history)]
		}
		w.history, w.historyHead = grown, 0
	}
	w.history[(w.historyHead+w.historyCount)%len(w.history)] = f
	w.historyCount++
	w.historyBytes += historySize
	for w.historyCount > 0 && w.historyBytes > historyByteLimit {
		w.store.releaseTail(w.evictHistory())
	}
}

// evictHistory clears the slot so the payload can be collected promptly.
// Callers hold w.mu and release the returned bytes from the store's quota.
func (w *Writer) evictHistory() int {
	f := &w.history[w.historyHead]
	released := len(f.Payload) + 24
	*f = Frame{}
	w.historyBytes -= released
	w.historyCount--
	if w.historyCount == 0 {
		w.history = nil
		w.historyHead = 0
	} else {
		w.historyHead = (w.historyHead + 1) % len(w.history)
	}
	return released
}
func (w *Writer) Pipe(stream Stream, r io.Reader) error {
	limit := w.maxLine
	if limit <= 0 || limit > maxFramePayload {
		limit = maxFramePayload
	}
	br := bufio.NewReaderSize(r, min(limit+1, 64<<10))
	line := make([]byte, 0, min(limit, 64<<10))
	truncated := false
	for {
		fragment, err := br.ReadSlice('\n')
		hasNewline := len(fragment) > 0 && fragment[len(fragment)-1] == '\n'
		if hasNewline {
			fragment = fragment[:len(fragment)-1]
		}
		if !truncated {
			remaining := limit - len(line)
			if len(fragment) > remaining {
				line = append(line, fragment[:max(remaining, 0)]...)
				truncated = true
			} else {
				line = append(line, fragment...)
			}
		}
		if hasNewline || (errors.Is(err, io.EOF) && len(line) > 0) {
			flags := Flags(0)
			if !hasNewline {
				flags |= FlagPartial
			}
			line = bytes.TrimSuffix(line, []byte{'\r'})
			if truncated {
				flags |= FlagTruncated
			}
			if !utf8.Valid(line) {
				flags |= FlagInvalidUTF8
			}
			if writeErr := w.Write(stream, line, flags); writeErr != nil {
				return writeErr
			}
			line = line[:0]
			truncated = false
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return err
		}
	}
}
func (w *Writer) Snapshot(after uint64, limit int) []Frame {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]Frame, 0, min(limit, w.historyCount))
	for i := range w.historyCount {
		frame := w.history[(w.historyHead+i)%len(w.history)]
		if frame.Sequence > after && len(out) < limit {
			out = append(out, frame)
		}
	}
	return out
}
func (w *Writer) Subscribe(after uint64) (<-chan Frame, <-chan struct{}, func()) {
	ch := make(chan Frame, 256)
	dropped := make(chan struct{})
	w.mu.Lock()
	if w.closed {
		close(ch)
		w.mu.Unlock()
		return ch, dropped, func() {}
	}
	w.subs[ch] = dropped
	w.mu.Unlock()
	return ch, dropped, func() {
		w.mu.Lock()
		if _, ok := w.subs[ch]; ok {
			delete(w.subs, ch)
			close(ch)
		}
		w.mu.Unlock()
	}
}
func (w *Writer) Close() error {
	lock := w.store.lockRun(w.runID, true)
	defer w.store.unlockRun(w.runID, lock, true)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return w.closeErr
	}
	w.closed = true
	w.idx.Final = true
	w.closeErr = w.finishChunk()
	w.store.releaseTail(w.historyBytes)
	w.historyBytes = 0
	w.history = nil
	w.historyHead = 0
	w.historyCount = 0
	for ch := range w.subs {
		close(ch)
		delete(w.subs, ch)
	}
	return w.closeErr
}

// discardHistoryThrough releases frames no longer needed in the live tail.
// Callers hold w.mu. Archived frames must not reappear after archive pruning.
func (w *Writer) discardHistoryThrough(sequence uint64) {
	released := 0
	for w.historyCount > 0 && w.history[w.historyHead].Sequence <= sequence {
		released += w.evictHistory()
	}
	w.store.releaseTail(released)
}

func (s *Store) reserveTail(n int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tailBytes+int64(n) > s.tailLimit {
		return false
	}
	s.tailBytes += int64(n)
	return true
}
func (s *Store) releaseTail(n int) {
	s.mu.Lock()
	s.tailBytes -= int64(n)
	if s.tailBytes < 0 {
		s.tailBytes = 0
	}
	s.mu.Unlock()
}

func (w *Writer) Stats() (int64, bool) { w.mu.Lock(); defer w.mu.Unlock(); return w.total, w.truncated }
func encode(dst io.Writer, f Frame) error {
	var h [24]byte
	return encodeWithHeader(dst, f, h[:])
}

func encodeWithHeader(dst io.Writer, f Frame, h []byte) error {
	h[0] = Version
	h[1] = byte(f.Stream)
	h[2] = byte(f.Flags)
	h[3] = 0
	binary.BigEndian.PutUint64(h[4:12], f.Sequence)
	binary.BigEndian.PutUint64(h[12:20], uint64(f.Timestamp.UnixMicro()))
	binary.BigEndian.PutUint32(h[20:24], uint32(len(f.Payload)))
	if _, err := dst.Write(h); err != nil {
		return err
	}
	_, err := dst.Write(f.Payload)
	return err
}
func decode(src io.Reader) (Frame, error) {
	f, n, err := decodeHeader(src)
	if err != nil {
		return f, err
	}
	f.Payload = make([]byte, n)
	_, err = io.ReadFull(src, f.Payload)
	return f, err
}

func decodeHeader(src io.Reader) (Frame, int, error) {
	var h [24]byte
	return decodeHeaderWithBuffer(src, h[:])
}

func decodeHeaderWithBuffer(src io.Reader, h []byte) (Frame, int, error) {
	var f Frame
	if _, err := io.ReadFull(src, h); err != nil {
		return f, 0, err
	}
	if h[0] != Version {
		return f, 0, fmt.Errorf("unsupported log frame version %d", h[0])
	}
	f.Stream = Stream(h[1])
	f.Flags = Flags(h[2])
	f.Sequence = binary.BigEndian.Uint64(h[4:12])
	f.Timestamp = time.UnixMicro(int64(binary.BigEndian.Uint64(h[12:20])))
	n := binary.BigEndian.Uint32(h[20:24])
	if n > maxFramePayload {
		return f, 0, fmt.Errorf("log frame payload %d exceeds maximum %d", n, maxFramePayload)
	}
	return f, int(n), nil
}

// salvageFrames decodes frames from a possibly torn chunk stream, stopping at
// the first corruption and returning everything recovered before it.
func salvageFrames(blob []byte) []Frame {
	dec, err := zstd.NewReader(bytes.NewReader(blob), zstd.WithDecoderMaxMemory(32<<20), zstd.WithDecoderMaxWindow(16<<20))
	if err != nil {
		return nil
	}
	defer dec.Close()
	var frames []Frame
	for {
		f, err := decode(dec)
		if err != nil {
			return frames
		}
		frames = append(frames, f)
	}
}

func encodeFrames(frames []Frame) ([]byte, error) {
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		return nil, err
	}
	for _, f := range frames {
		if err := encode(enc, f); err != nil {
			enc.Close()
			return nil, err
		}
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Read returns up to limit frames with a sequence greater than after, merged
// across the three storage tiers: archived chunks in the log database, sealed
// chunk files in the buffer directory, and the active writer's memory tail.
func (s *Store) Read(runID string, after uint64, limit int) ([]Frame, error) {
	return s.ReadContext(context.Background(), runID, after, limit)
}

func (s *Store) ReadContext(ctx context.Context, runID string, after uint64, limit int) ([]Frame, error) {
	return s.readContext(ctx, runID, after, limit, 16<<20, nil)
}

// ReadStreamContext uses a 1 MiB page budget for streaming consumers.
// Like ReadContext, one oversized valid frame is returned to advance the cursor.
func (s *Store) ReadStreamContext(ctx context.Context, runID string, after uint64, limit int) ([]Frame, error) {
	return s.readContext(ctx, runID, after, limit, 1<<20, nil)
}

// StreamReader reuses a decoder across backlog pages of one stream. It is
// owned by one caller and must be closed when that stream ends.
type StreamReader struct {
	store     *Store
	runID     string
	pageBytes int
	decoder   *zstd.Decoder
}

func (s *Store) NewStreamReader(runID string) *StreamReader {
	return s.newStreamReader(runID, 1<<20)
}

func (s *Store) newStreamReader(runID string, pageBytes int) *StreamReader {
	return &StreamReader{store: s, runID: runID, pageBytes: pageBytes}
}

func (r *StreamReader) ReadContext(ctx context.Context, after uint64, limit int) ([]Frame, error) {
	return r.store.readContext(ctx, r.runID, after, limit, r.pageBytes, &r.decoder)
}

func (r *StreamReader) Close() {
	if r.decoder != nil {
		r.decoder.Close()
		r.decoder = nil
	}
}

func (s *Store) readContext(ctx context.Context, runID string, after uint64, limit, maxResponseBytes int, decoder **zstd.Decoder) ([]Frame, error) {
	if decoder == nil {
		var owned *zstd.Decoder
		decoder = &owned
		defer func() {
			if owned != nil {
				owned.Close()
			}
		}()
	}
	// Pin the run's tier layout for the entire page. Migration, retention,
	// finalization and writes cannot move the cursor past unseen frames.
	lock := s.lockRun(runID, false)
	defer s.unlockRun(runID, lock, false)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit = min(max(limit, 1), 5000)
	out := make([]Frame, 0, min(limit, 100))
	last, responseBytes := after, 0
	appendFrame := func(frame Frame) bool {
		if frame.Sequence <= last {
			return true
		}
		// Allow one maximum-size frame (including its header) so every
		// valid frame can make progress. Never skip it for a smaller one.
		if len(out) > 0 && responseBytes+len(frame.Payload)+24 > maxResponseBytes {
			return false
		}
		out = append(out, frame)
		responseBytes += len(frame.Payload) + 24
		last = frame.Sequence
		return len(out) < limit && responseBytes < maxResponseBytes
	}
	// io.ReadFull passes its buffer through an interface. Reusing one header
	// per page avoids a heap allocation for every decoded or skipped frame.
	var header [24]byte
	var skipped io.LimitedReader
	consume := func(src io.Reader) (bool, error) {
		if *decoder == nil {
			var err error
			*decoder, err = zstd.NewReader(src, zstd.WithDecoderConcurrency(2), zstd.WithDecoderMaxMemory(32<<20), zstd.WithDecoderMaxWindow(16<<20))
			if err != nil {
				return false, err
			}
		} else if err := (*decoder).Reset(src); err != nil {
			return false, err
		}
		for {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			frame, payloadSize, err := decodeHeaderWithBuffer(*decoder, header[:])
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return true, nil
			}
			if err != nil {
				return false, err
			}
			if frame.Sequence <= last {
				// Cursor paging can skip many frames in its first chunk. Reuse
				// CopyN's limiting reader instead of allocating one per skip.
				skipped.R, skipped.N = *decoder, int64(payloadSize)
				_, err = io.Copy(io.Discard, &skipped)
				if skipped.N == 0 {
					err = nil
				} else if err == nil {
					err = io.EOF
				}
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					return true, nil
				}
				if err != nil {
					return false, err
				}
				continue
			}
			// Stop after the header, before allocating a payload that cannot
			// fit in this page. The next call restarts at the same sequence.
			if len(out) > 0 && responseBytes+payloadSize+24 > maxResponseBytes {
				return false, nil
			}
			frame.Payload = make([]byte, payloadSize)
			_, err = io.ReadFull(*decoder, frame.Payload)
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return true, nil
			}
			if err != nil {
				return false, err
			}
			if !appendFrame(frame) {
				return false, nil
			}
		}
	}
	if s.db != nil {
		more := true
		err := s.db.EachChunk(ctx, runID, after, func(c logdb.Chunk) (bool, error) {
			var err error
			more, err = consume(bytes.NewReader(c.Blob))
			return more, err
		})
		if err != nil {
			return nil, err
		}
		if !more {
			return out, nil
		}
	}
	entries, err := chunkFiles(filepath.Join(s.root, runID))
	if err != nil {
		return nil, err
	}
	// The index lists only sealed chunks. Missing, stale, or invalid entries
	// still take the normal decode path so unsealed and crash-salvaged files
	// remain visible.
	var sealedLast map[int]uint64
	if after > 0 && len(entries) > 1 {
		if data, err := os.ReadFile(filepath.Join(s.root, runID, "index.json")); err == nil {
			var idx index
			if json.Unmarshal(data, &idx) == nil && idx.Version == int(Version) {
				sealedLast = make(map[int]uint64, len(idx.Chunks))
				for _, chunk := range idx.Chunks {
					if chunk.Number > 0 && chunk.First > 0 && chunk.Last >= chunk.First {
						sealedLast[chunk.Number] = chunk.Last
					}
				}
			}
		}
	}
	for _, entry := range entries {
		if lastSequence, ok := sealedLast[entry.number]; ok && lastSequence <= after {
			continue
		}
		f, err := os.Open(entry.path)
		if err != nil {
			return nil, err
		}
		more, err := consume(f)
		f.Close()
		if err != nil {
			return nil, err
		}
		if !more {
			return out, nil
		}
	}
	if active := s.Active(runID); active != nil {
		for _, frame := range active.Snapshot(last, limit-len(out)) {
			if !appendFrame(frame) {
				break
			}
		}
	}
	return out, nil
}
func (s *Store) Raw(runID string, w io.Writer) error {
	return s.RawContext(context.Background(), runID, w)
}

func (s *Store) RawContext(ctx context.Context, runID string, w io.Writer) error {
	reader := s.newStreamReader(runID, 4<<20)
	defer reader.Close()
	// Reuse the separator instead of allocating a byte slice for each frame.
	newline := []byte{'\n'}
	var after uint64
	for {
		frames, err := reader.ReadContext(ctx, after, 5000)
		if err != nil {
			return err
		}
		for _, f := range frames {
			if f.Stream == Stderr {
				if _, err := io.WriteString(w, "[err] "); err != nil {
					return err
				}
			}
			if f.Stream == System {
				if _, err := io.WriteString(w, "[minicron] "); err != nil {
					return err
				}
			}
			if _, err := w.Write(f.Payload); err != nil {
				return err
			}
			if _, err := w.Write(newline); err != nil {
				return err
			}
			after = f.Sequence
		}
		if len(frames) == 0 {
			return nil
		}
	}
}

// Delete removes a run's logs from both the archive and the buffer directory.
func (s *Store) Delete(runID string) error {
	s.archiveMu.Lock()
	defer s.archiveMu.Unlock()
	lock := s.lockRun(runID, true)
	defer s.unlockRun(runID, lock, true)
	if s.Active(runID) != nil {
		return errors.New("cannot delete logs of an active writer")
	}
	if s.db != nil {
		if err := s.db.DeleteRun(context.Background(), runID); err != nil {
			return err
		}
	}
	return os.RemoveAll(filepath.Join(s.root, runID))
}

// DeleteRuns removes a retention page of inactive runs. An archive batch
// failure falls back to individual deletion, allowing unaffected runs to
// progress. The returned IDs have had both archive and file buffers removed.
func (s *Store) DeleteRuns(runIDs []string) ([]string, error) {
	s.archiveMu.Lock()
	defer s.archiveMu.Unlock()
	seen := make(map[string]bool, len(runIDs))
	valid := make([]string, 0, len(runIDs))
	type heldRunLock struct {
		id   string
		lock *runLock
	}
	var locks []heldRunLock
	var errs []error
	for _, id := range runIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		lock := s.lockRun(id, true)
		if s.Active(id) != nil {
			s.unlockRun(id, lock, true)
			errs = append(errs, fmt.Errorf("run %s: cannot delete logs of an active writer", id))
			continue
		}
		locks = append(locks, heldRunLock{id: id, lock: lock})
		valid = append(valid, id)
	}
	defer func() {
		for i := len(locks) - 1; i >= 0; i-- {
			s.unlockRun(locks[i].id, locks[i].lock, true)
		}
	}()
	batchFailed := false
	if s.db != nil && len(valid) > 0 {
		if err := s.db.DeleteRuns(context.Background(), valid); err != nil {
			batchFailed = true
			errs = append(errs, fmt.Errorf("archive batch deletion: %w", err))
		}
	}
	deleted := make([]string, 0, len(valid))
	for _, id := range valid {
		if batchFailed {
			if err := s.db.DeleteRun(context.Background(), id); err != nil {
				errs = append(errs, fmt.Errorf("run %s: %w", id, err))
				continue
			}
		}
		if err := os.RemoveAll(filepath.Join(s.root, id)); err != nil {
			errs = append(errs, fmt.Errorf("run %s: %w", id, err))
			continue
		}
		deleted = append(deleted, id)
	}
	return deleted, errors.Join(errs...)
}
