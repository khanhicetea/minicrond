package logstore

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/logdb"
	"github.com/khanhicetea/minicrond/internal/model"
)

func BenchmarkFrameEncoding(b *testing.B) {
	payload := make([]byte, 1024)
	b.SetBytes(int64(len(payload)))
	store, err := New(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	writer, err := store.Open("run", "bench", model.KindJob, WriterOptions{MaxBytes: 1 << 40, MaxLine: 256 << 10})
	if err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if err := writer.Write(Stdout, payload, 0); err != nil {
			b.Fatal(err)
		}
	}
	if err := store.Close("run"); err != nil {
		b.Fatal(err)
	}
	var compressed int64
	for _, chunk := range writer.idx.Chunks {
		compressed += chunk.Bytes
	}
	b.ReportMetric(float64(compressed)/float64(b.N), "disk-B/frame")
}

func BenchmarkFrameEncodingNoTail(b *testing.B) {
	payload := make([]byte, 1024)
	s, err := New(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	s.tailLimit = 0
	w, err := s.Open("no-tail", "bench", model.KindJob, WriterOptions{MaxBytes: 1 << 40, MaxLine: 256 << 10})
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := w.Write(Stdout, payload, 0); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if err := s.Close("no-tail"); err != nil {
		b.Fatal(err)
	}
}

func BenchmarkFrameEncodingRandom(b *testing.B) {
	const payloadSize = 1024
	lines := make([][]byte, 1024)
	rng := rand.New(rand.NewPCG(3, 4))
	for i := range lines {
		lines[i] = make([]byte, payloadSize)
		for offset := 0; offset < payloadSize; offset += 8 {
			binary.LittleEndian.PutUint64(lines[i][offset:], rng.Uint64())
		}
	}
	s, err := New(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	w, err := s.Open("random", "bench", model.KindJob, WriterOptions{MaxBytes: 1 << 40, MaxLine: 256 << 10})
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(payloadSize)
	b.ReportAllocs()
	b.ResetTimer()
	i := 0
	for b.Loop() {
		if err := w.Write(Stdout, lines[i%len(lines)], 0); err != nil {
			b.Fatal(err)
		}
		i++
	}
	b.StopTimer()
	if err := s.Close("random"); err != nil {
		b.Fatal(err)
	}
	var compressed int64
	for _, chunk := range w.idx.Chunks {
		compressed += chunk.Bytes
	}
	b.ReportMetric(float64(compressed)/float64(b.N), "disk-B/frame")
}

// Run with -benchtime=1x; a maximum-sized frame exercises the oversized
// chunk, encoder-window, and one-frame pagination paths together.
func BenchmarkMaximumLineEncoding(b *testing.B) {
	if b.N != 1 {
		b.Skip("run with -benchtime=1x")
	}
	payload := make([]byte, maxFramePayload)
	rng := rand.New(rand.NewPCG(5, 6))
	for offset := 0; offset < len(payload); offset += 8 {
		binary.LittleEndian.PutUint64(payload[offset:], rng.Uint64())
	}
	s, err := New(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	w, err := s.Open("maximum", "bench", model.KindJob, WriterOptions{MaxBytes: 32 << 20, MaxLine: maxFramePayload})
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := w.Write(Stdout, payload, 0); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if err := s.Close("maximum"); err != nil {
		b.Fatal(err)
	}
	frames, err := s.Read("maximum", 0, 1)
	if err != nil || len(frames) != 1 || !bytes.Equal(frames[0].Payload, payload) {
		b.Fatalf("maximum frame round-trip failed: frames=%d error=%v", len(frames), err)
	}
	b.ReportMetric(float64(w.idx.Chunks[0].Bytes), "disk-B")
}

// Run with -benchtime=1x to measure memory held by concurrent active writers.
func BenchmarkActiveWriters64(b *testing.B) {
	if b.N != 1 {
		b.Skip("run with -benchtime=1x")
	}
	s, err := New(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	for range 64 {
		runID := fmt.Sprintf("run-%d", len(s.writers))
		w, err := s.Open(runID, "bench", model.KindJob, WriterOptions{MaxBytes: 1 << 40, MaxLine: 256 << 10})
		if err != nil {
			b.Fatal(err)
		}
		if err := w.Write(Stdout, bytes.Repeat([]byte("x"), 1024), 0); err != nil {
			b.Fatal(err)
		}
	}
	runtime.GC()
	var heap runtime.MemStats
	runtime.ReadMemStats(&heap)
	b.ReportMetric(float64(heap.HeapAlloc)/(1<<20), "heap-MiB")
	for runID := range s.writers {
		if err := s.Close(runID); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkArchivedStore(b *testing.B, incompressible bool) *Store {
	b.Helper()
	dir := b.TempDir()
	db, err := logdb.Open(dir)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	s, err := New(filepath.Join(dir, "logs"))
	if err != nil {
		b.Fatal(err)
	}
	s.AttachDB(db)
	payload := bytes.Repeat([]byte("x"), 4<<10)
	rng := rand.New(rand.NewPCG(1, 2))
	for chunkNumber := 1; chunkNumber <= 20; chunkNumber++ {
		frames := make([]Frame, 250)
		for i := range frames {
			framePayload := payload
			if incompressible {
				framePayload = make([]byte, len(payload))
				for offset := 0; offset < len(framePayload); offset += 8 {
					binary.LittleEndian.PutUint64(framePayload[offset:], rng.Uint64())
				}
			}
			frames[i] = Frame{Sequence: uint64((chunkNumber-1)*250 + i + 1), Timestamp: time.Now(), Stream: Stdout, Payload: framePayload}
		}
		blob, err := encodeFrames(frames)
		if err != nil {
			b.Fatal(err)
		}
		chunk := logdb.Chunk{Number: chunkNumber, First: frames[0].Sequence, Last: frames[len(frames)-1].Sequence, RawBytes: int64(len(frames) * (len(payload) + 24)), Blob: blob}
		if err := db.PutChunks(b.Context(), "archived", "bench", model.KindJob, time.Now(), []logdb.Chunk{chunk}); err != nil {
			b.Fatal(err)
		}
	}
	return s
}

func BenchmarkArchivedBacklogPage(b *testing.B) {
	s := benchmarkArchivedStore(b, false)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		frames, err := s.Read("archived", 0, 5000)
		if err != nil || len(frames) < 4000 || len(frames) >= 5000 {
			b.Fatalf("page returned %d frames: %v", len(frames), err)
		}
	}
}

func BenchmarkArchivedStreamPage(b *testing.B) {
	s := benchmarkArchivedStore(b, false)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		frames, err := s.ReadStreamContext(b.Context(), "archived", 0, 5000)
		if err != nil || len(frames) < 200 || len(frames) > 300 {
			b.Fatalf("stream page returned %d frames: %v", len(frames), err)
		}
	}
}

func BenchmarkArchivedStreamBacklog(b *testing.B) {
	s := benchmarkArchivedStore(b, false)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		reader := s.NewStreamReader("archived")
		var after uint64
		for {
			frames, err := reader.ReadContext(b.Context(), after, 5000)
			if err != nil {
				reader.Close()
				b.Fatal(err)
			}
			if len(frames) == 0 {
				break
			}
			after = frames[len(frames)-1].Sequence
		}
		reader.Close()
		if after != 5000 {
			b.Fatalf("backlog ended at %d", after)
		}
	}
}

// Run with -benchtime=1x to measure 64 simultaneous SSE-style readers.
func BenchmarkConcurrentStreamBacklog64(b *testing.B) {
	if b.N != 1 {
		b.Skip("run with -benchtime=1x")
	}
	s := benchmarkArchivedStore(b, false)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		var wg sync.WaitGroup
		errs := make(chan error, 64)
		for range 64 {
			wg.Go(func() {
				reader := s.NewStreamReader("archived")
				defer reader.Close()
				var after uint64
				for {
					frames, err := reader.ReadContext(b.Context(), after, 5000)
					if err != nil {
						errs <- err
						return
					}
					if len(frames) == 0 {
						break
					}
					after = frames[len(frames)-1].Sequence
				}
				if after != 5000 {
					errs <- fmt.Errorf("backlog ended at %d", after)
				}
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			b.Fatal(err)
		}
	}
}

func BenchmarkArchivedRawDownload(b *testing.B) {
	s := benchmarkArchivedStore(b, false)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := s.Raw("archived", io.Discard); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkArchivedRawDownloadIncompressible(b *testing.B) {
	s := benchmarkArchivedStore(b, true)
	b.SetBytes(20 << 20)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := s.Raw("archived", io.Discard); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReadLateFileChunk(b *testing.B) {
	s, err := New(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	dir := filepath.Join(s.root, "run")
	if err := os.Mkdir(dir, 0o700); err != nil {
		b.Fatal(err)
	}
	idx := index{Version: int(Version)}
	for i := 1; i <= 256; i++ {
		blob, err := encodeFrames([]Frame{{Sequence: uint64(i), Stream: Stdout, Payload: []byte("line")}})
		if err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%06d.zst", i)), blob, 0o600); err != nil {
			b.Fatal(err)
		}
		idx.Chunks = append(idx.Chunks, chunkMeta{Number: i, First: uint64(i), Last: uint64(i)})
	}
	data, err := json.Marshal(idx)
	if err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), data, 0o600); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		frames, err := s.Read("run", 255, 1)
		if err != nil || len(frames) != 1 || frames[0].Sequence != 256 {
			b.Fatalf("late frame: %v, %v", frames, err)
		}
	}
}

func BenchmarkLiveTailEviction(b *testing.B) {
	s := &Store{tailLimit: 64 << 20}
	w := &Writer{store: s}
	payload := make([]byte, 64)
	for i := range historyLimit {
		w.appendHistory(Frame{Sequence: uint64(i + 1), Payload: payload})
	}
	b.ReportAllocs()
	b.ResetTimer()
	seq := uint64(historyLimit)
	for b.Loop() {
		seq++
		w.appendHistory(Frame{Sequence: seq, Payload: payload})
	}
}
