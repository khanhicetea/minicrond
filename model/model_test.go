package model

import "testing"

func TestTerminalStatuses(t *testing.T) {
	for _, status := range []string{"succeeded", "failed", "timeout", "stopped", "interrupted", "skipped", "missed"} {
		if !Terminal(status) {
			t.Errorf("expected %s to be terminal", status)
		}
	}
	for _, status := range []string{"pending", "running", ""} {
		if Terminal(status) {
			t.Errorf("expected %s to be non-terminal", status)
		}
	}
}
