package main

import (
	"errors"
	"testing"
)

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestGenerateReturnsOutputError(t *testing.T) {
	want := errors.New("output unavailable")
	if err := generate(failingWriter{want}); !errors.Is(err, want) {
		t.Fatalf("output error was not preserved: %v", err)
	}
}
