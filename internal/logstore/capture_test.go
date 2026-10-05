package logstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

// logCapture collects slog output so tests can assert there is no error storm.
type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	c.lines = append(c.lines, r.Level.String()+" "+r.Message)
	c.mu.Unlock()
	return nil
}
func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *logCapture) WithGroup(string) slog.Handler            { return c }
func (c *logCapture) count(substr string) (n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.lines {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	c := &logCapture{}
	old := slog.Default()
	slog.SetDefault(slog.New(c))
	t.Cleanup(func() { slog.SetDefault(old) })
	return c
}

func setCaptureRetry(t *testing.T, min, max time.Duration) {
	t.Helper()
	oldMin, oldMax := captureRetryMin, captureRetryMax
	captureRetryMin, captureRetryMax = min, max
	t.Cleanup(func() { captureRetryMin, captureRetryMax = oldMin, oldMax })
}

// breakStorage makes the writer's chunk file fail on every further write, as a
// vanished or full device would.
func breakStorage(t *testing.T, w *Writer) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.file.Close(); err != nil {
		t.Fatal(err)
	}
}

// ADR-8 2A: a write failure never stops the pipe from draining the child's
// output; the lines are discarded, capture is flagged incomplete, and the log
// reports the failure once rather than per line.
func TestPipeKeepsDrainingWhenLogWritesFail(t *testing.T) {
	logs := captureLogs(t)
	setCaptureRetry(t, time.Hour, time.Hour)
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Open("run", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Pipe(Stdout, bytes.NewReader([]byte("before\n"))); err != nil {
		t.Fatal(err)
	}
	breakStorage(t, w)
	input := bytes.NewReader(manyLines(5000))
	if err := w.Pipe(Stdout, input); err != nil {
		t.Fatalf("a storage failure must not reach the pump: %v", err)
	}
	if input.Len() != 0 {
		t.Fatalf("%d bytes of child output left undrained", input.Len())
	}
	status := w.Capture()
	if !status.Degraded || status.DroppedFrames != 5000 || status.DroppedBytes == 0 || status.Err == nil {
		t.Fatalf("capture status = %+v", status)
	}
	if _, truncated := w.Stats(); !truncated {
		t.Fatal("run is not flagged truncated although output was discarded")
	}
	if n := logs.count("log storage failed"); n != 1 {
		t.Fatalf("%d storage failure reports for 5000 discarded lines, want 1", n)
	}
	if s.CaptureFailures() != 1 {
		t.Fatalf("capture failures = %d", s.CaptureFailures())
	}
	// Accepted output before the failure is intact.
	frames, err := s.Read("run", 0, 10)
	if err != nil || len(frames) != 1 || string(frames[0].Payload) != "before" {
		t.Fatalf("frames before the failure: %v, %v", frames, err)
	}
	// Even though the final chunk could not be sealed, evidence reaches the index.
	_ = s.Close("run")
	var idx index
	data, err := os.ReadFile(filepath.Join(w.dir, "index.json"))
	if err != nil || json.Unmarshal(data, &idx) != nil {
		t.Fatalf("index unreadable: %v", err)
	}
	if !idx.Truncated || idx.DroppedFrames != 5000 || !idx.Final {
		t.Fatalf("index evidence = %+v", idx)
	}
}

// Capture resumes on a fresh chunk once storage works, records what was lost
// in a system line, and never reports the run as completely captured.
func TestCaptureRecoversAndRecordsLoss(t *testing.T) {
	logs := captureLogs(t)
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
	if err := w.Pipe(Stdout, bytes.NewReader(manyLines(10))); err != nil {
		t.Fatal(err)
	}
	// Within the retry delay nothing is attempted: later lines are only counted.
	if err := w.Pipe(Stdout, bytes.NewReader([]byte("lost too\n"))); err != nil {
		t.Fatal(err)
	}
	if got := w.Capture().DroppedFrames; got != 11 {
		t.Fatalf("dropped %d, want 11", got)
	}
	w.mu.Lock()
	w.retryAt = time.Now() // the bounded retry is due
	w.mu.Unlock()
	if err := w.Pipe(Stdout, bytes.NewReader([]byte("after\n"))); err != nil {
		t.Fatal(err)
	}
	if status := w.Capture(); status.Degraded || status.DroppedFrames != 11 {
		t.Fatalf("capture did not recover: %+v", status)
	}
	if _, truncated := w.Stats(); !truncated {
		t.Fatal("recovered run must still be flagged truncated")
	}
	if logs.count("log capture recovered") != 1 {
		t.Fatalf("recovery was logged %d times", logs.count("log capture recovered"))
	}
	if err := s.Close("run"); err != nil {
		t.Fatal(err)
	}
	frames, err := s.Read("run", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, f := range frames {
		texts = append(texts, string(f.Payload))
	}
	if len(frames) != 3 || texts[0] != "one" || texts[2] != "after" ||
		!strings.Contains(texts[1], "11 lines") || frames[1].Stream != System || frames[1].Flags&FlagTruncated == 0 {
		t.Fatalf("frames after recovery = %q", texts)
	}
	for i := 1; i < len(frames); i++ {
		if frames[i].Sequence <= frames[i-1].Sequence {
			t.Fatalf("sequence went backwards: %v", frames)
		}
	}
	var idx index
	data, _ := os.ReadFile(filepath.Join(w.dir, "index.json"))
	if err := json.Unmarshal(data, &idx); err != nil || !idx.Truncated || idx.DroppedFrames != 11 {
		t.Fatalf("index = %+v, %v", idx, err)
	}
}

// Recovery attempts back off, so a persistently failing disk is probed rarely
// and a recovery attempt that fails does not reset the episode.
func TestCaptureRetryIsBoundedAndBacksOff(t *testing.T) {
	setCaptureRetry(t, 10*time.Millisecond, 80*time.Millisecond)
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Open("run", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	breakStorage(t, w)
	// Keep storage unusable for rotation too: occupy the next chunk path.
	for n := 2; n <= 40; n++ {
		if err := os.Mkdir(w.chunkPath(n), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	for time.Since(start) < 400*time.Millisecond {
		if err := w.capture(Stdout, []byte("x"), 0); err != nil {
			t.Fatal(err)
		}
	}
	w.mu.Lock()
	delay, chunk := w.retryDelay, w.chunk
	w.mu.Unlock()
	if delay != 80*time.Millisecond {
		t.Fatalf("retry delay %v did not back off to its cap", delay)
	}
	// Each attempt claims at most one new chunk number; a probe per line would
	// have walked all 40 occupied paths.
	if chunk > 1 {
		t.Fatalf("recovery moved to chunk %d while rotation was impossible", chunk)
	}
	if !w.Capture().Degraded {
		t.Fatal("capture reported healthy while rotation is impossible")
	}
}

// A failed fsync is evidence, not a reason to stop: the child keeps being
// drained, the run is flagged truncated, and the failure is reported once per
// episode.
func TestSyncFailureKeepsDrainingAndFlagsRun(t *testing.T) {
	logs := captureLogs(t)
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.SetGroupSync(time.Hour, 200) // sync about every 7 lines
	failing := true
	s.syncFile = func(f *os.File) error {
		if failing {
			return errors.New("injected fsync failure")
		}
		return f.Sync()
	}
	w, err := s.Open("run", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	input := bytes.NewReader(manyLines(500))
	if err := w.Pipe(Stdout, input); err != nil {
		t.Fatalf("sync failure reached the pump: %v", err)
	}
	if input.Len() != 0 {
		t.Fatal("input not drained")
	}
	status := w.Capture()
	if status.SyncFailures < 10 {
		t.Fatalf("sync failures = %d", status.SyncFailures)
	}
	if _, truncated := w.Stats(); !truncated {
		t.Fatal("durability could not be confirmed but the run is not flagged")
	}
	if n := logs.count("log fsync failed"); n != 1 {
		t.Fatalf("%d fsync failure reports, want 1 per episode", n)
	}
	failing = false
	if err := w.Pipe(Stdout, bytes.NewReader(manyLines(100))); err != nil {
		t.Fatal(err)
	}
	failing = true
	if err := w.Pipe(Stdout, bytes.NewReader(manyLines(100))); err != nil {
		t.Fatal(err)
	}
	if n := logs.count("log fsync failed"); n != 2 {
		t.Fatalf("a new episode after a successful sync must be reported again, got %d reports", n)
	}
	frames, err := s.Read("run", 0, 5000)
	if err != nil || len(frames) != 700 {
		t.Fatalf("frames written through despite sync failures: %d, %v", len(frames), err)
	}
}

// Stop requests and timeouts are executor concerns, but the pump must stay
// cheap while discarding so a full disk does not make a chatty child slow.
func TestDegradedPipeDiscardsWithoutPerLineWork(t *testing.T) {
	setCaptureRetry(t, time.Hour, time.Hour)
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Open("run", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	breakStorage(t, w)
	input := manyLines(200000)
	start := time.Now()
	if err := w.Pipe(Stdout, bytes.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("discarding 200000 lines took %v", elapsed)
	}
	if got := w.Capture().DroppedFrames; got != 200000 {
		t.Fatalf("dropped %d", got)
	}
}
