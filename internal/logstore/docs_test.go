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
	if err := w.Pipe(Stdout, bytes.NewReader(manyLines(10))); err != nil { // 1 failed + 9 discarded
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
	// 1 (one), 2 (failed, consumed), 3 (summary), 4 (after) -> summary is 3.
	if frames[1].Sequence != 3 || frames[2].Sequence != 4 {
		t.Fatalf("sequences %d,%d: want the failed frame to be the only skipped number", frames[1].Sequence, frames[2].Sequence)
	}
}

func TestDocsDescribeTheFailedFrameSequence(t *testing.T) {
	docContains(t, "http-api.md", "consumed a sequence number but was not stored")
	docContains(t, "operations.md", "the single frame whose write failed does")
}

// N2: the quarantine copy is named <run>-<chunk>.zst.corrupt (preserveCorrupt).
func TestDocsNameTheQuarantineCopyLikeTheCode(t *testing.T) {
	docContains(t, "operations.md", "<run>-<chunk>.zst.corrupt")
	if b, _ := os.ReadFile("../../docs/operations.md"); strings.Contains(string(b), "<run>-<chunk>.corrupt") {
		t.Error("docs/operations.md names the quarantine copy without .zst")
	}
}
