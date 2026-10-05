package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/khanhicetea/minicrond/internal/logstore"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/store"
)

// fastReads shortens the admission timings so saturation tests run quickly.
func fastReads(t *testing.T) {
	t.Helper()
	wait, ttl := readAdmitWait, metricsCacheTTL
	readAdmitWait, metricsCacheTTL = 50*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { readAdmitWait, metricsCacheTTL = wait, ttl })
}

// storedRun registers a run row for a new definition and opens its log writer.
func storedRun(t *testing.T, s *Server, name string) (string, *logstore.Writer) {
	t.Helper()
	mustCreate(t, s, name, "true")
	def, _, err := s.store.Definition(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	run := model.Run{ID: id, DefinitionID: def.ID, Job: def.Name, Kind: def.Kind, Revision: def.Revision,
		Status: "running", Trigger: "manual", QueuedAt: time.Now().UTC()}
	if err := s.store.CreateRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	w, err := s.logs.Open(id, def.Name, string(def.Kind), logstore.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.logs.Close(id) })
	return id, w
}

func writeLines(t *testing.T, w *logstore.Writer, count, size int) {
	t.Helper()
	line := bytes.Repeat([]byte{'x'}, size)
	for range count {
		if err := w.Write(logstore.Stdout, line, 0); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadGateLimitsSlotsAndBytes(t *testing.T) {
	ctx := t.Context()
	g := newReadGate(ReadLimits{Slots: 2, BudgetBytes: 64 << 20})
	r1, err := g.acquire(ctx, 4<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := g.acquire(ctx, 4<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.acquire(ctx, 4<<20, 0); !errors.Is(err, errReadBusy) {
		t.Fatalf("third slot = %v, want busy", err)
	}
	start := time.Now()
	if _, err := g.acquire(ctx, 4<<20, 60*time.Millisecond); !errors.Is(err, errReadBusy) || time.Since(start) > time.Second {
		t.Fatalf("queued request = %v after %v, want busy after about its wait", err, time.Since(start))
	}
	// A queued request is admitted as soon as capacity is released.
	admitted := make(chan error, 1)
	go func() {
		release, err := g.acquire(ctx, 4<<20, 5*time.Second)
		if err == nil {
			release()
		}
		admitted <- err
	}()
	for _, _, waiting := g.stats(); waiting == 0; _, _, waiting = g.stats() {
		time.Sleep(time.Millisecond)
	}
	r1()
	r1() // releasing twice must not free a second slot
	select {
	case err := <-admitted:
		if err != nil {
			t.Fatalf("queued request: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued request was not admitted after a release")
	}
	r2()
	if inflight, bytes, waiting := g.stats(); inflight != 0 || bytes != 0 || waiting != 0 {
		t.Fatalf("gate not empty: %d %d %d", inflight, bytes, waiting)
	}

	// The byte budget binds even when slots remain.
	g = newReadGate(ReadLimits{Slots: 8, BudgetBytes: 8 << 20})
	a, _ := g.acquire(ctx, 4<<20, 0)
	b, _ := g.acquire(ctx, 4<<20, 0)
	if _, err := g.acquire(ctx, 4<<20, 0); !errors.Is(err, errReadBusy) {
		t.Fatalf("over-budget request = %v, want busy", err)
	}
	a()
	b()
	// An oversized request still runs when nothing else does.
	if release, err := g.acquire(ctx, 64<<20, 0); err != nil {
		t.Fatalf("oversized request on an idle gate: %v", err)
	} else {
		release()
	}
}

func TestReadGateBoundsWaitersAndHonorsCancel(t *testing.T) {
	g := newReadGate(ReadLimits{Slots: 1})
	hold, _ := g.acquire(t.Context(), 1, 0)
	defer hold()
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	results := make(chan error, g.maxWaiters)
	for range g.maxWaiters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := g.acquire(ctx, 1, time.Minute)
			results <- err
		}()
	}
	for _, _, waiting := g.stats(); waiting < g.maxWaiters; _, _, waiting = g.stats() {
		time.Sleep(time.Millisecond)
	}
	// The queue is full: further requests are refused at once, not parked.
	start := time.Now()
	if _, err := g.acquire(t.Context(), 1, time.Minute); !errors.Is(err, errReadBusy) || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("request beyond the wait queue = %v after %v", err, time.Since(start))
	}
	cancel()
	wg.Wait()
	close(results)
	for err := range results {
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled waiter = %v", err)
		}
	}
	if _, _, waiting := g.stats(); waiting != 0 {
		t.Fatalf("%d waiters left after cancel", waiting)
	}
}

func TestMetricsShareCoalescesAndExpires(t *testing.T) {
	old := metricsCacheTTL
	metricsCacheTTL = 150 * time.Millisecond
	defer func() { metricsCacheTTL = old }()
	var m metricsShare
	var computed atomic.Int32
	started, proceed := make(chan struct{}), make(chan struct{})
	compute := func(context.Context) (store.RunMetrics, error) {
		if computed.Add(1) == 1 {
			close(started)
		}
		<-proceed
		return store.RunMetrics{Total: 7}, nil
	}
	key := metricsKey{time.Hour, 48}
	var wg sync.WaitGroup
	results := make([]store.RunMetrics, 20)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			if results[i], err = m.get(t.Context(), key, compute); err != nil {
				t.Error(err)
			}
		}()
		if i == 0 {
			<-started // the first request is computing before the others arrive
		}
	}
	time.Sleep(20 * time.Millisecond)
	close(proceed)
	wg.Wait()
	for i, r := range results {
		if r.Total != 7 {
			t.Fatalf("result %d = %+v, want the shared result", i, r)
		}
	}
	if n := computed.Load(); n != 1 {
		t.Fatalf("%d computations for 20 concurrent requests, want 1", n)
	}
	// Within the TTL the result is reused without computing again.
	if r, err := m.get(t.Context(), key, compute); err != nil || r.Total != 7 || computed.Load() != 1 {
		t.Fatalf("cached read: %+v, %v, %d computations", r, err, computed.Load())
	}
	// Nothing is retained once it expires, and the next request recomputes.
	deadline := time.Now().Add(2 * time.Second)
	for {
		m.mu.Lock()
		n := len(m.flights)
		m.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d flights retained after the TTL", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := m.get(t.Context(), key, compute); err != nil || computed.Load() != 2 {
		t.Fatalf("after expiry: %v, %d computations", err, computed.Load())
	}
}

