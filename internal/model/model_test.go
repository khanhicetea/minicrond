package model

import "testing"

func TestRunTransitions(t *testing.T) {
	allowed := [][2]string{{"pending", "running"}, {"pending", "failed"}, {"pending", "skipped"}, {"pending", "missed"}, {"running", "succeeded"}, {"running", "failed"}, {"running", "timeout"}, {"running", "stopped"}, {"running", "interrupted"}}
	for _, transition := range allowed {
		if !CanTransition(transition[0], transition[1]) {
			t.Errorf("expected %s -> %s to be allowed", transition[0], transition[1])
		}
	}
	for _, transition := range [][2]string{{"succeeded", "running"}, {"pending", "succeeded"}, {"running", "skipped"}, {"timeout", "succeeded"}} {
		if CanTransition(transition[0], transition[1]) {
			t.Errorf("expected %s -> %s to be forbidden", transition[0], transition[1])
		}
	}
}
