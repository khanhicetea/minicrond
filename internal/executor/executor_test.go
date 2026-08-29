package executor

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/minicron/minicron/internal/logstore"
	"github.com/minicron/minicron/internal/model"
	"github.com/minicron/minicron/internal/store"
)

func TestSuccessAndTimeoutRemainDistinct(t *testing.T) {
	st, err := store.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	logs, err := logstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := New(st, logs, Options{MaxConcurrentRuns: 2})
	for _, tc := range []struct{ name, command, timeout, want string }{{"ok", "exit 0", "0", "succeeded"}, {"slow", "sleep 5", "100ms", "timeout"}} {
		d := model.Definition{Name: tc.name, Kind: model.KindJob, Authority: "file", SourceFile: "test", Command: tc.command, Shell: "/bin/sh", Timeout: tc.timeout, Grace: "0", Timezone: "UTC", OnOverlap: "skip", EnvBase: "clean", SuccessCodes: []int{0}}
		if err := st.SyncFiles(t.Context(), []model.Definition{d}, false); err != nil {
			t.Fatal(err)
		}
		stored, hash, err := st.Definition(t.Context(), d.Name)
		if err != nil {
			t.Fatal(err)
		}
		r, err := service.Trigger(t.Context(), stored, hash, "manual", nil)
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(3 * time.Second)
		for {
			current, err := st.Run(t.Context(), r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if model.TerminalStatuses[current.Status] {
				if current.Status != tc.want {
					t.Fatalf("%s: got %s, want %s", tc.name, current.Status, tc.want)
				}
				if runtime.GOOS == "linux" && current.ProcessStartID == "" {
					t.Fatalf("%s: process start identity was not persisted", tc.name)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("run did not finish")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// A backgrounded descendant that inherits stdout must not delay the run
// forever, lose captured output, or produce spurious pump-error lines.
func TestDescendantHoldingPipeIsBoundedAndClean(t *testing.T) {
	st, err := store.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	logs, err := logstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := New(st, logs, Options{MaxConcurrentRuns: 2})
	d := model.Definition{Name: "spawner", Kind: model.KindJob, Authority: "file", SourceFile: "test",
		Command: "echo parent; sleep 10 & echo done", Shell: "/bin/sh", Timeout: "0", Grace: "0",
		Timezone: "UTC", OnOverlap: "skip", EnvBase: "clean", SuccessCodes: []int{0}}
	if err := st.SyncFiles(t.Context(), []model.Definition{d}, false); err != nil {
		t.Fatal(err)
	}
	stored, hash, err := st.Definition(t.Context(), "spawner")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	r, err := service.Trigger(t.Context(), stored, hash, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		current, err := st.Run(t.Context(), r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if model.TerminalStatuses[current.Status] {
			if elapsed := time.Since(start); elapsed > 7*time.Second {
				t.Fatalf("descendant held the run for %s; drain bound failed", elapsed)
			}
			if current.Status != "succeeded" {
				t.Fatalf("status = %s, want succeeded", current.Status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run did not finish within the drain bound")
		}
		time.Sleep(20 * time.Millisecond)
	}
	frames, err := logs.Read(r.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var sawParent, sawDone, sawPumpError bool
	for _, f := range frames {
		text := string(f.Payload)
		if strings.Contains(text, "parent") {
			sawParent = true
		}
		if strings.Contains(text, "done") {
			sawDone = true
		}
		if strings.Contains(text, "log pump error") {
			sawPumpError = true
		}
	}
	if !sawParent || !sawDone {
		t.Fatalf("captured output incomplete: parent=%v done=%v", sawParent, sawDone)
	}
	if sawPumpError {
		t.Fatal("spurious log pump error recorded")
	}
}

func TestOverlapSkipDeclinesWhileActive(t *testing.T) {
	st, err := store.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	logs, err := logstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := New(st, logs, Options{MaxConcurrentRuns: 4})
	d := model.Definition{Name: "solo", Kind: model.KindJob, Authority: "file", SourceFile: "test",
		Command: "sleep 2", Shell: "/bin/sh", Timeout: "0", Grace: "0", OnOverlap: "skip",
		Timezone: "UTC", EnvBase: "clean", SuccessCodes: []int{0}}
	if err := st.SyncFiles(t.Context(), []model.Definition{d}, false); err != nil {
		t.Fatal(err)
	}
	stored, hash, err := st.Definition(t.Context(), "solo")
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.Trigger(t.Context(), stored, hash, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Trigger(t.Context(), stored, hash, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatal("skip policy must record a distinct declined run")
	}
	if second.Status != "skipped" || second.EndReason != "overlap_skip" {
		t.Fatalf("declined run = %s/%s, want skipped/overlap_skip", second.Status, second.EndReason)
	}
	if err := service.Stop(first.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		current, err := st.Run(t.Context(), first.ID)
		if err != nil {
			t.Fatal(err)
		}
		if model.TerminalStatuses[current.Status] {
			if current.Status != "stopped" {
				t.Fatalf("stopped run status = %s", current.Status)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("run did not stop")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
