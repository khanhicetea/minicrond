package logstore

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// A buffer whose own files keep failing to archive is moved aside, preserved,
// instead of failing every sweep forever.
func TestUnarchivableOrphanIsQuarantined(t *testing.T) {
	s, _, dir := newArchiveStore(t)
	buffer := filepath.Join(dir, "logs", "broken")
	blob, err := encodeFrames([]Frame{{Sequence: 1, Stream: Stdout, Payload: []byte("keep me")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(chunkPath(buffer, 1), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath(buffer), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= orphanQuarantineAfter; attempt++ {
		if err := s.ArchiveOrphans(); err == nil {
			t.Fatalf("sweep %d: expected invalid index error", attempt)
		}
		if attempt < orphanQuarantineAfter && len(bufferFiles(t, dir, "broken")) != 2 {
			t.Fatalf("sweep %d: buffer moved too early: %v", attempt, bufferFiles(t, dir, "broken"))
		}
	}
	if left := bufferFiles(t, dir, "broken"); len(left) != 0 {
		t.Fatalf("buffer still in the sweep path: %v", left)
	}
	if index, err := os.ReadFile(filepath.Join(dir, "logs", QuarantineDir, "broken", "broken.json")); err != nil || string(index) != "{" {
		t.Fatalf("quarantined index not preserved: %q, %v", index, err)
	}
	kept, err := os.ReadFile(filepath.Join(dir, "logs", QuarantineDir, "broken", "broken.000001.zst"))
	if err != nil || !bytes.Equal(kept, blob) {
		t.Fatalf("quarantined chunk not preserved: %v", err)
	}
	if err := s.ArchiveOrphans(); err != nil {
		t.Fatalf("sweep after quarantine: %v", err)
	}
}
