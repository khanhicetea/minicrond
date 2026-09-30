package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

func TestIdempotencyErrorsRemainDistinctFromDatabaseFailures(t *testing.T) {
	s, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	d, err := s.PutDefinition(t.Context(), model.Definition{Name: "job", Kind: model.KindJob, Command: "true"}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	run := model.Run{ID: "run", DefinitionID: d.ID, Job: d.Name, Kind: d.Kind, Revision: d.Revision, DefinitionHash: "hash", Status: "pending", Trigger: "manual", Attempt: 1, QueuedAt: time.Now()}
	if err := s.CreateRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveIdempotency(t.Context(), "admin", "trigger", "key", "hash", run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IdempotentRun(t.Context(), "admin", "trigger", "key", "changed"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("request conflict lost its type: %v", err)
	}
	if err := s.SaveIdempotency(t.Context(), "admin", "trigger", "key", "hash", run.ID); !errors.Is(err, ErrIdempotencyKeyExists) {
		t.Fatalf("reserved key lost its type: %v", err)
	}
	if _, err := s.AdmitIdempotentRun(t.Context(), run, "admin", "trigger", "key", "changed"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("admission conflict lost its type: %v", err)
	}
	if _, err := s.IdempotentRun(t.Context(), "admin", "trigger", "missing", "hash"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing reservation lost its cause: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.IdempotentRun(ctx, "admin", "trigger", "key", "hash"); !errors.Is(err, context.Canceled) || errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("database failure was treated as a conflict: %v", err)
	}
}
