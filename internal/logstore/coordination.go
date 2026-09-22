package logstore

import "sync"

// runLock lives only while an operation holds or is waiting for its lock.
// Keeping references for waiters prevents two locks being used for one run,
// without retaining a lock for every historical run forever.
type runLock struct {
	mu   sync.RWMutex
	refs int
}

// Lock order: archiveMu (archive operations only), run lock, Writer.mu,
// Store.mu. Store.mu is released before waiting on a run lock.
func (s *Store) lockRun(runID string, exclusive bool) func() {
	s.mu.Lock()
	lock := s.runLocks[runID]
	if lock == nil {
		lock = &runLock{}
		s.runLocks[runID] = lock
	}
	lock.refs++
	s.mu.Unlock()
	if exclusive {
		lock.mu.Lock()
	} else {
		lock.mu.RLock()
	}
	return func() {
		if exclusive {
			lock.mu.Unlock()
		} else {
			lock.mu.RUnlock()
		}
		s.mu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(s.runLocks, runID)
		}
		s.mu.Unlock()
	}
}
