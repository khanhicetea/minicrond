package logstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/khanhicetea/minicrond/internal/model"
)

// zstdFrames compresses raw bytes as one zstd stream.
func zstdStream(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// frameWithVersion builds one raw frame whose header carries the given version.
func frameWithVersion(version byte, seq uint64, payload string) []byte {
	var raw bytes.Buffer
	if err := encode(&raw, Frame{Sequence: seq, Stream: Stdout, Payload: []byte(payload)}); err != nil {
		panic(err)
	}
	b := raw.Bytes()
	b[0] = version
	return b
}

func TestSalvageFramesClassifiesStreams(t *testing.T) {
	valid, err := encodeFrames([]Frame{{Sequence: 1, Stream: Stdout, Payload: []byte("a")}, {Sequence: 2, Stream: Stdout, Payload: []byte("b")}})
	if err != nil {
		t.Fatal(err)
	}
	empty, err := encodeFrames(nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		blob       []byte
		frames     int
		wantErr    bool
		wantTorn   bool
		wantNilErr bool
	}{
		{"zero bytes", nil, 0, false, false, true},
		{"valid empty stream", empty, 0, false, false, true},
		{"valid", valid, 2, false, false, true},
		{"not zstd", []byte("this is definitely not zstd data"), 0, true, false, false},
		{"unsupported frame version", zstdStream(t, frameWithVersion(9, 1, "x")), 0, true, false, false},
		{"torn", valid[:len(valid)-6], 0, true, true, false},
		{"valid prefix then unsupported frame", zstdStream(t, append(frameWithVersion(Version, 1, "ok"), frameWithVersion(9, 2, "bad")...)), 1, true, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			frames, err := salvageFrames(tc.blob)
			if tc.wantNilErr != (err == nil) {
				t.Fatalf("err = %v", err)
			}
			if tc.wantTorn && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("torn stream not reported as ErrUnexpectedEOF: %v", err)
			}
			if !tc.wantTorn && err != nil && errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("corruption misreported as a torn tail: %v", err)
			}
			if tc.name != "torn" && len(frames) != tc.frames {
				t.Fatalf("recovered %d frames, want %d", len(frames), tc.frames)
			}
		})
	}
}

