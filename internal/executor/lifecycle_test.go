package executor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/logstore"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/store"
)

func TestConcurrentTriggersRespectOverlapAndShutdown(t *testing.T) {
	st, err := store.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	logs, err := logstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := New(st, logs, Options{MaxConcurrentRuns: 32})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.Shutdown(ctx)
	})
	d := model.Definition{Name: "solo", Kind: model.KindJob, Command: "sleep 30", Shell: "/bin/sh", Grace: 0, Timezone: "UTC", OnOverlap: "skip", SuccessCodes: []int{0}}
	if _, err := st.PutDefinition(t.Context(), d, 0, "test"); err != nil {
		t.Fatal(err)
	}
	d, hash, err := st.Definition(t.Context(), d.Name)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			<-start
			if _, err := s.Trigger(t.Context(), d, hash, "manual", nil); err != nil {
				t.Error(err)
			}
		})
	}
	close(start)
	wg.Wait()
	runs, err := st.Runs(t.Context(), d.Name, 100)
	if err != nil {
		t.Fatal(err)
	}
	started := 0
	for _, r := range runs {
		if r.Status != "skipped" {
			started++
		}
	}
	if len(runs) != 20 || started != 1 {
		t.Fatalf("got %d runs, %d admitted; want 20 and 1", len(runs), started)
	}
	s.Shutdown(t.Context())
	if _, err := s.Trigger(t.Context(), d, hash, "manual", nil); !errors.Is(err, ErrShutdown) {
		t.Fatalf("trigger after shutdown: %v", err)
	}
	if got := s.Active(d.Name); got != 0 {
		t.Fatalf("active after shutdown: %d", got)
	}
}

func TestTriggerCanceledContext(t *testing.T) {
	s := New(nil, nil, Options{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Trigger(ctx, model.Definition{}, "", "manual", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestProcessStartID(t *testing.T) {
	for _, name := range []string{"simple", "name with spaces", "name ) with ( parentheses)"} {
		stat := "123 (" + name + ") S " + strings.Repeat("0 ", 18) + "987654 0 0"
		if got := processStartID(stat); got != "987654" {
			t.Errorf("%q: got %q", name, got)
		}
	}
	for _, stat := range []string{"", "123 missing", "123 (short) S 0"} {
		if got := processStartID(stat); got != "" {
			t.Errorf("malformed stat %q: got %q", stat, got)
		}
	}
}
