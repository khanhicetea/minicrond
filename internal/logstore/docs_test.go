package logstore

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

// The operator docs describe contracts that tests elsewhere pin; these checks
// keep the text from drifting from the code (review N1, N2, N6, N7).
func docContains(t *testing.T, file string, phrases ...string) {
	t.Helper()
	b, err := os.ReadFile("../../docs/" + file)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(b)), " ")
	for _, p := range phrases {
		if !strings.Contains(text, strings.Join(strings.Fields(p), " ")) {
			t.Errorf("docs/%s does not say %q", file, p)
		}
	}
}

// N1: the failed frame consumes a sequence; discards do not. Pin the code
// behavior the docs describe.
func TestCaptureFailureSkipsExactlyTheFailedSequence(t *testing.T) {
	setCaptureRetry(t, time.Hour, time.Hour)
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Open("run", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Pipe(Stdout, bytes.NewReader([]byte("one\n"))); err != nil {
		t.Fatal(err)
	}
	breakStorage(t, w)
	if err := w.Pipe(Stdout, bytes.NewReader(manyLines(10))); err != nil { // one failed write of 10 lines
		t.Fatal(err)
	}
	w.mu.Lock()
	w.retryAt = time.Now()
	w.mu.Unlock()
	if err := w.Pipe(Stdout, bytes.NewReader([]byte("after\n"))); err != nil {
		t.Fatal(err)
	}
	if err := s.Close("run"); err != nil {
		t.Fatal(err)
	}
	frames, err := s.Read("run", 0, 100)
	if err != nil || len(frames) != 3 {
		t.Fatalf("frames = %v, %v", frames, err)
	}
	// 1 (one), 2-11 (one read, flushed together, failed: consumed),
	// 12 (summary), 13 (after). Lines discarded later consume nothing.
	if frames[1].Sequence != 12 || frames[2].Sequence != 13 {
		t.Fatalf("sequences %d,%d: want only the failed write's frames to be skipped", frames[1].Sequence, frames[2].Sequence)
	}
}

func TestDocsDescribeTheFailedFrameSequence(t *testing.T) {
	docContains(t, "http-api.md", "consumed sequence numbers but were not stored")
	docContains(t, "operations.md", "the frames of the one write that failed do")
}

// N2: the quarantine copy is named <run>-<chunk>.zst.corrupt (preserveCorrupt).
func TestDocsNameTheQuarantineCopyLikeTheCode(t *testing.T) {
	docContains(t, "operations.md", "<run>-<chunk>.zst.corrupt")
	if b, _ := os.ReadFile("../../docs/operations.md"); strings.Contains(string(b), "<run>-<chunk>.corrupt") {
		t.Error("docs/operations.md names the quarantine copy without .zst")
	}
}

// N6: zero means "default", not "disabled".
func TestDocsSayZeroSyncWindowSelectsTheDefault(t *testing.T) {
	docContains(t, "configuration.md", "`0` selects the default and does not disable")
}

// N7: the accepted downside of cursor-read following is recorded.
func TestDocsRecordTheFollowPollingDownside(t *testing.T) {
	docContains(t, "operations.md", "re-decodes the active chunk from its start", "Accepted downside of following a run")
}

// ADR-20 contracts that operators rely on stay documented.
func TestLogArchiveWritePathIsDocumented(t *testing.T) {
	docContains(t, "operations.md", "log-archive/<run_id>/", "Back up the whole data directory")
	docContains(t, "operations.md", "Pipe EOF and run completion join the same group", "a finished run's last output included")
	docContains(t, "configuration.md", "database pages plus the chunk files in")
	if fileChunkMin != 64<<10 || archiveRunBatch != 32 || maxDeferredSeals != 4096 {
		t.Fatalf("ADR-20 constants changed (%d/%d/%d): update docs/adr/0020-log-archive-write-path.md, docs/operations.md and docs/releases/v0.6.0.md",
			fileChunkMin, archiveRunBatch, maxDeferredSeals)
	}
}
