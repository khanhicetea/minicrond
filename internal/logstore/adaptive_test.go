package logstore

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/logdb"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/klauspost/compress/zstd"
)

func numberedLines(prefix string, n int) []byte {
	var b bytes.Buffer
	for i := range n {
		fmt.Fprintf(&b, "%s line %04d with some ordinary log text\n", prefix, i)
	}
	return b.Bytes()
}

func archivedChunks(t *testing.T, ldb *logdb.LogDB, runID string) []logdb.Chunk {
	t.Helper()
	var out []logdb.Chunk
	if err := ldb.EachChunk(t.Context(), runID, 0, func(c logdb.Chunk) (bool, error) { out = append(out, c); return true, nil }); err != nil {
		t.Fatal(err)
	}
	return out
}

func waitArchived(t *testing.T, dir, runID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "logs", runID)); errors.Is(err, os.ErrNotExist) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s was not archived", runID)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A run below plainLimit never creates an encoder; its single plain chunk is
// handed to the archiver in memory and stored compressed in one pass.
func TestSmallRunStaysPlainAndArchivesCompressed(t *testing.T) {
	s, ldb, dir := newArchiveStore(t)
	s.StartArchiver()
	t.Cleanup(func() { _ = s.StopArchiver(t.Context()) })
	w, err := s.Open("small", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	input := numberedLines("small", 40) // about 2 KiB
	if err := w.Pipe(Stdout, bytes.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	if w.enc != nil || !w.plain {
		t.Fatal("a small run created an encoder")
	}
	hot := readAll(t, s, "small") // read through the hot plain chunk
	checkSequence(t, hot, 40)
	if err := s.Seal("small"); err != nil {
		t.Fatal(err)
	}
	waitArchived(t, dir, "small")
	if n := s.archivedFromImage.Load(); n != 1 {
		t.Fatalf("archived from image %d times, want 1 (no read-back)", n)
	}
	s.mu.Lock()
	left := len(s.archiver.sealed)
	s.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d sealed images left after archival", left)
	}
	chunks := archivedChunks(t, ldb, "small")
	if len(chunks) != 1 || chunks[0].RawBytes == 0 {
		t.Fatalf("archived chunks: %+v", chunks)
	}
	if plainSize := int64(len(plainFrameHeader)) + 3 + chunks[0].RawBytes + 3; int64(len(chunks[0].Blob)) >= plainSize {
		t.Fatalf("archive stored %d bytes for a %d-byte plain chunk: not recompressed", len(chunks[0].Blob), plainSize)
	}
	archived := readAll(t, s, "small")
	checkSequence(t, archived, 40)
	for i := range archived {
		if !bytes.Equal(archived[i].Payload, hot[i].Payload) {
			t.Fatalf("frame %d changed through archival", i+1)
		}
	}
}

// Output past plainLimit seals the plain chunk and continues compressed; every
// frame stays readable, in order, while running, sealed and archived.
func TestPromotionAtPlainLimit(t *testing.T) {
	s, ldb, _ := newArchiveStore(t)
	w, err := s.Open("chatty", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	input := numberedLines("chatty", 2000) // about 100 KiB
	if err := w.Pipe(Stdout, bytes.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	if w.enc == nil || w.plain || !w.promoted {
		t.Fatal("chatty run was not promoted")
	}
	if len(w.idx.Chunks) != 1 || !w.idx.Chunks[0].Plain || w.idx.Chunks[0].Raw > int64(plainLimit) {
		t.Fatalf("plain chunk after promotion: %+v", w.idx.Chunks)
	}
	checkSequence(t, readAll(t, s, "chatty"), 2000)
	if err := s.Close("chatty"); err != nil {
		t.Fatal(err)
	}
	chunks := archivedChunks(t, ldb, "chatty")
	if len(chunks) != 2 {
		t.Fatalf("archived %d chunks, want plain + compressed", len(chunks))
	}
	checkSequence(t, readAll(t, s, "chatty"), 2000)
}

// One line larger than plainLimit, as the first output: the empty plain chunk
// switches to compressed in place instead of leaving an empty chunk behind.
func TestLongFirstLinePromotesInPlace(t *testing.T) {
	s, _, _ := newArchiveStore(t)
	w, err := s.Open("long", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	long := append(bytes.Repeat([]byte("x"), plainLimit+100), '\n')
	if err := w.Pipe(Stdout, bytes.NewReader(long)); err != nil {
		t.Fatal(err)
	}
	if w.chunk != 1 || w.plain || w.enc == nil {
		t.Fatalf("chunk %d plain=%v: want chunk 1 switched to compressed", w.chunk, w.plain)
	}
	if err := s.Close("long"); err != nil {
		t.Fatal(err)
	}
	frames := readAll(t, s, "long")
	if len(frames) != 1 || len(frames[0].Payload) != plainLimit+100 {
		t.Fatalf("frames: %d", len(frames))
	}
}

// A worker checkpoint releases a promoted writer's encoder; capture continues
// plain and promotes again only if output outgrows plainLimit again.
func TestCheckpointReleasesEncoder(t *testing.T) {
	s, _, _ := newArchiveStore(t)
	w, err := s.Open("worker", "w", model.KindWorker, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Pipe(Stdout, bytes.NewReader(numberedLines("burst", 1000))); err != nil {
		t.Fatal(err)
	}
	if w.enc == nil {
		t.Fatal("not promoted")
	}
	if err := w.Flush(s); err != nil {
		t.Fatal(err)
	}
	if w.enc != nil || !w.plain || w.promoted {
		t.Fatal("checkpoint kept the encoder")
	}
	if err := w.Pipe(Stdout, bytes.NewReader([]byte("quiet now\n"))); err != nil {
		t.Fatal(err)
	}
	if w.enc != nil {
		t.Fatal("one quiet line recreated the encoder")
	}
	if err := w.Flush(s); err != nil { // an empty compressed chunk switches in place too
		t.Fatal(err)
	}
	if err := s.Close("worker"); err != nil {
		t.Fatal(err)
	}
	frames := readAll(t, s, "worker")
	checkSequence(t, frames, 1001)
	if string(frames[1000].Payload) != "quiet now" {
		t.Fatalf("last frame %q", frames[1000].Payload)
	}
}

// A run deleted while queued for archival is not resurrected from the image
// handed to the archiver.
func TestSealedImageOfDeletedRunIsNotArchived(t *testing.T) {
	s, _, dir := newArchiveStore(t)
	img := append(append([]byte{}, plainFrameHeader...), 1, 0, 0)
	_, _, ok := s.sealedShortRun(filepath.Join(dir, "logs", "gone"), &sealedRun{meta: chunkMeta{Number: 1, First: 1, Last: 1, Plain: true}, image: img})
	if ok {
		t.Fatal("archived a deleted run from its sealed image")
	}
}

// After a daemon crash, a plain chunk (still open, or torn mid-block) and a
// chunk holding a plain frame followed by a compressed one are salvaged and
// read with the store's own decoders.
func TestPlainAndMixedChunksDecode(t *testing.T) {
	plainOf := func(frames []Frame, last bool) []byte {
		img := append([]byte{}, plainFrameHeader...)
		blk := len(img)
		img = append(img, 0, 0, 0)
		for _, f := range frames {
			img = appendFrame(img, f)
		}
		putRawBlockHeader(img[blk:], len(img)-blk-3, false)
		if last {
			img = append(img, 1, 0, 0)
		}
		return img
	}
	mk := func(first uint64, n, size int) []Frame {
		out := make([]Frame, n)
		for i := range out {
			out[i] = Frame{Sequence: first + uint64(i), Timestamp: time.Unix(1700000000, 0).UTC(), Payload: bytes.Repeat([]byte{'a' + byte(i%26)}, size)}
		}
		return out
	}
	small := mk(1, 3, 40)
	compressed, err := encodeFrames(mk(4, 500, 60))
	if err != nil {
		t.Fatal(err)
	}
	mixed := append(plainOf(small, true), compressed...)
	if got, err := salvageFrames(mixed); err != nil || len(got) != 503 || got[502].Sequence != 503 {
		t.Fatalf("mixed salvage: %d frames, %v", len(got), err)
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(2), zstd.WithDecoderMaxMemory(32<<20), zstd.WithDecoderMaxWindow(16<<20))
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	for _, blob := range [][]byte{mixed, plainOf(small, true), compressed} {
		if err := dec.Reset(bytes.NewReader(blob)); err != nil {
			t.Fatal(err)
		}
		for {
			if _, err := decode(dec); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatalf("read path: %v", err)
			}
		}
	}
	open := plainOf(small, false)
	if got, err := salvageFrames(open); len(got) != 3 || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("open plain chunk: %d frames, %v", len(got), err)
	}
	torn := plainOf(append(small, mk(4, 1, 5000)...), false)
	if got, err := salvageFrames(torn[:len(torn)-100]); len(got) != 0 || !errors.Is(err, io.ErrUnexpectedEOF) {
		// One block holds every frame: a block cut short yields nothing, as
		// for a compressed block. Pipe writes one block per read, so a crash
		// loses at most the read being written.
		t.Fatalf("torn plain chunk: %d frames, %v", len(got), err)
	}
}

func TestPlainRecordsRejectsMalformedChunks(t *testing.T) {
	good := append(append([]byte{}, plainFrameHeader...), 2<<3, 0, 0, 'h', 'i', 1, 0, 0)
	if rec, err := plainRecords(good); err != nil || string(rec) != "hi" {
		t.Fatalf("good chunk: %q, %v", rec, err)
	}
	for name, bad := range map[string][]byte{
		"compressed frame": {0x28, 0xb5, 0x2f, 0xfd, 0x20, 0},
		"no last block":    good[:len(good)-3],
		"short block":      append(append([]byte{}, plainFrameHeader...), 9<<3, 0, 0, 'x'),
		"rle block":        append(append([]byte{}, plainFrameHeader...), 1<<3|1<<1|1, 0, 0, 'x'),
		"trailing data":    append(append([]byte{}, good...), 0),
	} {
		if _, err := plainRecords(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
