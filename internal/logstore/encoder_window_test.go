package logstore

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"runtime"
	"testing"

	"github.com/khanhicetea/minicrond/internal/model"
)

// Frames far larger than the encoder's match window still round-trip, both
// from the live buffer and after archival.
func TestFramesLargerThanEncoderWindowRoundTrip(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Open("big", "job", model.KindJob, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(1, 2))
	big := make([]byte, 3<<20)
	for i := range big {
		big[i] = byte('a' + rng.IntN(26))
	}
	repeated := bytes.Repeat([]byte("same line again "), 64<<10) // 1 MiB, compressible
	for _, payload := range [][]byte{[]byte("small"), big, repeated, []byte("tail")} {
		if err := w.Write(Stdout, payload, 0); err != nil {
			t.Fatal(err)
		}
	}
	check := func(when string) {
		frames, err := s.Read("big", 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(frames) != 4 || !bytes.Equal(frames[1].Payload, big) || !bytes.Equal(frames[2].Payload, repeated) || string(frames[3].Payload) != "tail" {
			t.Fatalf("%s: %d frames read back", when, len(frames))
		}
	}
	check("live")
	if err := s.Close("big"); err != nil {
		t.Fatal(err)
	}
	check("sealed")
}

// Each open run keeps one encoder; it must stay well under the 1.46 MiB a
// 1 MiB window costs (5A small-server profile, 10 workers).
func TestOpenWriterHeapIsSmall(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("heap measurement")
	}
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	heap := func() int64 {
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return int64(m.HeapAlloc)
	}
	base := heap()
	var ws []*Writer
	for i := range 10 {
		w, err := s.Open(fmt.Sprintf("run-%d", i), "job", model.KindWorker, WriterOptions{MaxLine: 256 << 10})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Write(Stdout, []byte("hello"), 0); err != nil {
			t.Fatal(err)
		}
		ws = append(ws, w)
	}
	per := (heap() - base) / 10
	runtime.KeepAlive(ws)
	if per > 900<<10 {
		t.Fatalf("open writer retains %d KiB of heap", per>>10)
	}
	t.Logf("open writer retains %d KiB of heap", per>>10)
}
