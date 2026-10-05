package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/khanhicetea/minicrond/internal/store"
)

// Expensive reads (JSON log pages, raw download pages, metrics) are rare
// diagnostics. They share one small admission gate so that a burst of them can
// neither pile up decoded pages and sort buffers in memory nor compete with job
// execution for CPU: excess requests wait briefly and are then refused with a
// retryable 503 (audit A14, ADR-8 decision 5).

// ReadLimits configures the shared admission gate for expensive reads. Zero
// fields select the defaults.
type ReadLimits struct {
	// Slots is the number of expensive reads that may run at once.
	Slots int
	// BudgetBytes bounds the summed estimated working set of those reads.
	BudgetBytes int64
	// WorkTimeout bounds one request's read/aggregation work. It is separate
	// from the socket write deadline, which is renewed for the response write.
	WorkTimeout time.Duration
}

const (
	DefaultReadSlots       = 4
	DefaultReadBudgetBytes = 32 << 20
	DefaultReadWorkTimeout = 20 * time.Second
	// MinReadBudgetBytes is the largest single-request estimate; a smaller
	// budget could never admit that request.
	MinReadBudgetBytes = metricsCost
)

// Estimated working sets, reserved against the byte budget while a request
// holds a slot. They are deliberately round upper-ish bounds, not measurements
// of each request: a 1 MiB payload page costs its decoded frames, base64 text
// and JSON encoding buffer; metrics cost O(rows) samples (about 7 MB at 30,000
// in-window runs).
const (
	logPageCost = 4 << 20
	rawPageCost = 4 << 20
	metricsCost = 8 << 20
)

// Timing knobs; variables so tests can shorten them.
var (
	// readAdmitWait is how long a new expensive request queues for a slot
	// before it is refused. Waiting is short because the caller can retry.
	readAdmitWait = 500 * time.Millisecond
	// readRetryAfter is advertised to clients refused with 503.
	readRetryAfter = time.Second
	// metricsCacheTTL is how long a computed metrics result is shared. It is
	// request-scoped: nothing refreshes it, and it is dropped when it expires.
	metricsCacheTTL = 3 * time.Second
)

var errReadBusy = errors.New("expensive reads are at capacity")

// readGate is a slot count plus a byte budget, with a bounded wait queue.
type readGate struct {
	mu           sync.Mutex
	slots        int
	budget       int64
	maxWaiters   int
	maxDownloads int // downloads hold a slot for their whole length; see acquireDownload
	inflight     int
	downloads    int
	bytes        int64
	waiting      int
	changed      chan struct{} // closed and replaced whenever capacity is released
	peakBytes    int64         // observed maxima, for tests and diagnostics
	peakInflight int
}

func newReadGate(l ReadLimits) *readGate {
	if l.Slots <= 0 {
		l.Slots = DefaultReadSlots
	}
	if l.BudgetBytes <= 0 {
		l.BudgetBytes = DefaultReadBudgetBytes
	}
	return &readGate{slots: l.Slots, budget: l.BudgetBytes, maxWaiters: 4 * l.Slots, maxDownloads: max(1, l.Slots-1), changed: make(chan struct{})}
}

// acquire reserves one slot and cost bytes, waiting up to wait. It returns
// errReadBusy when capacity does not appear in time or too many requests are
// already waiting, and ctx's error when the caller gives up first. A request
// is always admitted when nothing else runs, so an undersized budget cannot
// lock out all reads.
func (g *readGate) acquire(ctx context.Context, cost int64, wait time.Duration) (release func(), err error) {
	return g.acquireKind(ctx, cost, wait, false)
}

// acquireKind is acquire for a normal read or, with download set, a raw
// download. A download holds its slot for as long as its client keeps reading,
// so at most max(1, slots-1) run at once: with more than one slot, a JSON page
// or metrics request always has capacity left however many downloads are in
// progress. A download beyond that cap is refused at once rather than queued,
// because waiting for another download to finish would outlast any sensible
// admission wait.
func (g *readGate) acquireKind(ctx context.Context, cost int64, wait time.Duration, download bool) (release func(), err error) {
	var timer *time.Timer
	var timeout <-chan time.Time
	waiting := false
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		g.mu.Lock()
		if waiting {
			g.waiting--
			waiting = false
		}
		if download && g.downloads >= g.maxDownloads {
			g.mu.Unlock()
			return nil, errReadBusy
		}
		if g.inflight < g.slots && (g.inflight == 0 || g.bytes+cost <= g.budget) {
			g.inflight++
			if download {
				g.downloads++
			}
			g.bytes += cost
			g.peakBytes = max(g.peakBytes, g.bytes)
			g.peakInflight = max(g.peakInflight, g.inflight)
			g.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					g.mu.Lock()
					g.inflight--
					if download {
						g.downloads--
					}
					g.bytes -= cost
					close(g.changed)
					g.changed = make(chan struct{})
					g.mu.Unlock()
				})
			}, nil
		}
		if wait <= 0 || g.waiting >= g.maxWaiters {
			g.mu.Unlock()
			return nil, errReadBusy
		}
		g.waiting++
		waiting = true
		changed := g.changed
		g.mu.Unlock()
		if timer == nil {
			timer = time.NewTimer(wait)
			timeout = timer.C
		}
		select {
		case <-changed:
		case <-timeout:
			g.mu.Lock()
			g.waiting--
			waiting = false
			g.mu.Unlock()
			return nil, errReadBusy
		case <-ctx.Done():
			g.mu.Lock()
			g.waiting--
			waiting = false
			g.mu.Unlock()
			return nil, ctx.Err()
		}
	}
}

