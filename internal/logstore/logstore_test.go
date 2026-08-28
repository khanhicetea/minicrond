package logstore

import (
	"bytes"
	"testing"
	"time"
)

func TestFramesSurviveChunkStorage(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Open("run", 1<<20, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Pipe(Stdout, bytes.NewReader([]byte("hello\npartial"))); err != nil {
		t.Fatal(err)
	}
	if err := s.Close("run"); err != nil {
		t.Fatal(err)
	}
	frames, err := s.Read("run", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 {
		t.Fatalf("got %d frames", len(frames))
	}
	if string(frames[0].Payload) != "hell" || frames[0].Flags&FlagTruncated == 0 {
		t.Fatalf("first frame = %#v", frames[0])
	}
	if frames[1].Flags&FlagPartial == 0 || frames[1].Sequence != 2 {
		t.Fatalf("second frame = %#v", frames[1])
	}
}

func TestBacklogToLiveSubscriptionHasStableSequence(t *testing.T) {
	s, _ := New(t.TempDir())
	w, _ := s.Open("run", 1<<20, 1024)
	if err := w.Write(Stdout, []byte("backlog"), 0); err != nil {
		t.Fatal(err)
	}
	live, _, unsubscribe := w.Subscribe(0)
	defer unsubscribe()
	if err := w.Write(Stderr, []byte("handoff"), 0); err != nil {
		t.Fatal(err)
	}
	backlog, err := s.Read("run", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(backlog) != 2 || backlog[0].Sequence != 1 || backlog[1].Sequence != 2 {
		t.Fatalf("active backlog = %v", backlog)
	}
	select {
	case frame := <-live:
		if frame.Sequence != 2 {
			t.Fatalf("live sequence = %d", frame.Sequence)
		}
	case <-time.After(time.Second):
		t.Fatal("live frame lost")
	}
}

func TestInvalidUTF8IsFlaggedAndPreserved(t *testing.T) {
	s, _ := New(t.TempDir())
	w, _ := s.Open("run", 1024, 1024)
	payload := []byte{0xff, '\n'}
	if err := w.Pipe(Stderr, bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close("run"); err != nil {
		t.Fatal(err)
	}
	frames, err := s.Read("run", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || frames[0].Flags&FlagInvalidUTF8 == 0 || !bytes.Equal(frames[0].Payload, []byte{0xff}) {
		t.Fatalf("frame = %#v", frames)
	}
}
