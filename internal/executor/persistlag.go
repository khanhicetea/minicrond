package executor

import (
	"sync"
	"sync/atomic"
	"time"
)

// persistLag measures how long a run's terminal state took to become durable
// after its process ended: normally a few milliseconds, longer while the
// metadata database is slow, unbounded while the finalizer is still retrying.
// It records only what the completion path already does; nothing runs for it.
type persistLag struct {
	count, totalNS, lastNS, maxNS atomic.Int64

	// pending holds the end time of every terminal state still waiting for the
	// background finalizer, keyed by a token. It is bounded by the number of
	// such finalizers, and a mutex (not racing atomics) keeps the count and the
	// oldest age consistent.
	mu      sync.Mutex
	next    uint64
	pending map[uint64]time.Time
}

func (p *persistLag) observe(ended time.Time) {
	if ended.IsZero() {
		return
	}
	d := max(time.Since(ended), 0)
	p.count.Add(1)
	p.totalNS.Add(int64(d))
	p.lastNS.Store(int64(d))
	for {
		old := p.maxNS.Load()
		if int64(d) <= old || p.maxNS.CompareAndSwap(old, int64(d)) {
			return
		}
	}
}

// PersistenceLag reports terminal-state persistence lag since the daemon
// started.
type PersistenceLag struct {
	// Persisted is the number of terminal states stored; LastMS, MaxMS and
	// TotalMS measure end-of-process to durable-commit time.
	Persisted int64 `json:"persisted"`
	LastMS    int64 `json:"last_ms"`
	MaxMS     int64 `json:"max_ms"`
	TotalMS   int64 `json:"total_ms"`
	// Pending is the number of runs whose terminal state could not be stored
	// inline and is being retried in the background; PendingOldestAgeMS is how
	// long the oldest of them has been waiting. Pending should be zero.
	Pending            int64 `json:"pending"`
	PendingOldestAgeMS int64 `json:"pending_oldest_age_ms"`
}

// PersistenceLag returns the terminal-persistence lag statistics.
func (s *Service) PersistenceLag() PersistenceLag { return s.persistLag.snapshot() }

func (p *persistLag) snapshot() PersistenceLag {
	l := PersistenceLag{
		Persisted: p.count.Load(),
		LastMS:    p.lastNS.Load() / int64(time.Millisecond),
		MaxMS:     p.maxNS.Load() / int64(time.Millisecond),
		TotalMS:   p.totalNS.Load() / int64(time.Millisecond),
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	l.Pending = int64(len(p.pending))
	var oldest time.Time
	for _, ended := range p.pending {
		if !ended.IsZero() && (oldest.IsZero() || ended.Before(oldest)) {
			oldest = ended
		}
	}
	if !oldest.IsZero() {
		l.PendingOldestAgeMS = max(time.Since(oldest).Milliseconds(), 0)
	}
	return l
}

// startPending notes a terminal state handed to the background finalizer and
// returns the token to pass to endPending.
func (p *persistLag) startPending(ended time.Time) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending == nil {
		p.pending = make(map[uint64]time.Time)
	}
	p.next++
	p.pending[p.next] = ended
	return p.next
}

func (p *persistLag) endPending(token uint64) {
	p.mu.Lock()
	delete(p.pending, token)
	p.mu.Unlock()
}
