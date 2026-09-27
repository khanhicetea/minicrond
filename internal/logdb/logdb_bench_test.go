package logdb

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func BenchmarkDeleteRunWithChunk(b *testing.B) {
	dir := b.TempDir()
	db, err := Open(dir)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	if _, err := db.db.ExecContext(b.Context(), "PRAGMA wal_autocheckpoint=0"); err != nil {
		b.Fatal(err)
	}
	for i := range b.N {
		id := fmt.Sprintf("run-%d", i)
		if err := db.PutChunks(b.Context(), id, "job", "job", time.Now(), []Chunk{{Number: 1, First: 1, Last: 1, RawBytes: 32, Blob: []byte("compressed")}}); err != nil {
			b.Fatal(err)
		}
	}
	walPath := filepath.Join(dir, "minicron-logs.db-wal")
	walSize := func() int64 {
		info, err := os.Stat(walPath)
		if err != nil {
			b.Fatal(err)
		}
		return info.Size()
	}
	before := walSize()
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if err := db.DeleteRun(b.Context(), fmt.Sprintf("run-%d", i)); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(walSize()-before)/float64(b.N), "wal-B/op")
}
