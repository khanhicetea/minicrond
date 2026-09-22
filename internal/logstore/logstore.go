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
	"strconv"
	"strings"
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
	unlock := s.lockRun(runID, true)
	defer unlock()
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
	unlock := s.lockRun(runID, true)
	defer unlock()
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
		unlock := s.lockRun(runID, true)
		if s.Active(runID) == nil {
			if err := s.archiveOrphan(runID, filepath.Join(s.root, runID)); err != nil {
				errs = append(errs, fmt.Errorf("run %s: %w", runID, err))
			}
		}
		unlock()
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
	history      []Frame
	historyBytes int
	closed       bool
	closeErr     error
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
	unlock := w.store.lockRun(w.runID, true)
	defer unlock()
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
	if w.enc != nil {
		if err := w.finishChunk(); err != nil {
			return err
		}
	}
	w.chunk++
	f, err := os.OpenFile(w.chunkPath(w.chunk), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	enc, err := zstd.NewWriter(f, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		f.Close()
		return err
	}
	w.file, w.enc, w.chunkRaw, w.chunkFirst = f, enc, 0, w.seq+1
	return nil
}
func (w *Writer) finishChunk() error {
	encErr := w.enc.Close()
	syncErr := w.file.Sync()
	info, statErr := w.file.Stat()
	closeErr := w.file.Close()
	if err := errors.Join(encErr, syncErr, statErr, closeErr); err != nil {
		return err
	}
	if w.seq >= w.chunkFirst {
		w.idx.Chunks = append(w.idx.Chunks, chunkMeta{w.chunk, w.chunkFirst, w.seq, info.Size(), int64(w.chunkRaw), false})
	}
	return w.writeIndex()
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
	unlock := w.store.lockRun(w.runID, true)
	defer unlock()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errors.New("log writer closed")
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
	f := Frame{w.seq, time.Now().UTC(), stream, flags, slices.Clone(payload)}
	if err := encode(w.enc, f); err != nil {
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
	historySize := len(f.Payload) + 24
	if w.store.reserveTail(historySize) {
		w.history = append(w.history, f)
		w.historyBytes += historySize
	}
	for len(w.history) > 0 && (len(w.history) > historyLimit || w.historyBytes > historyByteLimit) {
		released := len(w.history[0].Payload) + 24
		w.historyBytes -= released
		w.store.releaseTail(released)
		w.history = slices.Delete(w.history, 0, 1)
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
	out := make([]Frame, 0, min(limit, len(w.history)))
	for _, frame := range w.history {
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
	unlock := w.store.lockRun(w.runID, true)
	defer unlock()
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
	for ch := range w.subs {
		close(ch)
		delete(w.subs, ch)
	}
	return w.closeErr
}

// discardHistoryThrough releases frames no longer needed in the live tail.
// Callers hold w.mu. Archived frames must not reappear after archive pruning.
func (w *Writer) discardHistoryThrough(sequence uint64) {
	n, released := 0, 0
	for n < len(w.history) && w.history[n].Sequence <= sequence {
		released += len(w.history[n].Payload) + 24
		n++
	}
	w.history = slices.Delete(w.history, 0, n)
	w.historyBytes -= released
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
	h[0] = Version
	h[1] = byte(f.Stream)
	h[2] = byte(f.Flags)
	binary.BigEndian.PutUint64(h[4:12], f.Sequence)
	binary.BigEndian.PutUint64(h[12:20], uint64(f.Timestamp.UnixMicro()))
	binary.BigEndian.PutUint32(h[20:24], uint32(len(f.Payload)))
	if _, err := dst.Write(h[:]); err != nil {
		return err
	}
	_, err := dst.Write(f.Payload)
	return err
}
func decode(src io.Reader) (Frame, error) {
	var f Frame
	var h [24]byte
	if _, err := io.ReadFull(src, h[:]); err != nil {
		return f, err
	}
	if h[0] != Version {
		return f, fmt.Errorf("unsupported log frame version %d", h[0])
	}
	f.Stream = Stream(h[1])
	f.Flags = Flags(h[2])
	f.Sequence = binary.BigEndian.Uint64(h[4:12])
	f.Timestamp = time.UnixMicro(int64(binary.BigEndian.Uint64(h[12:20])))
	n := binary.BigEndian.Uint32(h[20:24])
	if n > maxFramePayload {
		return f, fmt.Errorf("log frame payload %d exceeds maximum %d", n, maxFramePayload)
	}
	f.Payload = make([]byte, int(n))
	_, err := io.ReadFull(src, f.Payload)
	return f, err
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
	// Pin the run's tier layout for the entire page. Migration, retention,
	// finalization and writes cannot move the cursor past unseen frames.
	unlock := s.lockRun(runID, false)
	defer unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit = min(max(limit, 1), 5000)
	out := make([]Frame, 0, min(limit, 100))
	last, responseBytes := after, 0
	const maxResponseBytes = 16 << 20
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
	consume := func(src io.Reader) (bool, error) {
		dec, err := zstd.NewReader(src, zstd.WithDecoderMaxMemory(32<<20), zstd.WithDecoderMaxWindow(16<<20))
		if err != nil {
			return false, err
		}
		defer dec.Close()
		for {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			frame, err := decode(dec)
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
	for _, entry := range entries {
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
	var after uint64
	for {
		frames, err := s.ReadContext(ctx, runID, after, 5000)
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
			if _, err := w.Write([]byte{'\n'}); err != nil {
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
	unlock := s.lockRun(runID, true)
	defer unlock()
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

// byteUnits lists byte-size suffixes longest-first. Order matters: a map
// would make "10KiB" randomly match the bare "B" suffix, silently changing
// the parsed size from run to run.
var byteUnits = []struct {
	suffix string
	mul    int64
}{
	{"GiB", 1 << 30},
	{"MiB", 1 << 20},
	{"KiB", 1 << 10},
	{"B", 1},
}

func ParseBytes(value string) (int64, error) {
	value = strings.TrimSpace(value)
	for _, u := range byteUnits {
		if n, ok := strings.CutSuffix(value, u.suffix); ok {
			v, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
			if err != nil {
				return 0, err
			}
			if v < 0 || v > int64(^uint64(0)>>1)/u.mul {
				return 0, errors.New("byte size must be nonnegative and fit in int64")
			}
			return v * u.mul, nil
		}
	}
	v, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, err
	}
	if v < 0 {
		return 0, errors.New("byte size must be nonnegative")
	}
	return v, nil
}
