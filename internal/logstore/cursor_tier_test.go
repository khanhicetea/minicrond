package logstore

import (
	"fmt"
	"testing"

	"github.com/khanhicetea/minicrond/internal/model"
)

// A06: with the sequence-oriented archive cursor, paging a run whose frames
// live in several tiers at once (archived chunks, sealed chunk files, the
// active chunk) still yields every frame exactly once and in order, for every
// cursor position and page size, and keeps doing so while the run migrates
// between tiers.
func TestPagingAcrossTiersWhileMigrating(t *testing.T) {
	s, _, _ := newArchiveStore(t)
	w, err := s.Open("run", "job", model.KindWorker, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close("run")
	var total uint64
	write := func(n int) {
		for range n {
			total++
			if err := w.Write(Stdout, []byte(fmt.Sprintf("line %d", total)), 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	checkAllCursors := func(label string) {
		t.Helper()
		for _, page := range []int{1, 3, 7, 5000} {
			// Page from the start to the end.
			var next uint64
			for {
				frames, err := s.Read("run", next, page)
				if err != nil {
					t.Fatalf("%s: %v", label, err)
				}
				if len(frames) == 0 {
					break
				}
				for _, f := range frames {
					if f.Sequence != next+1 || string(f.Payload) != fmt.Sprintf("line %d", f.Sequence) {
						t.Fatalf("%s: page=%d got seq %d (%q) after %d", label, page, f.Sequence, f.Payload, next)
					}
					next = f.Sequence
				}
			}
			if next != total {
				t.Fatalf("%s: page=%d ended at %d, want %d", label, page, next, total)
			}
		}
		// Every starting cursor, including at chunk boundaries and the end.
		for after := uint64(0); after <= total; after++ {
			frames, err := s.Read("run", after, 5000)
			if err != nil || uint64(len(frames)) != total-after || (len(frames) > 0 && frames[0].Sequence != after+1) {
				t.Fatalf("%s: after=%d got %d frames, err %v; want %d", label, after, len(frames), err, total-after)
			}
		}
	}
	// Three archived chunks, one sealed file chunk, and an active chunk.
	for range 3 {
		write(5)
		if err := w.Flush(s); err != nil {
			t.Fatal(err)
		}
	}
	checkAllCursors("archived only")
	write(4)
	w.mu.Lock()
	if err := w.rotate(); err != nil { // seal the chunk without archiving it
		w.mu.Unlock()
		t.Fatal(err)
	}
	w.mu.Unlock()
	write(3)
	checkAllCursors("archived + sealed file + active")
	if err := w.Flush(s); err != nil { // sealed file chunk migrates into the archive
		t.Fatal(err)
	}
	write(2)
	checkAllCursors("after migration")
	// Completed run: everything archived, no buffer files.
	if err := s.Close("run"); err != nil {
		t.Fatal(err)
	}
	checkAllCursors("closed")
}
