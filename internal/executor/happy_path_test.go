package executor

import (
	"context"
	"testing"

	"github.com/khanhicetea/minicrond/internal/model"
)

// Concurrent triggers of an on_overlap = skip job start exactly one run: the
// overlap reservation is made with the admission decision.
func TestConcurrentTriggersStartOneRun(t *testing.T) {
	_, st, s := resilienceService(t, Options{MaxConcurrentRuns: 4})
	d, hash := putJob(t, st, model.Definition{Name: "wave", Kind: model.KindJob, Command: "sleep 0.2", Shell: "/bin/sh", OnOverlap: "skip", SuccessCodes: []int{0}})
	const triggers = 8
	results := make(chan model.Run, triggers)
	for range triggers {
		go func() {
			r, err := s.Trigger(context.Background(), d, hash, "manual", nil)
			if err != nil {
				t.Error(err)
			}
			results <- r
		}()
	}
	started := 0
	for range triggers {
		r := <-results
		switch {
		case r.Status == "pending":
			started++
			if ch := s.Wait(r.ID); ch != nil {
				defer func() { <-ch }()
			}
		case r.Status == "skipped" && r.EndReason == "overlap_skip":
		default:
			t.Fatalf("unexpected run %q/%q", r.Status, r.EndReason)
		}
	}
	if started != 1 {
		t.Fatalf("%d of %d concurrent triggers of an on_overlap=skip job started, want exactly 1", started, triggers)
	}
}
