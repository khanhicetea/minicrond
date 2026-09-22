package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/khanhicetea/minicrond/internal/config"
	"github.com/khanhicetea/minicrond/internal/logdb"
	"github.com/khanhicetea/minicrond/internal/logstore"
)

func TestWorkerFlushLoopRetriesInactiveBuffers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		logs, err := logstore.New(filepath.Join(dir, "logs"))
		if err != nil {
			t.Fatal(err)
		}
		w, err := logs.Open("run", "job", "job", logstore.WriterOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Write(logstore.Stdout, []byte("retained"), 0); err != nil {
			t.Fatal(err)
		}
		// A finalized file-only buffer has the same durable layout as a
		// failed final archive. It must be picked up by normal maintenance.
		if err := logs.Close("run"); err != nil {
			t.Fatal(err)
		}
		db, err := logdb.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		logs.AttachDB(db)
		d := &Daemon{logs: logs, cfg: &config.Config{Logs: config.Logs{WorkerFlushInterval: "1s"}}}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			d.workerFlushLoop(ctx)
		}()
		defer func() { cancel(); <-done }()
		synctest.Wait()
		time.Sleep(time.Second) // advance the fake clock to the maintenance tick
		synctest.Wait()
		if _, err := os.Stat(filepath.Join(dir, "logs", "run")); !os.IsNotExist(err) {
			t.Fatalf("maintenance did not archive inactive buffer: %v", err)
		}
		frames, err := logs.Read("run", 0, 100)
		if err != nil || len(frames) != 1 {
			t.Fatalf("got %d frames, error %v", len(frames), err)
		}
	})
}
