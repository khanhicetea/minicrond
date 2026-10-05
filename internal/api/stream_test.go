package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/khanhicetea/minicrond/internal/logstore"
	"github.com/khanhicetea/minicrond/internal/model"
)

func fastStreamPoll(t *testing.T) {
	t.Helper()
	old := streamPollInterval
	streamPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { streamPollInterval = old })
}

// liveRun registers a run row and opens its log writer directly, so a test can
// drive output and completion by hand.
func liveRun(t *testing.T, s *Server, opt logstore.WriterOptions) (string, *logstore.Writer) {
	t.Helper()
	mustCreate(t, s, "live", "true")
	def, _, err := s.store.Definition(t.Context(), "live")
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	run := model.Run{ID: id, DefinitionID: def.ID, Job: def.Name, Kind: def.Kind, Revision: def.Revision,
		Status: "running", Trigger: "manual", QueuedAt: time.Now().UTC()}
	if err := s.store.CreateRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	w, err := s.logs.Open(id, def.Name, string(def.Kind), opt)
	if err != nil {
		t.Fatal(err)
	}
	return id, w
}

type sseEvent struct {
	id, name, data string
}

func (e sseEvent) sequence(t *testing.T) uint64 {
	t.Helper()
	var f struct {
		Sequence uint64 `json:"sequence"`
	}
	if err := json.Unmarshal([]byte(e.data), &f); err != nil {
		t.Fatalf("event %q: %v", e.data, err)
	}
	return f.Sequence
}

// readEvents parses SSE until the stream ends or the read fails.
func readEvents(r *bufio.Reader, out chan<- sseEvent) {
	defer close(out)
	var ev sseEvent
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if ev.name != "" {
				out <- ev
			}
			ev = sseEvent{}
		case strings.HasPrefix(line, "id: "):
			ev.id = line[4:]
		case strings.HasPrefix(line, "event: "):
			ev.name = line[7:]
		case strings.HasPrefix(line, "data: "):
			ev.data = line[6:]
		}
	}
}

func openStream(t *testing.T, ts *httptest.Server, path string, headers map[string]string) (<-chan sseEvent, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, "GET", ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		cancel()
		t.Fatalf("stream status %d", resp.StatusCode)
	}
	events := make(chan sseEvent, 64)
	go func() {
		readEvents(bufio.NewReader(resp.Body), events)
		resp.Body.Close()
	}()
	return events, cancel
}

func nextEvent(t *testing.T, events <-chan sseEvent, within time.Duration) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("stream ended unexpectedly")
		}
		return ev
	case <-time.After(within):
		t.Fatal("timed out waiting for an SSE event")
	}
	return sseEvent{}
}

