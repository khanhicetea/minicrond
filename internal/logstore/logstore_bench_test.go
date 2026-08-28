package logstore

import "testing"

func BenchmarkFrameEncoding(b *testing.B) {
	payload := make([]byte, 1024)
	b.SetBytes(int64(len(payload)))
	store, err := New(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	writer, err := store.Open("run", 1<<40, 256<<10)
	if err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if err := writer.Write(Stdout, payload, 0); err != nil {
			b.Fatal(err)
		}
	}
	if err := store.Close("run"); err != nil {
		b.Fatal(err)
	}
}
