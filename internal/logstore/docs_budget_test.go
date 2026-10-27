package logstore

import (
	"os"
	"strings"
	"testing"
)

// The operator docs and the ADR describe the disk-budget contract; keep the text
// from drifting from the code (ADR-10).
func TestDocsDescribeTheDiskBudgetContract(t *testing.T) {
	docContains(t, "operations.md",
		"## Log disk budget",
		"outranks retention age",
		"`logs.disk_min_free` (MiB) | `512`",
		"never** deleted by this mechanism: live buffers",
		"`.quarantine/`",
		"Disk pressure never touches the quarantine",
		"`insufficient: true`",
		"`log_storage.writer_lock_waits`",
		"`terminal_persistence`",
		"`quarantine_bytes`",
	)
	docContains(t, "configuration.md",
		"`logs.disk_min_free`", "an explicit `0` disables the rule",
		"`logs.disk_budget`", "starts at 90% and stops at 80%",
		"`logs.quarantine_keep_for` / `logs.quarantine_max_size`",
	)
	if _, err := os.Stat("../../docs/adr/0010-log-disk-budget.md"); err != nil {
		t.Fatal(err)
	}
	docContains(t, "adr/0010-log-disk-budget.md",
		"### Eligible data", "### Accounting", "### Watermarks", "### If reclamation is insufficient",
		"### Quarantine", "### A06: sequence-oriented archive cursor", "## Accepted downsides",
	)
}

// The watermark constants in the docs are the ones in the code.
func TestDocumentedWatermarksMatchTheCode(t *testing.T) {
	if budgetHighPercent != 90 || budgetLowPercent != 80 || freeLowDivisor != 4 || freeMaxShare != 4 {
		t.Fatalf("watermark constants changed (%d/%d, +1/%d, clamp 1/%d): update docs/adr/0010-log-disk-budget.md, docs/operations.md and docs/configuration.md",
			budgetHighPercent, budgetLowPercent, freeLowDivisor, freeMaxShare)
	}
}

// The running-worker tail in the docs is the one in the code.
func TestDocumentedActiveTailMatchesTheCode(t *testing.T) {
	if activeTailBytes != 8<<20 {
		t.Fatalf("activeTailBytes = %d: update the 8 MiB in docs/adr/0010-log-disk-budget.md, docs/operations.md and docs/configuration.md", activeTailBytes)
	}
	docContains(t, "operations.md", "the newest\n8 MiB of archived chunks of runs that still have a writer")
	docContains(t, "configuration.md", "the newest 8 MiB of a running worker's archived chunks")
	docContains(t, "adr/0010-log-disk-budget.md", "## Amendment 2026-10-26: running workers keep only a recent tail")
}

// Review SF2: the docs must not promise admission control that does not exist.
func TestDocsStateAdmissionIsNotWiredToDiskPressure(t *testing.T) {
	docContains(t, "operations.md", "Admission is not wired to disk pressure in this build", "their log output is dropped")
	docContains(t, "adr/0010-log-disk-budget.md", "New work (4B) is not wired yet", "admission is unchanged")
	for _, f := range []string{"operations.md", "adr/0010-log-disk-budget.md"} {
		b, _ := os.ReadFile("../../docs/" + f)
		if strings.Contains(string(b), "new work follows the queue policy") || strings.Contains(string(b), "New work follows 4B") {
			t.Errorf("docs/%s still claims new work follows the queue policy", f)
		}
	}
}