func TestMetricsShareFailuresAreNotCachedAndCancelHandsOver(t *testing.T) {
	fastReads(t)
	var m metricsShare
	key := metricsKey{time.Hour, 48}
	boom := errors.New("boom")
	if _, err := m.get(t.Context(), key, func(context.Context) (store.RunMetrics, error) { return store.RunMetrics{}, boom }); !errors.Is(err, boom) {
		t.Fatalf("error = %v", err)
	}
	if r, err := m.get(t.Context(), key, func(context.Context) (store.RunMetrics, error) { return store.RunMetrics{Total: 1}, nil }); err != nil || r.Total != 1 {
		t.Fatalf("a failure was cached: %+v, %v", r, err)
	}
	time.Sleep(metricsCacheTTL + 50*time.Millisecond)

	// The computing request is canceled while another still waits: the waiter
	// takes over instead of inheriting the cancellation.
	leaderCtx, cancelLeader := context.WithCancel(t.Context())
	entered := make(chan struct{})
	leaderDone := make(chan error, 1)
	go func() {
		_, err := m.get(leaderCtx, key, func(ctx context.Context) (store.RunMetrics, error) {
			close(entered)
			<-ctx.Done()
			return store.RunMetrics{}, ctx.Err()
		})
		leaderDone <- err
	}()
	<-entered
	followerDone := make(chan store.RunMetrics, 1)
	go func() {
		r, err := m.get(t.Context(), key, func(context.Context) (store.RunMetrics, error) { return store.RunMetrics{Total: 9}, nil })
		if err != nil {
			t.Error(err)
		}
		followerDone <- r
	}()
	time.Sleep(20 * time.Millisecond)
	cancelLeader()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader = %v", err)
	}
	select {
	case r := <-followerDone:
		if r.Total != 9 {
			t.Fatalf("follower = %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follower did not take over from the canceled leader")
	}

	// A waiter that gives up leaves promptly and does not disturb the leader.
	release := make(chan struct{})
	time.Sleep(metricsCacheTTL + 50*time.Millisecond)
	leading := make(chan struct{})
	go func() {
		defer close(leading)
		_, _ = m.get(t.Context(), key, func(context.Context) (store.RunMetrics, error) { <-release; return store.RunMetrics{}, nil })
	}()
	time.Sleep(20 * time.Millisecond)
	waiterCtx, cancelWaiter := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancelWaiter()
	start := time.Now()
	if _, err := m.get(waiterCtx, key, nil); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("waiter = %v after %v", err, time.Since(start))
	}
	close(release)
	<-leading
}

