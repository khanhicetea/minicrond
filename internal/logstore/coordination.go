package logstore

import "sync"

// runLock lives only while an operation holds or is waiting for its lock.
// Keeping references for waiters prevents two locks being used for one run,
// without retaining a lock for every historical run forever.
type runLock struct {
	mu   sync.RWMutex
	refs int
}

// Lock order: run ownership (archive and delete operations only, see
// Store.claim), an archive slot, run lock, Writer.mu, Store.mu. Store.mu is
// released before waiting on a run lock.
//
// Acquisition and release are separate calls because this lock is taken for
// every accepted log frame. Returning an unlock closure here makes that hot
// path allocate even though the lock itself is already reference counted.
func (s *Store) lockRun(runID string, exclusive bool) *runLock {
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
	return lock
}

func (s *Store) unlockRun(runID string, lock *runLock, exclusive bool) {
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
