package model

import "time"

// WorkerExhaustion is a supervision outcome, not an execution run status.
// LastRunID may be empty when admission failed before a run could be created.
type WorkerExhaustion struct {
	Reason    string    `json:"reason"`
	LastRunID string    `json:"last_run_id,omitempty"`
	Attempts  int       `json:"attempts"`
	Revision  int64     `json:"revision"`
	At        time.Time `json:"at"`
}
