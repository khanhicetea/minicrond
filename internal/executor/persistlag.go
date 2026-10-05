package executor

import (
	"sync/atomic"
	"time"
)

// persistLag measures how long a run's terminal state took to become durable
// after its process ended: normally a few milliseconds, longer while the
// metadata database is slow, unbounded while the finalizer is still retrying.
// It records only what the completion path already does; nothing runs for it.
type persistLag struct {
	count, totalNS, lastNS, maxNS atomic.Int64
	pending                       atomic.Int64 // terminal states still waiting for the background finalizer
	oldestPendingEnded            atomic.Int64 // unix nanos of the oldest still-pending run's end, 0 if none (approximate)
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
		Pending:   p.pending.Load(),
	}
	if l.Pending > 0 {
		if ended := p.oldestPendingEnded.Load(); ended > 0 {
			l.PendingOldestAgeMS = max(time.Since(time.Unix(0, ended)).Milliseconds(), 0)
		}
	}
	return l
}

// startPending notes a terminal state handed to the background finalizer.
func (p *persistLag) startPending(ended time.Time) {
	p.pending.Add(1)
	if ended.IsZero() {
		return
	}
	n := ended.UnixNano()
	for {
		old := p.oldestPendingEnded.Load()
		if old != 0 && old <= n || p.oldestPendingEnded.CompareAndSwap(old, n) {
			return
		}
	}
}

func (p *persistLag) endPending() {
	if p.pending.Add(-1) == 0 {
		p.oldestPendingEnded.Store(0)
	}
}
