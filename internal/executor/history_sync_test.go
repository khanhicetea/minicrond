package executor

import (
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

// X1 / ADR-23: an on_history_unavailable = "skip" job's run row and terminal
// state keep per-write FULL commits; other jobs' history is committed relaxed
// and synced on the cadence.
func TestSkipJobHistoryIsCommittedFull(t *testing.T) {
	t.Setenv("MINICROND_HISTORY_SYNC", "")
	_, st, s := resilienceService(t, Options{MaxConcurrentRuns: 2})
	if !st.HistorySyncStats().Deferred {
		t.Fatal("deferred history sync is off on a FULL store")
	}
	runJob := func(d model.Definition, hash string) {
		t.Helper()
		r, err := s.Trigger(t.Context(), d, hash, "manual", nil)
		if err != nil {
			t.Fatal(err)
		}
		waitDone(t, s, r.ID)
		eventually(t, 10*time.Second, "terminal state not stored", func() bool {
			got, err := st.Run(t.Context(), r.ID)
			return err == nil && got.Status == "succeeded"
		})
	}

	skip, skipHash := putJob(t, st, model.Definition{Name: "billing", Kind: model.KindJob, Command: "true", Shell: "/bin/sh", OnOverlap: "skip", OnHistoryUnavailable: "skip", SuccessCodes: []int{0}})
	before := st.HistorySyncStats().RelaxedWrites
	runJob(skip, skipHash)
	if got := st.HistorySyncStats().RelaxedWrites; got != before {
		t.Fatalf("skip job made %d relaxed history writes, want 0", got-before)
	}

	plain, plainHash := putJob(t, st, model.Definition{Name: "cleanup", Kind: model.KindJob, Command: "true", Shell: "/bin/sh", OnOverlap: "skip", SuccessCodes: []int{0}})
	runJob(plain, plainHash)
	if got := st.HistorySyncStats().RelaxedWrites; got != before+2 {
		t.Fatalf("plain job made %d relaxed history writes, want 2 (row and terminal state)", got-before)
	}
}