// The followed stream delivers output as it is stored, batched at the poll
// interval, and the final tail is delivered before done.
func TestStreamFollowsLiveRunAndDeliversFinalTailBeforeDone(t *testing.T) {
	fastStreamPoll(t)
	s, _, _ := setup(t)
	ts := httptest.NewServer(s.middleware(s.routes(), true))
	defer ts.Close()
	id, w := liveRun(t, s, logstore.WriterOptions{MaxLine: 1024})
	if err := w.Write(logstore.Stdout, []byte("backlog"), 0); err != nil {
		t.Fatal(err)
	}
	events, cancel := openStream(t, ts, "/api/v1/runs/"+id+"/log/stream", nil)
	defer cancel()
	if ev := nextEvent(t, events, 5*time.Second); ev.name != "line" || ev.sequence(t) != 1 || ev.id != "1" {
		t.Fatalf("first event = %+v", ev)
	}
	if ev := nextEvent(t, events, 5*time.Second); ev.name != "backlog_done" {
		t.Fatalf("second event = %+v", ev)
	}
	// Live output arrives within a poll or two, without any subscription.
	if err := w.Write(logstore.Stderr, []byte("live"), 0); err != nil {
		t.Fatal(err)
	}
	if ev := nextEvent(t, events, 5*time.Second); ev.name != "line" || ev.sequence(t) != 2 {
		t.Fatalf("live event = %+v", ev)
	}
	// Final frames written right before the run completes must precede done.
	for i := range 5 {
		if err := w.Write(logstore.Stdout, []byte(fmt.Sprintf("tail %d", i)), 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.logs.Close(id); err != nil {
		t.Fatal(err)
	}
	for want := uint64(3); want <= 7; want++ {
		if ev := nextEvent(t, events, 5*time.Second); ev.name != "line" || ev.sequence(t) != want {
			t.Fatalf("tail event %d = %+v", want, ev)
		}
	}
	if ev := nextEvent(t, events, 5*time.Second); ev.name != "done" {
		t.Fatalf("final event = %+v", ev)
	}
	if ev, ok := <-events; ok {
		t.Fatalf("event after done: %+v", ev)
	}
}

// A cursor behind retention gets an explicit gap event, then the retained
// frames; reconnecting with the id the gap carries resumes without a gap.
func TestStreamReportsRetentionGapAndResumes(t *testing.T) {
	fastStreamPoll(t)
	s, _, _ := setup(t)
	ts := httptest.NewServer(s.middleware(s.routes(), true))
	defer ts.Close()
	id, w := liveRun(t, s, logstore.WriterOptions{MaxBytes: 60, MaxLine: 1024})
	for i := range 10 {
		if err := w.Write(logstore.Stdout, []byte(fmt.Sprintf("l%02d", i+1)), 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.logs.Close(id); err != nil {
		t.Fatal(err)
	}
	events, cancel := openStream(t, ts, "/api/v1/runs/"+id+"/log/stream", map[string]string{"Last-Event-ID": "3"})
	defer cancel()
	gap := nextEvent(t, events, 5*time.Second)
	if gap.name != "gap" {
		t.Fatalf("first event = %+v, want gap", gap)
	}
	var body struct{ After, First uint64 }
	if err := json.Unmarshal([]byte(strings.ToLower(gap.data)), &body); err != nil || body.After != 3 || body.First <= 4 {
		t.Fatalf("gap data %q: %+v, %v", gap.data, body, err)
	}
	if gap.id != fmt.Sprint(body.First-1) {
		t.Fatalf("gap id %q must resume just before the first retained frame %d", gap.id, body.First)
	}
	var lines []uint64
	for ev := range events {
		if ev.name == "line" {
			lines = append(lines, ev.sequence(t))
		}
	}
	if len(lines) == 0 || lines[0] != body.First || lines[len(lines)-1] != 10 {
		t.Fatalf("lines after the gap = %v", lines)
	}
	// Reconnect with the gap's id: no further gap.
	again, cancel2 := openStream(t, ts, "/api/v1/runs/"+id+"/log/stream", map[string]string{"Last-Event-ID": gap.id})
	defer cancel2()
	for ev := range again {
		if ev.name == "gap" {
			t.Fatalf("resumed stream reported a gap: %+v", ev)
		}
	}
}

// A01 regression: the audit's probe retained 64 MiB of queued payload for one
// subscriber that never consumed. A stalled SSE client now costs the daemon
// nothing in proportion to what the run writes, and the writer never waits.
func TestStalledStreamClientRetainsNoQueuedPayloadAndNeverBlocksWriter(t *testing.T) {
	fastStreamPoll(t)
	s, _, _ := setup(t)
	ts := httptest.NewServer(s.middleware(s.routes(), true))
	defer ts.Close()
	const maxLine = 256 << 10
	id, w := liveRun(t, s, logstore.WriterOptions{MaxBytes: 1 << 20, MaxLine: maxLine})

	// Connect and never read the body.
	req, _ := http.NewRequestWithContext(t.Context(), "GET", ts.URL+"/api/v1/runs/"+id+"/log/stream", nil)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	heap := func() uint64 {
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	payload := bytes.Repeat([]byte("0123456789abcdef"), maxLine/16)
	before := heap()
	done := make(chan error, 1)
	go func() {
		for range 256 {
			if err := w.Write(logstore.Stdout, payload, 0); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("writer blocked behind a stalled stream client")
	}
	if grown := int64(heap()) - int64(before); grown > 24<<20 {
		t.Fatalf("retained heap grew by %d MiB with a stalled client (the old queue held 64 MiB)", grown>>20)
	}
	if err := s.logs.Close(id); err != nil {
		t.Fatal(err)
	}
}

// D03: nothing remains for a viewer after it disconnects: the handler returns
// (releasing its stream slot, poll timer and reader) and leaves no goroutine.
func TestStreamLeavesNothingBehindAfterDisconnect(t *testing.T) {
	fastStreamPoll(t)
	s, _, _ := setup(t)
	ts := httptest.NewServer(s.middleware(s.routes(), true))
	defer ts.Close()
	id, w := liveRun(t, s, logstore.WriterOptions{MaxLine: 1024})
	if err := w.Write(logstore.Stdout, []byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	baseline := runtime.NumGoroutine()
	var cancels []context.CancelFunc
	for range 5 {
		events, cancel := openStream(t, ts, "/api/v1/runs/"+id+"/log/stream", nil)
		nextEvent(t, events, 5*time.Second) // line
		cancels = append(cancels, cancel)
	}
	if len(s.streamSlots) != 5 {
		t.Fatalf("stream slots in use = %d", len(s.streamSlots))
	}
	for _, cancel := range cancels {
		cancel()
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(s.streamSlots) != 0 || runtime.NumGoroutine() > baseline+1 {
		if time.Now().After(deadline) {
			t.Fatalf("after disconnect: %d slots, %d goroutines (baseline %d)", len(s.streamSlots), runtime.NumGoroutine(), baseline)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// With no viewer the writer carries no per-frame viewer work at all: a
	// write after the last disconnect must succeed and be readable on demand.
	if err := w.Write(logstore.Stdout, []byte("later"), 0); err != nil {
		t.Fatal(err)
	}
	if frames, err := s.logs.Read(id, 1, 10); err != nil || len(frames) != 1 {
		t.Fatalf("read after viewers left: %v, %v", frames, err)
	}
	if err := s.logs.Close(id); err != nil {
		t.Fatal(err)
	}
}