func writeOrphan(t *testing.T, dir, run, chunk string, data []byte) string {
	t.Helper()
	buffer := filepath.Join(dir, "logs", run)
	if err := os.MkdirAll(buffer, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(buffer, chunk)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A05: a nonempty undecodable orphan chunk is never deleted as "recovered".
func TestCorruptOrphanChunkIsPreservedWithError(t *testing.T) {
	for _, tc := range []struct {
		name string
		blob []byte
	}{
		{"malformed zstd", []byte("not a zstd stream at all, just bytes")},
		{"unsupported frame version", zstdStream(t, frameWithVersion(9, 1, "from the future"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, db, dir := newArchiveStore(t)
			path := writeOrphan(t, dir, "run", "000001.zst", tc.blob)
			err := s.ArchiveOrphans()
			if err == nil {
				t.Fatal("a corrupt chunk was reported as successfully archived")
			}
			kept, readErr := os.ReadFile(path)
			if readErr != nil || !bytes.Equal(kept, tc.blob) {
				t.Fatalf("corrupt chunk was not preserved: %v", readErr)
			}
			if _, chunks, _, _ := db.Stats(t.Context()); chunks != 0 {
				t.Fatalf("archived %d chunks from corrupt input", chunks)
			}
			// Repeated failures end in the quarantine, preserved, not deleted.
			for range orphanQuarantineAfter {
				_ = s.ArchiveOrphans()
			}
			quarantined, readErr := os.ReadFile(filepath.Join(dir, "logs", QuarantineDir, "run", "000001.zst"))
			if readErr != nil || !bytes.Equal(quarantined, tc.blob) {
				t.Fatalf("corrupt chunk missing from the quarantine: %v", readErr)
			}
		})
	}
}

// Valid chunks around a corrupt one are still archived; the corrupt one stays
// and the sweep reports it.
func TestCorruptChunkDoesNotBlockValidSiblings(t *testing.T) {
	s, db, dir := newArchiveStore(t)
	good, err := encodeFrames([]Frame{{Sequence: 1, Stream: Stdout, Payload: []byte("good")}})
	if err != nil {
		t.Fatal(err)
	}
	writeOrphan(t, dir, "run", "000001.zst", good)
	bad := writeOrphan(t, dir, "run", "000002.zst", []byte("garbage garbage garbage"))
	next, err := encodeFrames([]Frame{{Sequence: 2, Stream: Stdout, Payload: []byte("also good")}})
	if err != nil {
		t.Fatal(err)
	}
	writeOrphan(t, dir, "run", "000003.zst", next)
	if err := s.ArchiveOrphans(); err == nil {
		t.Fatal("expected the corrupt chunk to be reported")
	}
	if _, err := os.Stat(bad); err != nil {
		t.Fatalf("corrupt chunk gone: %v", err)
	}
	if _, chunks, _, _ := db.Stats(t.Context()); chunks != 2 {
		t.Fatalf("archived %d chunks, want the 2 valid ones", chunks)
	}
}

// The valid-prefix salvage of a torn final chunk keeps working and stays quiet.
func TestTornChunkValidPrefixIsStillSalvaged(t *testing.T) {
	s, _, dir := newArchiveStore(t)
	crashed, err := New(filepath.Join(dir, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := crashed.Open("run", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"one", "two", "three"} {
		if err := w.Write(Stdout, []byte(line), 0); err != nil {
			t.Fatal(err)
		}
	}
	// Daemon crash: no Close. The OS then tears the tail of the last block.
	path := w.chunkPath(1)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, info.Size()-3); err != nil {
		t.Fatal(err)
	}
	if err := s.ArchiveOrphans(); err != nil {
		t.Fatalf("torn tail must be salvaged without error: %v", err)
	}
	frames, err := s.Read("run", 0, 10)
	if err != nil || len(frames) < 2 || string(frames[0].Payload) != "one" {
		t.Fatalf("salvaged %v, %v", frames, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "logs", "run")); !os.IsNotExist(err) {
		t.Fatalf("buffer not removed after a successful salvage: %v", err)
	}
}

// Corruption after a valid prefix: the prefix is archived and the original
// bytes are preserved, never silently dropped.
func TestPartlyCorruptChunkArchivesPrefixAndPreservesOriginal(t *testing.T) {
	s, db, dir := newArchiveStore(t)
	blob := zstdStream(t, append(frameWithVersion(Version, 1, "keep"), frameWithVersion(9, 2, "damaged")...))
	writeOrphan(t, dir, "run", "000001.zst", blob)
	if err := s.ArchiveOrphans(); err != nil {
		t.Fatalf("prefix salvage failed: %v", err)
	}
	frames, err := s.Read("run", 0, 10)
	if err != nil || len(frames) != 1 || string(frames[0].Payload) != "keep" {
		t.Fatalf("archived prefix = %v, %v", frames, err)
	}
	if _, chunks, _, _ := db.Stats(t.Context()); chunks != 1 {
		t.Fatalf("archived %d chunks", chunks)
	}
	kept, err := os.ReadFile(filepath.Join(dir, "logs", QuarantineDir, "run-000001.zst.corrupt"))
	if err != nil || !bytes.Equal(kept, blob) {
		t.Fatalf("original bytes not preserved: %v", err)
	}
}

// A genuinely empty chunk is not an error and leaves nothing behind.
func TestEmptyOrphanChunkIsNotCorruption(t *testing.T) {
	s, _, dir := newArchiveStore(t)
	writeOrphan(t, dir, "run", "000001.zst", nil)
	if err := s.ArchiveOrphans(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "logs", "run")); !os.IsNotExist(err) {
		t.Fatalf("empty buffer kept: %v", err)
	}
}

// A11: a pipe-level truncation reaches the writer stats, the final index, and
// the frame.
func TestPipeTruncationSetsRunLevelFlag(t *testing.T) {
	s, _, dir := newArchiveStore(t)
	w, err := s.Open("run", "job", model.KindJob, WriterOptions{MaxLine: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Pipe(Stdout, bytes.NewReader([]byte("123456789\n"))); err != nil {
		t.Fatal(err)
	}
	if _, truncated := w.Stats(); !truncated {
		t.Fatal("writer stats do not report the pipe truncation")
	}
	// Read the final index before archival removes the buffer.
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "logs", "run", "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idx index
	if err := json.Unmarshal(data, &idx); err != nil || !idx.Truncated || !idx.Final {
		t.Fatalf("final index = %+v, %v", idx, err)
	}
	frames, err := s.Read("run", 0, 10)
	if err != nil || len(frames) != 1 || string(frames[0].Payload) != "1234" || frames[0].Flags&FlagTruncated == 0 {
		t.Fatalf("frame = %+v, %v", frames, err)
	}
}