// Saturated admission yields a retryable 503 quickly, and only for the
// expensive reads; everything else keeps working.
func TestExpensiveReadsRefusedWhenSaturated(t *testing.T) {
	fastReads(t)
	s, _, _ := setup(t)
	s.SetReadLimits(ReadLimits{Slots: 1})
	id, w := storedRun(t, s, "busy")
	writeLines(t, w, 5, 100)
	hold, err := s.gate().acquire(t.Context(), 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/metrics/runs?range=1h", "/api/v1/runs/" + id + "/log", "/api/v1/runs/" + id + "/log/raw"} {
		start := time.Now()
		rec := call(s, true, "GET", path, "", "", nil)
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("%s: refusal took %v", path, elapsed)
		}
		if rec.Code != 503 || rec.Header().Get("Retry-After") != "1" {
			t.Errorf("%s: %d retry-after %q, want 503 with Retry-After", path, rec.Code, rec.Header().Get("Retry-After"))
			continue
		}
		if code := decode(t, rec)["error"].(map[string]any)["code"]; code != "read_busy" {
			t.Errorf("%s: error code %v", path, code)
		}
		if rec.Header().Get("Content-Disposition") != "" {
			t.Errorf("%s: a refused download carries download headers", path)
		}
	}
	for _, path := range []string{"/healthz", "/api/v1/runs/" + id, "/api/v1/jobs", "/api/v1/runs"} {
		if rec := call(s, true, "GET", path, "", "", nil); rec.Code != 200 {
			t.Errorf("%s while reads are saturated: %d", path, rec.Code)
		}
	}
	hold()
	for _, path := range []string{"/api/v1/metrics/runs?range=1h", "/api/v1/runs/" + id + "/log", "/api/v1/runs/" + id + "/log/raw"} {
		if rec := call(s, true, "GET", path, "", "", nil); rec.Code != 200 {
			t.Errorf("%s after release: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	if inflight, _, waiting := s.gate().stats(); inflight != 0 || waiting != 0 {
		t.Fatalf("gate leaked: inflight %d waiting %d", inflight, waiting)
	}
}

// The request-work deadline bounds the read itself and answers retryably.
func TestExpensiveReadsHonorWorkDeadline(t *testing.T) {
	s, _, _ := setup(t)
	s.SetReadLimits(ReadLimits{WorkTimeout: time.Nanosecond})
	id, w := storedRun(t, s, "slow")
	writeLines(t, w, 3, 100)
	for _, path := range []string{"/api/v1/metrics/runs?range=1h", "/api/v1/runs/" + id + "/log", "/api/v1/runs/" + id + "/log/raw"} {
		rec := call(s, true, "GET", path, "", "", nil)
		if rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
			t.Errorf("%s: %d %s, want a retryable 503", path, rec.Code, rec.Body.String())
			continue
		}
		if code := decode(t, rec)["error"].(map[string]any)["code"]; code != "read_timeout" {
			t.Errorf("%s: error code %v", path, code)
		}
	}
	if inflight, _, _ := s.gate().stats(); inflight != 0 {
		t.Fatalf("slot leaked after a timed-out read: %d", inflight)
	}
}

