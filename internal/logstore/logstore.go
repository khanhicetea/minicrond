package logstore

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"
)

const Version byte = 1
const chunkLimit = 1 << 20

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
	Number int    `json:"number"`
	First  uint64 `json:"first"`
	Last   uint64 `json:"last"`
	Bytes  int64  `json:"bytes"`
	Raw    int64  `json:"raw"`
}
type index struct {
	Version   int         `json:"version"`
	Chunks    []chunkMeta `json:"chunks"`
	Final     bool        `json:"final"`
	Truncated bool        `json:"truncated"`
}

type Store struct {
	root    string
	mu      sync.Mutex
	writers map[string]*Writer
}

func New(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &Store{root: root, writers: make(map[string]*Writer)}, nil
}
func (s *Store) Open(runID string, maxBytes int64, maxLine int) (*Writer, error) {
	dir := filepath.Join(s.root, runID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	w := &Writer{dir: dir, maxBytes: maxBytes, maxLine: maxLine, subs: make(map[chan Frame]chan struct{})}
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
func (s *Store) Close(runID string) error {
	s.mu.Lock()
	w := s.writers[runID]
	delete(s.writers, runID)
	s.mu.Unlock()
	if w == nil {
		return nil
	}
	return w.Close()
}

type Writer struct {
	mu         sync.Mutex
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
	truncated  bool
	idx        index
	subs       map[chan Frame]chan struct{}
	history    []Frame
	closed     bool
}

func (w *Writer) rotate() error {
	if w.enc != nil {
		if err := w.finishChunk(); err != nil {
			return err
		}
	}
	w.chunk++
	path := filepath.Join(w.dir, fmt.Sprintf("%06d.zst", w.chunk))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
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
		w.idx.Chunks = append(w.idx.Chunks, chunkMeta{w.chunk, w.chunkFirst, w.seq, info.Size(), int64(w.chunkRaw)})
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
		w.truncated = true
		for len(w.idx.Chunks) > 0 && w.total+int64(frameSize) > w.maxBytes {
			oldest := w.idx.Chunks[0]
			_ = os.Remove(filepath.Join(w.dir, fmt.Sprintf("%06d.zst", oldest.Number)))
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
	if len(w.history) > 5000 {
		w.history = slices.Clone(w.history[len(w.history)-5000:])
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
			if !utf8Valid(line) {
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
func (s *Store) Read(runID string, after uint64, limit int) ([]Frame, error) {
	limit = min(max(limit, 1), 5000)
	entries, err := filepath.Glob(filepath.Join(s.root, runID, "*.zst"))
	if err != nil {
		return nil, err
	}
	slices.Sort(entries)
	out := make([]Frame, 0, min(limit, 100))
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
			if frame.Sequence > after {
				out = append(out, frame)
			}
		}
		dec.Close()
		f.Close()
		if len(out) >= limit {
			break
		}
	}
	if active := s.Active(runID); active != nil && len(out) < limit {
		last := after
		if len(out) > 0 {
			last = out[len(out)-1].Sequence
		}
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
func (s *Store) Delete(runID string) error { return os.RemoveAll(filepath.Join(s.root, runID)) }
func ParseBytes(value string) (int64, error) {
	value = strings.TrimSpace(value)
	units := map[string]int64{"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "B": 1}
	for suffix, mul := range units {
		if n, ok := strings.CutSuffix(value, suffix); ok {
			v, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
			return v * mul, err
		}
	}
	return strconv.ParseInt(value, 10, 64)
}
func utf8Valid(p []byte) bool { return utf8.Valid(p) }
