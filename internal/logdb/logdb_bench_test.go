package logdb

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Fresh run IDs measure normal archival; replay measures crash-recovery upserts.
// Both use the production WAL/FULL settings and default checkpoint cadence.
func BenchmarkPutChunks(b *testing.B) {
	for _, workload := range []struct {
		count int
		bytes int
	}{{1, 128}, {16, 128}, {64, 128}, {64, 64 << 10}} {
		for _, replay := range []bool{false, true} {
			mode := "insert"
			if replay {
				mode = "replay"
			}
			b.Run(fmt.Sprintf("%s/chunks_%d/blob_%d", mode, workload.count, workload.bytes), func(b *testing.B) {
				db, err := Open(b.TempDir())
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() { db.Close() })
				chunks := make([]Chunk, workload.count)
				for i := range chunks {
					chunks[i] = chunk(i+1, uint64(i+1), uint64(i+1), bytes.Repeat([]byte("x"), workload.bytes))
				}
				at := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
				if replay {
					if err := db.PutChunks(b.Context(), "replay", "job", "job", at, chunks); err != nil {
						b.Fatal(err)
					}
				}
				b.SetBytes(int64(workload.count * workload.bytes))
				b.ReportAllocs()
				i := 0
				for b.Loop() {
					id := "replay"
					if !replay {
						id = fmt.Sprintf("run-%d", i)
					}
					if err := db.PutChunks(b.Context(), id, "job", "job", at, chunks); err != nil {
						b.Fatal(err)
					}
					i++
				}
			})
		}
	}
}

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

func BenchmarkStats(b *testing.B) {
	db, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	chunks := make([]Chunk, 64)
	for i := range chunks {
		chunks[i] = chunk(i+1, uint64(i+1), uint64(i+1), make([]byte, 128))
	}
	for i := range 16 {
		if err := db.PutChunks(b.Context(), fmt.Sprintf("run-%d", i), "job", "job", time.Now(), chunks); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		runs, chunks, size, err := db.Stats(b.Context())
		if err != nil || runs != 16 || chunks != 1024 || size != 128*1024 {
			b.Fatalf("stats = %d/%d/%d: %v", runs, chunks, size, err)
		}
	}
}