// Work time must not eat the socket write budget: a request that queued for
// admission and then ran longer than the server write timeout still gets its
// response because the write deadline is renewed for the response write.
func TestResponseWriteDeadlineIsIndependentOfWorkTime(t *testing.T) {
	s, _, _ := setup(t)
	readAdmitWait = 2 * time.Second
	t.Cleanup(func() { readAdmitWait = 500 * time.Millisecond })
	s.SetReadLimits(ReadLimits{Slots: 1})
	id, w := storedRun(t, s, "deadline")
	writeLines(t, w, 3, 100)
	server := newRealServer(t, s, 400*time.Millisecond)
	hold, err := s.gate().acquire(t.Context(), 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(600*time.Millisecond, hold) // longer than the 400 ms write timeout
	for _, path := range []string{"/api/v1/runs/" + id + "/log", "/api/v1/metrics/runs"} {
		start := time.Now()
		resp, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatalf("%s lost its response after queuing for %v: %v", path, time.Since(start), err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || err != nil || len(body) == 0 {
			t.Fatalf("%s = %d, %v, %d bytes", path, resp.StatusCode, err, len(body))
		}
		if path[len(path)-3:] == "log" && time.Since(start) < 500*time.Millisecond {
			t.Fatalf("the request was not delayed (%v), so the test proves nothing", time.Since(start))
		}
	}
}

// A JSON page is capped near 1 MiB of payload instead of the old 16 MiB, and
// clients walk the log by sequence.
func TestJSONLogPageIsByteCapped(t *testing.T) {
	s, _, _ := setup(t)
	id, w := storedRun(t, s, "big")
	const lines, size = 40, 100 << 10 // 4 MiB in total
	writeLines(t, w, lines, size)
	var after uint64
	pages, total := 0, 0
	for {
		rec := call(s, true, "GET", fmt.Sprintf("/api/v1/runs/%s/log?after=%d&limit=5000", id, after), "", "", nil)
		if rec.Code != 200 {
			t.Fatalf("page %d: %d %s", pages, rec.Code, rec.Body.String())
		}
		var page struct {
			Items []struct {
				Sequence uint64 `json:"sequence"`
				Payload  []byte `json:"payload"`
			} `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) == 0 {
			break
		}
		bytesInPage := 0
		for _, f := range page.Items {
			bytesInPage += len(f.Payload)
			after = f.Sequence
		}
		if bytesInPage > 1<<20+size {
			t.Fatalf("page %d carries %d payload bytes, want about 1 MiB at most", pages, bytesInPage)
		}
		total += len(page.Items)
		pages++
	}
	if total != lines || pages < 3 {
		t.Fatalf("read %d lines in %d pages, want %d lines in several pages", total, pages, lines)
	}
}

func TestRawDownloadMatchesStoreAndPages(t *testing.T) {
	s, _, _ := setup(t)
	id, w := storedRun(t, s, "rawcheck")
	for i := range 30 {
		stream := logstore.Stdout
		if i%3 == 1 {
			stream = logstore.Stderr
		}
		if err := w.Write(stream, bytes.Repeat([]byte{byte('a' + i%26)}, 150<<10), 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Write(logstore.System, []byte("done"), 0); err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	if err := s.logs.Raw(id, &want); err != nil {
		t.Fatal(err)
	}
	rec := call(s, true, "GET", "/api/v1/runs/"+id+"/log/raw", "", "", nil)
	if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), want.Bytes()) {
		t.Fatalf("raw download: %d, %d bytes, want %d identical bytes", rec.Code, rec.Body.Len(), want.Len())
	}
	if rec.Header().Get("Content-Disposition") == "" || rec.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("download headers: %v", rec.Header())
	}
	if want.Len() < 4<<20 {
		t.Fatalf("fixture too small to exercise paging: %d", want.Len())
	}
}

// A stalled download occupies its one slot, so other expensive reads are
// refused promptly rather than hanging, and everything recovers once the
// client reads again.
func TestStalledDownloadDoesNotHangOtherReads(t *testing.T) {
	fastReads(t)
	s, _, _ := setup(t)
	s.SetReadLimits(ReadLimits{Slots: 1})
	id, w := storedRun(t, s, "stalled")
	const lines, size = 100, 200 << 10 // 20 MiB: more than loopback buffers hold
	writeLines(t, w, lines, size)
	server := newRealServer(t, s, 10*time.Second)
	resp, err := http.Get(server.URL + "/api/v1/runs/" + id + "/log/raw")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for inflight, _, _ := s.gate().stats(); inflight == 0; inflight, _, _ = s.gate().stats() {
		if time.Now().After(deadline) {
			t.Fatal("the download never took a slot")
		}
		time.Sleep(5 * time.Millisecond)
	}
	start := time.Now()
	other, err := http.Get(server.URL + "/api/v1/runs/" + id + "/log")
	if err != nil {
		t.Fatal(err)
	}
	other.Body.Close()
	if other.StatusCode != 503 || time.Since(start) > 2*time.Second {
		t.Fatalf("log page beside a stalled download: %d after %v", other.StatusCode, time.Since(start))
	}
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil || n != int64(lines)*(size+1) {
		t.Fatalf("download after the stall: %d bytes, %v; want %d", n, err, lines*(size+1))
	}
	if inflight, _, _ := s.gate().stats(); inflight != 0 {
		t.Fatalf("slot still held after the download: %d", inflight)
	}
}

func TestCanceledRequestLeavesAdmissionQueue(t *testing.T) {
	s, _, _ := setup(t)
	readAdmitWait = 30 * time.Second
	t.Cleanup(func() { readAdmitWait = 500 * time.Millisecond })
	s.SetReadLimits(ReadLimits{Slots: 1})
	hold, _ := s.gate().acquire(t.Context(), 1, 0)
	defer hold()
	ctx, cancel := context.WithCancel(t.Context())
	req := httptest.NewRequestWithContext(ctx, "GET", "/api/v1/metrics/runs", nil)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { s.middleware(s.routes(), true).ServeHTTP(rec, req); close(done) }()
	for _, _, waiting := s.gate().stats(); waiting == 0; _, _, waiting = s.gate().stats() {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a canceled metrics request kept waiting for a slot")
	}
	if _, _, waiting := s.gate().stats(); waiting != 0 || rec.Body.Len() != 0 {
		t.Fatalf("waiting %d, wrote %q", waiting, rec.Body.String())
	}
}

// mustServe is call for a GET that fails the test, instead of crashing it, when
// the handler aborts the connection.
func mustServe(t *testing.T, s *Server, path string) (rec *httptest.ResponseRecorder) {
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Errorf("%s aborted: %v", path, recovered)
			rec = nil
		}
	}()
	return call(s, true, "GET", path, "", "", nil)
}

// Concurrent metrics, JSON pages and downloads against active writers: the
// reads stay inside the admission bounds, refusals are prompt, memory stays
// bounded, writers are not starved, and nothing is left behind afterwards.
func TestMixedReadLoadStaysBounded(t *testing.T) {
	fastReads(t)
	s, _, _ := setup(t)
	limits := ReadLimits{Slots: 3, BudgetBytes: 16 << 20}
	s.SetReadLimits(limits)
	// Finished runs are read while other runs have live writers. (A download of
	// a run that is still being written chases the writer until it catches up,
	// which is correct but would make this test's duration load-dependent.)
	var ids []string
	for i := range 3 {
		id, w := storedRun(t, s, fmt.Sprintf("load%d", i))
		writeLines(t, w, 12, 100<<10) // 1.2 MiB each, so reads span pages
		if err := s.logs.Close(id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	var writers []*logstore.Writer
	for i := range 3 {
		_, w := storedRun(t, s, fmt.Sprintf("active%d", i))
		writers = append(writers, w)
	}
	// A tight GC target keeps uncollected garbage from the many short-lived
	// responses from drowning the retained-working-set signal.
	defer debug.SetGCPercent(debug.SetGCPercent(20))
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	baseHeap := int64(base.HeapInuse)

	stop := make(chan struct{})
	var background sync.WaitGroup
	var peakHeap atomic.Uint64
	background.Add(1)
	go func() {
		defer background.Done()
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			for old := peakHeap.Load(); m.HeapInuse > old && !peakHeap.CompareAndSwap(old, m.HeapInuse); old = peakHeap.Load() {
			}
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	var written atomic.Int64
	var slowestWrite atomic.Int64
	for _, w := range writers {
		background.Add(1)
		go func() {
			defer background.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				start := time.Now()
				if err := w.Write(logstore.Stdout, []byte("active writer output"), 0); err != nil {
					t.Error(err)
					return
				}
				written.Add(1)
				for d, old := int64(time.Since(start)), slowestWrite.Load(); d > old && !slowestWrite.CompareAndSwap(old, d); old = slowestWrite.Load() {
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}

	var ok, refused atomic.Int64
	var slowestRefusal atomic.Int64
	var clients sync.WaitGroup
	paths := []string{"/api/v1/metrics/runs?range=24h", "/api/v1/metrics/runs?range=1h&buckets=12"}
	for _, id := range ids {
		paths = append(paths, "/api/v1/runs/"+id+"/log?limit=5000", "/api/v1/runs/"+id+"/log/raw")
	}
	endAt := time.Now().Add(1500 * time.Millisecond)
	for c := range 24 {
		clients.Add(1)
		go func() {
			defer clients.Done()
			for i := c; time.Now().Before(endAt); i++ {
				start := time.Now()
				rec := mustServe(t, s, paths[i%len(paths)])
				if rec == nil {
					return
				}
				switch rec.Code {
				case 200:
					ok.Add(1)
				case 503:
					if rec.Header().Get("Retry-After") == "" {
						t.Errorf("503 without Retry-After on %s", paths[i%len(paths)])
					}
					refused.Add(1)
					for d, old := int64(time.Since(start)), slowestRefusal.Load(); d > old && !slowestRefusal.CompareAndSwap(old, d); old = slowestRefusal.Load() {
					}
				default:
					t.Errorf("%s: %d %s", paths[i%len(paths)], rec.Code, rec.Body.String())
				}
			}
		}()
	}
	clients.Wait()
	close(stop)
	background.Wait()

	g := s.gate()
	if g.peakInflight > limits.Slots || g.peakBytes > limits.BudgetBytes {
		t.Fatalf("admission exceeded: %d in flight (limit %d), %d bytes (budget %d)", g.peakInflight, limits.Slots, g.peakBytes, limits.BudgetBytes)
	}
	if ok.Load() == 0 || refused.Load() == 0 {
		t.Fatalf("expected both served and refused requests under saturation: %d ok, %d refused", ok.Load(), refused.Load())
	}
	if d := time.Duration(slowestRefusal.Load()); d > 2*time.Second {
		t.Fatalf("slowest refusal took %v", d)
	}
	if written.Load() < 100 {
		t.Fatalf("active writers made only %d writes during the load", written.Load())
	}
	if d := time.Duration(slowestWrite.Load()); d > time.Second {
		t.Fatalf("a writer was blocked for %v", d)
	}
	if growth := int64(peakHeap.Load()) - baseHeap; growth > 192<<20 {
		t.Fatalf("heap grew by %d MiB under read load", growth>>20)
	}
	if inflight, bytes, waiting := g.stats(); inflight != 0 || bytes != 0 || waiting != 0 {
		t.Fatalf("admission state left behind: %d %d %d", inflight, bytes, waiting)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.metrics.mu.Lock()
		retained := len(s.metrics.flights)
		s.metrics.mu.Unlock()
		if retained == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d metrics results retained while idle", retained)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("%d served, %d refused (slowest %v), %d writes (slowest %v), peak %d slots / %d MiB, heap growth %d MiB",
		ok.Load(), refused.Load(), time.Duration(slowestRefusal.Load()), written.Load(), time.Duration(slowestWrite.Load()),
		g.peakInflight, g.peakBytes>>20, (int64(peakHeap.Load())-baseHeap)>>20)
}

func TestMetricsEndpointSharesConcurrentRequests(t *testing.T) {
	s, _, _ := setup(t)
	mustCreate(t, s, "shared", "true")
	var wg sync.WaitGroup
	totals := make([]float64, 16)
	for i := range totals {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := call(s, true, "GET", "/api/v1/metrics/runs?range=1h", "", "", nil)
			if rec.Code != 200 {
				t.Errorf("metrics: %d %s", rec.Code, rec.Body.String())
				return
			}
			totals[i] = decode(t, rec)["total"].(float64)
		}()
	}
	wg.Wait()
	s.gate().mu.Lock()
	peak := s.gate().peakInflight
	s.gate().mu.Unlock()
	if peak != 1 {
		t.Fatalf("16 identical metrics requests held %d slots at once, want one shared computation", peak)
	}
}
