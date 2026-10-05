package logstore

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

// A15: the archiver's id queue is capped. Ids beyond the cap are not lost: the
// buffers stay on disk, show up in the sealed-buffer stats, and the idle
// archiver rediscovers and archives them with an orphan sweep.
func TestArchiveQueueIsBoundedAndOverflowIsRediscovered(t *testing.T) {
	old := maxArchiveQueue
	maxArchiveQueue = 3
	t.Cleanup(func() { maxArchiveQueue = old })
	s, db, dir := newArchiveStore(t)
	s.StartArchiver()
	t.Cleanup(func() { s.StopArchiver(t.Context()) })
	// Block both archive workers so sealed runs pile up behind them.
	for range cap(s.archiveSlots) {
		s.archiveSlots <- struct{}{}
	}
	const runs = 12
	for i := range runs {
		id := fmt.Sprintf("run%02d", i)
		w, err := s.Open(id, "job", model.KindJob, WriterOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Write(Stdout, []byte("payload "+id), 0); err != nil {
			t.Fatal(err)
		}
		if err := s.Seal(id); err != nil {
			t.Fatal(err)
		}
	}
	if backlog := s.ArchiveBacklog(); backlog > maxArchiveQueue {
		t.Fatalf("archive queue grew to %d, cap %d", backlog, maxArchiveQueue)
	}
	st := s.ArchiveStats()
	if !st.Overflow || st.OverflowTotal == 0 || st.SealedRuns != runs || st.SealedBytes <= 0 || st.Queued > maxArchiveQueue {
		t.Fatalf("stats while blocked = %+v", st)
	}
	// Free the workers: the queued ids drain, then the idle sweep finds the rest.
	for range cap(s.archiveSlots) {
		<-s.archiveSlots
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		entries, err := os.ReadDir(filepath.Join(dir, "logs"))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d buffers never archived; stats %+v", len(entries), s.ArchiveStats())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n, _, _, err := db.Stats(t.Context()); err != nil || n != runs {
		t.Fatalf("archived runs = %d, %v; want %d", n, err, runs)
	}
	if st := s.ArchiveStats(); st.SealedRuns != 0 || st.SealedBytes != 0 || st.Overflow {
		t.Fatalf("stats after drain = %+v", st)
	}
}
