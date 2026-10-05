package logstore

import (
	"bytes"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

func heapInUse() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// A01/D01 regression. The audit's probe wrote 256 maximum-size lines past a
// non-consuming subscriber and retained 64 MiB of queued payload (plus a
// 16 MiB-per-run tail). There is no subscription or payload tail any more:
// with readers attached or not, retained heap stays flat however much is written.
func TestMaximumSizeFramesRetainNoPayloadWithSlowReaders(t *testing.T) {
	const (
		maxLine = 256 << 10
		frames  = 256
	)
	for _, readers := range []int{0, 4} {
		t.Run(fmt.Sprintf("readers=%d", readers), func(t *testing.T) {
			s, err := New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			w, err := s.Open("run", "job", model.KindJob, WriterOptions{MaxBytes: 1 << 20, MaxLine: maxLine})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close("run")
			payload := bytes.Repeat([]byte("0123456789abcdef"), maxLine/16)
			// Slow readers: each holds a StreamReader and takes one page now and
			// then; none may make the writer wait for it or pin payload.
			var stop atomic.Bool
			var wg sync.WaitGroup
			for range readers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					r := s.NewStreamReader("run")
					defer r.Close()
					var after uint64
					for !stop.Load() {
						page, err := r.ReadContext(t.Context(), after, 5000)
						if err == nil && len(page) > 0 {
							after = page[len(page)-1].Sequence
						}
						time.Sleep(20 * time.Millisecond)
					}
				}()
			}
			before := heapInUse()
			start := time.Now()
			for range frames {
				if err := w.Write(Stdout, payload, 0); err != nil {
					t.Fatal(err)
				}
			}
			elapsed := time.Since(start)
			stop.Store(true)
			wg.Wait()
			after := heapInUse()
			if grown := int64(after) - int64(before); grown > 24<<20 {
				t.Fatalf("retained heap grew %d MiB while writing %d MiB (the old subscriber queue held 64 MiB)", grown>>20, frames*maxLine>>20)
			}
			// 64 MiB of writes plus slow readers must finish promptly: the writer
			// never waits for a reader's consumption, only for a page's decode.
			if elapsed > 30*time.Second {
				t.Fatalf("writes took %v with slow readers", elapsed)
			}
		})
	}
}

// Retention gap + reconnect: a cursor that fell behind a drop_old eviction sees
// the gap as a jump in sequence, and resuming from just before the first
// retained frame is gapless from there.
func TestRetentionGapIsVisibleAndReconnectCursorIsGapless(t *testing.T) {
	s, _, _ := newArchiveStore(t)
	// Room for about two frames; older sealed chunks are evicted.
	w, err := s.Open("run", "job", model.KindJob, WriterOptions{MaxBytes: 60, MaxLine: 1024})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 10 {
		if err := w.Write(Stdout, []byte(fmt.Sprintf("l%02d", i+1)), 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, truncated := w.Stats(); !truncated {
		t.Fatal("eviction must flag the run truncated")
	}
	page, err := s.Read("run", 3, 100) // a viewer that was at sequence 3
	if err != nil || len(page) == 0 {
		t.Fatalf("read after evicted cursor: %v, %v", page, err)
	}
	first := page[0].Sequence
	if first <= 4 {
		t.Fatalf("first retained sequence %d after cursor 3: no gap observed", first)
	}
	// Reconnect with the cursor the gap reported (first-1): contiguous to the end.
	resumed, err := s.Read("run", first-1, 100)
	if err != nil || len(resumed) == 0 || resumed[0].Sequence != first {
		t.Fatalf("resume at %d: %v, %v", first-1, resumed, err)
	}
	for i := 1; i < len(resumed); i++ {
		if resumed[i].Sequence != resumed[i-1].Sequence+1 {
			t.Fatalf("gap inside the retained range: %v", resumed)
		}
	}
	if last := resumed[len(resumed)-1]; last.Sequence != 10 {
		t.Fatalf("last retained sequence = %d, want 10", last.Sequence)
	}
}

// Tier migration under a following cursor: chunks move active file -> archive
// while a reader pages forward; every frame is delivered once, in order.
func TestFollowingCursorSurvivesTierMigration(t *testing.T) {
	s, _, _ := newArchiveStore(t)
	w, err := s.Open("run", "worker", model.KindWorker, WriterOptions{MaxLine: 1024})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close("run") })
	var after uint64
	var seen []uint64
	poll := func() {
		for {
			page, err := s.Read("run", after, 7)
			if err != nil {
				t.Fatal(err)
			}
			if len(page) == 0 {
				return
			}
			for _, f := range page {
				seen = append(seen, f.Sequence)
			}
			after = page[len(page)-1].Sequence
		}
	}
	next := 0
	write := func(n int) {
		for range n {
			next++
			if err := w.Write(Stdout, []byte(fmt.Sprintf("line %d", next)), 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	write(10)
	poll()
	write(10)
	if err := w.Flush(s); err != nil { // active chunk -> archive
		t.Fatal(err)
	}
	write(5)
	poll()
	if err := w.Flush(s); err != nil {
		t.Fatal(err)
	}
	write(5)
	if err := s.Seal("run"); err != nil { // final tail, then archive
		t.Fatal(err)
	}
	poll()
	if len(seen) != next {
		t.Fatalf("delivered %d of %d frames", len(seen), next)
	}
	for i, seq := range seen {
		if seq != uint64(i+1) {
			t.Fatalf("frame %d has sequence %d", i, seq)
		}
	}
}
