package supervisor

import "context"

// Shutdown joins supervision without a deadline.
func (s *Supervisor) Shutdown() { _ = s.ShutdownContext(context.Background()) }

func (s *Supervisor) State(name string) State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateLocked(name)
}