func (g *readGate) stats() (inflight int, bytes int64, waiting int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inflight, g.bytes, g.waiting
}

// SetReadLimits configures expensive-read admission. Call it before the HTTP
// listeners start; the defaults apply otherwise.
func (s *Server) SetReadLimits(l ReadLimits) {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	if l.WorkTimeout <= 0 {
		l.WorkTimeout = DefaultReadWorkTimeout
	}
	s.readGate = newReadGate(l)
	s.readWork = l.WorkTimeout
}

func (s *Server) gate() *readGate {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	if s.readGate == nil {
		s.readGate = newReadGate(ReadLimits{})
		s.readWork = DefaultReadWorkTimeout
	}
	return s.readGate
}

func (s *Server) workTimeout() time.Duration {
	s.gate()
	s.readMu.Lock()
	defer s.readMu.Unlock()
	return s.readWork
}

// readWorkContext derives the request-work deadline. The socket write
// deadline is managed separately (extendWriteDeadline before the response).
func (s *Server) readWorkContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.workTimeout())
}

// writeReadFailure maps an expensive-read error to a response: a retryable 503
// for admission refusal or an exceeded work deadline, nothing for a client that
// went away, 500 otherwise.
func (s *Server) writeReadFailure(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case r.Context().Err() != nil:
		// The client is gone; there is nobody to answer.
	case errors.Is(err, errReadBusy):
		writeBusy(w, "read_busy", "too many expensive reads in progress; retry shortly")
	case errors.Is(err, context.DeadlineExceeded):
		writeBusy(w, "read_timeout", "read did not finish within its work budget; retry later or narrow the request")
	default:
		internal(w, r, err)
	}
}

func writeBusy(w http.ResponseWriter, code, message string) {
	w.Header().Set("Retry-After", strconv.Itoa(max(1, int((readRetryAfter+time.Second-1)/time.Second))))
	writeError(w, http.StatusServiceUnavailable, code, message)
}

// admitRead reserves capacity for one expensive read, reporting a refusal
// through writeReadFailure.
func (s *Server) admitRead(w http.ResponseWriter, r *http.Request, cost int64) (release func(), ok bool) {
	return s.admit(w, r, cost, false)
}

func (s *Server) admit(w http.ResponseWriter, r *http.Request, cost int64, download bool) (release func(), ok bool) {
	release, err := s.gate().acquireKind(r.Context(), cost, readAdmitWait, download)
	if err != nil {
		s.writeReadFailure(w, r, err)
		return nil, false
	}
	return release, true
}

// metricsKey identifies one computed view. Requests with equal keys inside
// the TTL share a result.
type metricsKey struct {
	window  time.Duration
	buckets int
}

// metricsFlight is one in-progress or recently finished computation.
type metricsFlight struct {
	done   chan struct{}
	result store.RunMetrics
	err    error
}

// metricsShare coalesces concurrent metrics requests and reuses a result for a
// few seconds. Each key is computed by the first request's own goroutine, so
// it needs no background worker; entries are dropped by a timer started only
// after a computation, so an idle daemon retains nothing.
type metricsShare struct {
	mu      sync.Mutex
	flights map[metricsKey]*metricsFlight
}

// get returns the shared result for key, computing it with compute (under the
// caller's ctx) when none is in flight or fresh. A waiter whose ctx ends
// leaves without disturbing the others; if the computing request was canceled
// while others still wait, the next waiter computes instead.
func (m *metricsShare) get(ctx context.Context, key metricsKey, compute func(context.Context) (store.RunMetrics, error)) (store.RunMetrics, error) {
	for {
		m.mu.Lock()
		if m.flights == nil {
			m.flights = make(map[metricsKey]*metricsFlight)
		}
		flight, shared := m.flights[key]
		if !shared {
			flight = &metricsFlight{done: make(chan struct{})}
			m.flights[key] = flight
		}
		m.mu.Unlock()
		if !shared {
			return m.lead(ctx, key, flight, compute)
		}
		select {
		case <-flight.done:
			if flight.err != nil && errors.Is(flight.err, context.Canceled) && ctx.Err() == nil {
				continue // the computing request went away; try again
			}
			return flight.result, flight.err
		case <-ctx.Done():
			return store.RunMetrics{}, ctx.Err()
		}
	}
}

// lead computes a flight this request created and publishes the outcome.
func (m *metricsShare) lead(ctx context.Context, key metricsKey, flight *metricsFlight, compute func(context.Context) (store.RunMetrics, error)) (store.RunMetrics, error) {
	finished := false
	defer func() {
		if finished {
			return
		}
		// compute panicked: release the waiters instead of stranding them.
		m.mu.Lock()
		flight.err = errors.New("metrics computation aborted")
		delete(m.flights, key)
		close(flight.done)
		m.mu.Unlock()
	}()
	result, err := compute(ctx)
	finished = true
	m.mu.Lock()
	flight.result, flight.err = result, err
	if err != nil {
		// Failures are never cached.
		delete(m.flights, key)
	} else {
		time.AfterFunc(metricsCacheTTL, func() {
			m.mu.Lock()
			if m.flights[key] == flight {
				delete(m.flights, key)
			}
			m.mu.Unlock()
		})
	}
	close(flight.done)
	m.mu.Unlock()
	return result, err
}
