package executor

import (
	"context"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/logstore"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/store"
)

func TestFailedJobRetriesAndLinksAttempts(t *testing.T) {
	st, err := store.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	logs, err := logstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan model.Run, 4)
	svc := New(st, logs, Options{OnFinished: func(r model.Run, _ model.Definition) { finished <- r }})
	defer svc.Shutdown(context.Background())
	d := model.Definition{Name: "flaky", Kind: model.KindJob, Command: "exit 1", Shell: "/bin/sh", OnOverlap: "skip", Retries: 2, RetryDelay: 1}
	if _, err := st.PutDefinition(t.Context(), d, 0, "test"); err != nil {
		t.Fatal(err)
	}
	d, hash, err := st.Definition(t.Context(), d.Name)
	if err != nil {
		t.Fatal(err)
	}
	first, err := svc.Trigger(t.Context(), d, hash, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	parent := ""
	for attempt := 1; attempt <= 3; attempt++ {
		select {
		case r := <-finished:
			persisted, err := st.Run(t.Context(), r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if r.Attempt != attempt || r.Status != "failed" || r.ParentRunID != parent || persisted.ParentRunID != parent {
				t.Fatalf("attempt %d: run = %+v, persisted = %+v", attempt, r, persisted)
			}
			if attempt == 1 && r.ID != first.ID || attempt > 1 && r.Trigger != "retry" {
				t.Fatalf("attempt %d: unexpected run = %+v", attempt, r)
			}
			parent = r.ID
		case <-time.After(5 * time.Second):
			t.Fatalf("attempt %d never finished", attempt)
		}
	}
	select {
	case r := <-finished:
		t.Fatalf("unexpected extra attempt: %+v", r)
	case <-time.After(1200 * time.Millisecond):
	}
}

func TestRetryTimerCancelledOnShutdown(t *testing.T) {
	st, err := store.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	logs, err := logstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan model.Run, 2)
	svc := New(st, logs, Options{OnFinished: func(r model.Run, _ model.Definition) { finished <- r }})
	d := model.Definition{Name: "cancelled", Kind: model.KindJob, Command: "exit 1", Shell: "/bin/sh", OnOverlap: "skip", Retries: 1, RetryDelay: 1}
	if _, err := st.PutDefinition(t.Context(), d, 0, "test"); err != nil {
		t.Fatal(err)
	}
	d, hash, err := st.Definition(t.Context(), d.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Trigger(t.Context(), d, hash, "manual", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("initial run never finished")
	}
	if err := svc.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-finished:
		t.Fatalf("retry after shutdown: %+v", r)
	case <-time.After(1200 * time.Millisecond):
	}
}
