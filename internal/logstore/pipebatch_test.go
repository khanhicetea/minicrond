package logstore

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	"github.com/khanhicetea/minicrond/internal/model"
)

// checkingReader hands out one piece per Read, as a pipe returns what one
// child write made available. Before handing out the next piece it checks
// that every complete line of the earlier pieces is already readable, i.e.
// Pipe flushed its batch before it blocked on the next read.
type checkingReader struct {
	t      *testing.T
	s      *Store
	runID  string
	pieces [][]byte
	lines  int // complete lines handed out so far
}

func (r *checkingReader) Read(p []byte) (int, error) {
	frames, err := r.s.Read(r.runID, 0, 100000)
	if err != nil {
		r.t.Fatalf("read while piping: %v", err)
	}
	if len(frames) != r.lines {
		r.t.Fatalf("%d of %d lines visible before the next read: a batch waited for more input", len(frames), r.lines)
	}
	if len(r.pieces) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.pieces[0])
	r.lines += bytes.Count(r.pieces[0][:n], []byte{'\n'})
	r.pieces[0] = r.pieces[0][n:]
	if len(r.pieces[0]) == 0 {
		r.pieces = r.pieces[1:]
	}
	return n, nil
}

func TestPipeFlushesEachReadBeforeBlocking(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Open("run", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var pieces [][]byte
	for i := range 50 {
		var b bytes.Buffer
		for j := range i * 7 % 40 {
			fmt.Fprintf(&b, "burst %d line %d\n", i, j)
		}
		pieces = append(pieces, b.Bytes())
	}
	r := &checkingReader{t: t, s: s, runID: "run", pieces: pieces}
	if err := w.Pipe(Stdout, r); err != nil {
		t.Fatal(err)
	}
	if w.pendFrames != 0 || w.pendRaw != 0 {
		t.Fatalf("pending after Pipe: %d frames, %d bytes", w.pendFrames, w.pendRaw)
	}
	if err := s.Close("run"); err != nil {
		t.Fatal(err)
	}
	frames, err := s.Read("run", 0, 100000)
	if err != nil || len(frames) != r.lines {
		t.Fatalf("frames = %d, want %d (%v)", len(frames), r.lines, err)
	}
	for i, f := range frames {
		if f.Sequence != uint64(i+1) {
			t.Fatalf("frame %d has sequence %d", i, f.Sequence)
		}
	}
}

// A line longer than the read buffer is assembled across reads and stored
// whole, and the next short line takes the in-place path again.
func TestPipeLongLineSpansReads(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Open("run", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	long := bytes.Repeat([]byte("x"), 3*pipeReadBuffer+17)
	input := append(append([]byte("short\r\n"), long...), "\nend"...)
	if err := w.Pipe(Stdout, bytes.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close("run"); err != nil {
		t.Fatal(err)
	}
	frames, err := s.Read("run", 0, 10)
	if err != nil || len(frames) != 3 {
		t.Fatalf("frames = %d, %v", len(frames), err)
	}
	if string(frames[0].Payload) != "short" || !bytes.Equal(frames[1].Payload, long) || string(frames[2].Payload) != "end" || frames[2].Flags&FlagPartial == 0 {
		t.Fatalf("payloads: %q, %d bytes, %q flags %v", frames[0].Payload, len(frames[1].Payload), frames[2].Payload, frames[2].Flags)
	}
}
