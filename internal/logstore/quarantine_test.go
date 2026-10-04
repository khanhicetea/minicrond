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
	if err := os.Mkdir(buffer, 0o700); err != nil {
		t.Fatal(err)
	}
	blob, err := encodeFrames([]Frame{{Sequence: 1, Stream: Stdout, Payload: []byte("keep me")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(buffer, "000001.zst"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(buffer, "index.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= orphanQuarantineAfter; attempt++ {
		if err := s.ArchiveOrphans(); err == nil {
			t.Fatalf("sweep %d: expected invalid index error", attempt)
		}
		_, statErr := os.Stat(buffer)
		if attempt < orphanQuarantineAfter && statErr != nil {
			t.Fatalf("sweep %d: buffer moved too early: %v", attempt, statErr)
		}
	}
	if _, err := os.Stat(buffer); !os.IsNotExist(err) {
		t.Fatalf("buffer still in the sweep path: %v", err)
	}
	kept, err := os.ReadFile(filepath.Join(dir, "logs", QuarantineDir, "broken", "000001.zst"))
	if err != nil || !bytes.Equal(kept, blob) {
		t.Fatalf("quarantined chunk not preserved: %v", err)
	}
	if err := s.ArchiveOrphans(); err != nil {
		t.Fatalf("sweep after quarantine: %v", err)
	}
}
