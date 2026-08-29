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

	"github.com/minicron/minicron/internal/logdb"
)

const Version byte = 1
const chunkLimit = 1 << 20

// historyLimit bounds the in-memory tail served to live readers. Trimming
// uses hysteresis (see Write) so the shift is amortized instead of running
// on every frame once the tail is full.
const historyLimit = 5000

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
	root    string
	db      *logdb.LogDB
	mu      sync.Mutex
	writers map[string]*Writer
}

func New(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &Store{root: root, writers: make(map[string]*Writer)}, nil
}

// AttachDB enables SQLite archival into the given log database.
func (s *Store) AttachDB(db *logdb.LogDB) { s.db = db }

// WriterOptions configures one run's log buffer.
type WriterOptions struct {
	// MaxBytes caps the retained log (raw frame bytes). 0 means unbounded.
	MaxBytes int64
	// MaxLine truncates single lines longer than this many bytes. 0 disables.
	MaxLine int
	// DropNew selects the log_on_full policy: true refuses new frames once
	// MaxBytes is reached; false (drop_old) evicts the oldest sealed chunks
	// instead.
	DropNew bool
}

func (s *Store) Open(runID, job, kind string, opt WriterOptions) (*Writer, error) {
	dir := filepath.Join(s.root, runID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	w := &Writer{runID: runID, job: job, kind: kind, dir: dir, maxBytes: opt.MaxBytes, maxLine: opt.MaxLine, dropNew: opt.DropNew, subs: make(map[chan Frame]chan struct{})}
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
	s.mu.Lock()
	w := s.writers[runID]
	delete(s.writers, runID)
	s.mu.Unlock()
	if w == nil {
		return nil
	}
	err := w.Close()
	if s.db != nil {
		w.mu.Lock()
		aerr := s.archiveSealedLocked(w)
		// Remove the buffer only when the writer finalized cleanly; a failed
		// close leaves the directory for the startup salvage sweep.
		if aerr == nil && err == nil {
			aerr = os.RemoveAll(w.dir)
		}
		w.mu.Unlock()
		err = errors.Join(err, aerr)
	}
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

// ArchiveOrphans sweeps buffer directories left behind by a crash (no active
// writer) into the archive and deletes them. Chunk files that were mid-write
// when the daemon died are salvaged up to the last intact frame.
func (s *Store) ArchiveOrphans() error {
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
		if s.Active(runID) != nil {
			continue
		}
		if err := s.archiveOrphan(runID, filepath.Join(s.root, runID)); err != nil {
			errs = append(errs, fmt.Errorf("run %s: %w", runID, err))
		}
	}
	return errors.Join(errs...)
}

// archiveSealedLocked copies every not-yet-archived sealed chunk of an open or
// closed writer into the archive, marks it archived in the index, and removes
// the buffer file. Database first, file removal second: a crash in between is
// healed by the upsert on the next attempt. Callers must hold w.mu.
func (s *Store) archiveSealedLocked(w *Writer) error {
	if s.db == nil {
		return nil
	}
	var pending []logdb.Chunk
	for _, m := range w.idx.Chunks {
		if m.Archived {
			continue
		}
		blob, err := os.ReadFile(w.chunkPath(m.Number))
		if errors.Is(err, fs.ErrNotExist) {
			continue // already removed by retention or an earlier flush
		}
		if err != nil {
			return err
		}
		pending = append(pending, logdb.Chunk{Number: m.Number, First: m.First, Last: m.Last, RawBytes: m.Raw, Blob: blob})
	}
	if len(pending) == 0 {
		return nil
	}
	if err := s.db.PutChunks(context.Background(), w.runID, w.job, w.kind, time.Now(), pending); err != nil {
		return err
	}
	for _, c := range pending {
		for i := range w.idx.Chunks {
			if w.idx.Chunks[i].Number == c.Number {
				w.idx.Chunks[i].Archived = true
			}
		}
		if err := os.Remove(w.chunkPath(c.Number)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return w.writeIndex()
}

// archiveOrphan recovers one orphaned buffer directory. Sealed chunks named in
// index.json are archived verbatim; chunk files missing from the index (or a
// missing index) were mid-write at the crash and are salvaged frame by frame.
func (s *Store) archiveOrphan(runID, dir string) error {
	sealed := make(map[int]chunkMeta)
	if b, err := os.ReadFile(filepath.Join(dir, "index.json")); err == nil {
		var idx index
		if json.Unmarshal(b, &idx) == nil {
			for _, m := range idx.Chunks {
				sealed[m.Number] = m
			}
		}
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.zst"))
	if err != nil {
		return err
	}
	slices.Sort(files)
	var chunks []logdb.Chunk
	for _, path := range files {
		number, err := strconv.Atoi(strings.TrimSuffix(filepath.Base(path), ".zst"))
		if err != nil {
			continue
		}
		blob, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if m, ok := sealed[number]; ok {
			chunks = append(chunks, logdb.Chunk{Number: number, First: m.First, Last: m.Last, RawBytes: m.Raw, Blob: blob})
			continue
		}
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
		chunks = append(chunks, logdb.Chunk{Number: number, First: frames[0].Sequence, Last: frames[len(frames)-1].Sequence, RawBytes: raw, Blob: reenc})
	}
	if len(chunks) == 0 {
		return os.RemoveAll(dir)
	}
	if err := s.db.PutChunks(context.Background(), runID, "", "", time.Now(), chunks); err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

type Writer struct {
	mu         sync.Mutex
	runID      string
	job        string
	kind       string
	dir        string
	file       *os.File
	enc        *zstd.Encoder
	chunk      int
	chunkRaw   int
	chunkFirst uint64
	seq        uint64
	total      int64
	maxBytes   int64
	maxLine    int
	dropNew    bool
	truncated  bool
	idx        index
	subs       map[chan Frame]chan struct{}
	history    []Frame
	closed     bool
}

func (w *Writer) chunkPath(n int) string { return filepath.Join(w.dir, fmt.Sprintf("%06d.zst", n)) }

// Flush seals the current chunk (if it has data) and archives all sealed
// chunks of this writer.
func (w *Writer) Flush(s *Store) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || s.db == nil {
		return nil
	}
	if w.chunkRaw > 0 {
		if err := w.rotate(); err != nil {
			return err
		}
	}
	return s.archiveSealedLocked(w)
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
	if err := w.enc.Close(); err != nil {
		return err
	}
	if err := w.file.Sync(); err != nil {
		return err
	}
	info, err := w.file.Stat()
	if closeErr := w.file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if w.seq >= w.chunkFirst {
		w.idx.Chunks = append(w.idx.Chunks, chunkMeta{w.chunk, w.chunkFirst, w.seq, info.Size(), int64(w.chunkRaw), false})
	}
	return w.writeIndex()
}
func (w *Writer) writeIndex() error {
	w.idx.Version = int(Version)
	w.idx.Truncated = w.truncated
	b, err := json.Marshal(w.idx)
	if err != nil {
		return err
	}
	tmp := filepath.Join(w.dir, "index.json.tmp")
	if err = os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(w.dir, "index.json"))
}
func (w *Writer) Write(stream Stream, payload []byte, flags Flags) error {
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
	if w.maxBytes > 0 && w.total+int64(frameSize) > w.maxBytes {
		if w.dropNew {
			// log_on_full = drop_new: keep the retained history and refuse
			// the incoming frame instead of evicting old chunks.
			w.truncated = true
			return nil
		}
		w.truncated = true
		for len(w.idx.Chunks) > 0 && w.total+int64(frameSize) > w.maxBytes {
			oldest := w.idx.Chunks[0]
			_ = os.Remove(w.chunkPath(oldest.Number))
			w.total -= oldest.Raw
			w.idx.Chunks = w.idx.Chunks[1:]
		}
		if w.total+int64(frameSize) > w.maxBytes {
			return nil
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
	w.chunkRaw += frameSize
	w.total += int64(frameSize)
	w.history = append(w.history, f)
	// Trim with hysteresis: shifting the tail on every write would cost an
	// O(historyLimit) move per frame, so allow 25% overshoot before cutting
	// back down to the limit.
	if len(w.history) > historyLimit+historyLimit/4 {
		w.history = slices.Delete(w.history, 0, len(w.history)-historyLimit)
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
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			flags := Flags(0)
			if line[len(line)-1] == '\n' {
				line = line[:len(line)-1]
				line = bytes.TrimSuffix(line, []byte{'\r'})
			} else {
				flags |= FlagPartial
			}
			if !utf8.Valid(line) {
				flags |= FlagInvalidUTF8
			}
			if writeErr := w.Write(stream, line, flags); writeErr != nil {
				return writeErr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
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
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	w.idx.Final = true
	err := w.finishChunk()
	for ch := range w.subs {
		close(ch)
		delete(w.subs, ch)
	}
	return err
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
	f.Payload = make([]byte, n)
	_, err := io.ReadFull(src, f.Payload)
	return f, err
}

// salvageFrames decodes frames from a possibly torn chunk stream, stopping at
// the first corruption and returning everything recovered before it.
func salvageFrames(blob []byte) []Frame {
	dec, err := zstd.NewReader(bytes.NewReader(blob))
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
	limit = min(max(limit, 1), 5000)
	out := make([]Frame, 0, min(limit, 100))
	last := after
	if s.db != nil {
		err := s.db.EachChunk(context.Background(), runID, last, func(c logdb.Chunk) (bool, error) {
			dec, err := zstd.NewReader(bytes.NewReader(c.Blob))
			if err != nil {
				return false, err
			}
			defer dec.Close()
			for len(out) < limit {
				frame, err := decode(dec)
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					break
				}
				if err != nil {
					return false, err
				}
				if frame.Sequence > last {
					out = append(out, frame)
					last = frame.Sequence
				}
			}
			return len(out) < limit, nil
		})
		if err != nil {
			return nil, err
		}
		if len(out) >= limit {
			return out, nil
		}
	}
	entries, err := filepath.Glob(filepath.Join(s.root, runID, "*.zst"))
	if err != nil {
		return nil, err
	}
	slices.Sort(entries)
	for _, path := range entries {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		dec, err := zstd.NewReader(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		for len(out) < limit {
			frame, err := decode(dec)
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			if err != nil {
				dec.Close()
				f.Close()
				return nil, err
			}
			if frame.Sequence > last {
				out = append(out, frame)
				last = frame.Sequence
			}
		}
		dec.Close()
		f.Close()
		if len(out) >= limit {
			break
		}
	}
	if active := s.Active(runID); active != nil && len(out) < limit {
		out = append(out, active.Snapshot(last, limit-len(out))...)
	}
	return out, nil
}
func (s *Store) Raw(runID string, w io.Writer) error {
	var after uint64
	for {
		frames, err := s.Read(runID, after, 5000)
		if err != nil {
			return err
		}
		for _, f := range frames {
			if f.Stream == Stderr {
				_, _ = io.WriteString(w, "[err] ")
			}
			if f.Stream == System {
				_, _ = io.WriteString(w, "[minicron] ")
			}
			_, _ = w.Write(f.Payload)
			_, _ = w.Write([]byte{'\n'})
			after = f.Sequence
		}
		if len(frames) < 5000 {
			return nil
		}
	}
}

// Delete removes a run's logs from both the archive and the buffer directory.
func (s *Store) Delete(runID string) error {
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
			return v * u.mul, nil
		}
	}
	return strconv.ParseInt(value, 10, 64)
}
