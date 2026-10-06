package executor

// ProcessTarget identifies a run's direct child, not its descendants.
type ProcessTarget struct {
	RunID   string `json:"run_id"`
	Job     string `json:"job"`
	Kind    string `json:"kind"`
	PID     int    `json:"pid"`
	StartID string `json:"process_start_id"`
}

// MonitorTargets copies at most limit identities under the execution lock.
// procfs I/O happens later, outside this lock. No sampling runs in background.
func (s *Service) MonitorTargets(limit int) ([]ProcessTarget, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	targets := make([]ProcessTarget, 0, min(max(limit, 0), len(s.active)))
	for _, a := range s.active {
		if len(targets) >= limit {
			break
		}
		targets = append(targets, a.monitor)
	}
	return targets, len(s.active)
}
