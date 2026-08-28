package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/minicron/minicron/internal/model"
)

func TestFreshMigrationAndReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Meta(t.Context(), "missing"); err != sql.ErrNoRows || got != "" {
		t.Fatalf("got %q, %v", got, err)
	}
	if err := s.SetMeta(t.Context(), "instance", "one"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, err := s.Meta(t.Context(), "instance"); err != nil || got != "one" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestScheduleNextRoundTrip(t *testing.T) {
	st, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	def := model.Definition{Name: "scheduled", Kind: model.KindJob, Authority: "file", SourceFile: "jobs.toml", Schedule: "@every 1m", Timezone: "UTC"}
	if err := st.SyncFiles(t.Context(), []model.Definition{def}, false); err != nil {
		t.Fatal(err)
	}
	stored, _, err := st.Definition(t.Context(), def.Name)
	if err != nil {
		t.Fatal(err)
	}
	anchor := time.Date(2026, time.January, 1, 10, 0, 0, 0, time.UTC)
	next := anchor.Add(time.Minute)
	if err := st.SetScheduleStateWithNext(t.Context(), stored.ID, "hash", anchor, anchor, next); err != nil {
		t.Fatal(err)
	}
	got, err := st.ScheduleNext(t.Context(), stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(next) {
		t.Fatalf("next fire = %s, want %s", got, next)
	}

	if err := st.SetScheduleState(t.Context(), stored.ID, "new-hash", anchor, time.Time{}); err != nil {
		t.Fatal(err)
	}
	got, err = st.ScheduleNext(t.Context(), stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsZero() {
		t.Fatalf("next fire after reset = %s, want zero", got)
	}
}

func TestAuthorityConflictRollsBackWholeReload(t *testing.T) {
	s, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	dbDef := model.Definition{Name: "owned", Kind: model.KindJob, Command: "true", Shell: "/bin/sh", Timezone: "UTC", Timeout: "0", Grace: "0", OnOverlap: "skip", CatchUp: "none", SuccessCodes: []int{0}}
	if _, err := s.PutDefinition(t.Context(), dbDef, 0, "test"); err != nil {
		t.Fatal(err)
	}
	fileDef := dbDef
	fileDef.Authority = "file"
	fileDef.SourceFile = "jobs.toml"
	fileDef.Command = "false"
	if err := s.SyncFiles(t.Context(), []model.Definition{fileDef}, false); err == nil {
		t.Fatal("expected authority conflict")
	}
	got, _, err := s.Definition(t.Context(), "owned")
	if err != nil {
		t.Fatal(err)
	}
	if got.Command != "true" || got.Authority != "db" {
		t.Fatalf("definition changed: %#v", got)
	}
}

func TestCopyDefinitionsIsAtomicOnAuthorityConflict(t *testing.T) {
	s, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	file := model.Definition{Name: "file-owned", Kind: model.KindJob, Authority: "file", SourceFile: "jobs.toml", Command: "true", Shell: "/bin/sh", Timezone: "UTC", Timeout: "0", Grace: "0", OnOverlap: "skip", CatchUp: "none", SuccessCodes: []int{0}}
	if err = s.SyncFiles(t.Context(), []model.Definition{file}, false); err != nil {
		t.Fatal(err)
	}
	newDef := file
	newDef.Name = "new-copy"
	newDef.Authority = "db"
	newDef.SourceFile = ""
	conflict := file
	conflict.Authority = "db"
	conflict.SourceFile = ""
	if err = s.CopyDefinitions(t.Context(), []model.Definition{newDef, conflict}, "test"); err == nil {
		t.Fatal("expected conflict")
	}
	if _, _, err = s.Definition(t.Context(), "new-copy"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("partial import committed: %v", err)
	}
}

func TestCrashRecoveryAndRetentionKeepNewest(t *testing.T) {
	s, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d := model.Definition{Name: "job", Kind: model.KindJob, Command: "true", Shell: "/bin/sh", Timezone: "UTC", Timeout: "0", Grace: "0", OnOverlap: "skip", CatchUp: "none", SuccessCodes: []int{0}}
	d, err = s.PutDefinition(t.Context(), d, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i, status := range []string{"running", "succeeded", "failed"} {
		r := model.Run{ID: fmt.Sprintf("run-%d", i), DefinitionID: d.ID, Job: d.Name, Kind: d.Kind, Revision: d.Revision, DefinitionHash: "hash", Status: status, Trigger: "manual", Attempt: 1, QueuedAt: now.Add(time.Duration(i) * time.Second)}
		if err := s.CreateRun(t.Context(), r); err != nil {
			t.Fatal(err)
		}
		if status != "running" {
			if err := s.FinishRun(t.Context(), r.ID, status, "exit", nil, "", now.Add(time.Duration(i)*time.Second), 0, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.Run(t.Context(), "run-0")
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != "interrupted" {
		t.Fatalf("status = %s", recovered.Status)
	}
	ids, err := s.RetentionCandidates(t.Context(), d.ID, 1, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("candidates = %v", ids)
	}
}

func TestNewerSchemaRefused(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", dir+"/minicron.db")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version=99"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, err = Open(t.Context(), dir)
	if err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("expected newer-schema error, got %v", err)
	}
}

func TestRetentionKeepsNewestTerminalRun(t *testing.T) {
	s, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	def := model.Definition{Name: "ret", Kind: model.KindJob, Authority: "file", SourceFile: "t",
		Command: "true", Shell: "/bin/sh", Timezone: "UTC", Timeout: "0", Grace: "0", OnOverlap: "skip", CatchUp: "none", SuccessCodes: []int{0}}
	if err := s.SyncFiles(t.Context(), []model.Definition{def}, false); err != nil {
		t.Fatal(err)
	}
	stored, _, err := s.Definition(t.Context(), "ret")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-48 * time.Hour)
	ids := []string{}
	for i := range 5 {
		end := base.Add(time.Duration(i) * time.Hour)
		id := fmt.Sprintf("run-%d", i)
		ids = append(ids, id)
		r := model.Run{ID: id, DefinitionID: stored.ID, Job: "ret", Kind: model.KindJob, Revision: 1,
			Status: "succeeded", Trigger: "schedule", QueuedAt: end, EndedAt: &end}
		if err := s.CreateRun(t.Context(), r); err != nil {
			t.Fatal(err)
		}
	}
	// keep=2 and nothing older than cutoff: only count overflow applies.
	stale, err := s.RetentionCandidates(t.Context(), stored.ID, 2, time.Now().UTC().Add(-1000*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 3 {
		t.Fatalf("expected 3 candidates past keep=2, got %v", stale)
	}
	for _, id := range stale {
		if id == ids[4] {
			t.Fatal("newest terminal run must always be retained")
		}
		if err := s.DeleteRun(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := s.Runs(t.Context(), "ret", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("retention left %d runs, want 2", len(runs))
	}
	// Age cap: everything except the newest eventually expires.
	cutoff := time.Now().UTC().Add(-1 * time.Hour)
	stale, _ = s.RetentionCandidates(t.Context(), stored.ID, 0, cutoff)
	if len(stale) == 0 || stale[0] == ids[4] {
		t.Fatalf("age cap must expire old runs but keep the newest: %v", stale)
	}
}

func TestUpgradeFromOlderSchemaFixture(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", dir+"/minicron.db")
	if err != nil {
		t.Fatal(err)
	}
	// A pre-v0.1 fixture: meta table only, user_version 0.
	if _, err := db.Exec("CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO meta VALUES ('legacy','kept')"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if version, err := s.SchemaVersion(t.Context()); err != nil || version != SchemaVersion {
		t.Fatalf("schema version after upgrade = %d, %v", version, err)
	}
	if got, err := s.Meta(t.Context(), "legacy"); err != nil || got != "kept" {
		t.Fatalf("fixture data lost during upgrade: %q, %v", got, err)
	}
}
