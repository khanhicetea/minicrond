package store

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/khanhicetea/minicrond/internal/model"
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

	def := model.Definition{Name: "scheduled", Kind: model.KindJob, Schedule: "@every 1m", Timezone: "UTC"}
	if _, err := st.PutDefinition(t.Context(), def, 0, "test"); err != nil {
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

func TestCrashRecoveryAndRetentionKeepNewest(t *testing.T) {
	s, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d := model.Definition{Name: "job", Kind: model.KindJob, Command: "true", Shell: "/bin/sh", Timezone: "UTC", Timeout: 0, Grace: 0, OnOverlap: "skip", CatchUp: "none", SuccessCodes: []int{0}}
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
	ids, err := s.RetentionCandidates(t.Context(), d.ID, 1, time.Time{}, nil, 10)
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

func TestLegacyAuthoritySchemaRefused(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", dir+"/minicron.db")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version=1"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, err = Open(t.Context(), dir)
	if err == nil || !strings.Contains(err.Error(), "remove the development database") {
		t.Fatalf("expected incompatible-schema error, got %v", err)
	}
}

func TestRunMetricsCountsBeyondRunsLimit(t *testing.T) {
	s, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	def := model.Definition{Name: "metric", Kind: model.KindJob, Command: "true", Shell: "/bin/sh", Timezone: "UTC", Timeout: 0, Grace: 0, OnOverlap: "skip", CatchUp: "none", SuccessCodes: []int{0}}
	if _, err := s.PutDefinition(t.Context(), def, 0, "test"); err != nil {
		t.Fatal(err)
	}
	stored, _, err := s.Definition(t.Context(), "metric")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i := range 501 {
		at := now.Add(-time.Duration(i) * time.Second)
		run := model.Run{ID: fmt.Sprintf("metric-%d", i), DefinitionID: stored.ID, Job: "metric", Kind: model.KindJob, Revision: 1, Status: "succeeded", Trigger: "schedule", QueuedAt: at, EndedAt: &at}
		if err := s.CreateRun(t.Context(), run); err != nil {
			t.Fatal(err)
		}
	}
	metrics, err := s.RunMetrics(t.Context(), now.Add(-time.Hour), now, 48)
	if err != nil {
		t.Fatal(err)
	}
	if metrics.Total != 501 || metrics.Succeeded != 501 {
		t.Fatalf("metrics counted %d/%d runs, want 501/501", metrics.Total, metrics.Succeeded)
	}
}

func TestRunsPageKeepsStableOrderWhenNewRunArrives(t *testing.T) {
	s, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	def := model.Definition{Name: "pages", Kind: model.KindJob, Command: "true", Shell: "/bin/sh", Timezone: "UTC", SuccessCodes: []int{0}}
	if _, err := s.PutDefinition(t.Context(), def, 0, "test"); err != nil {
		t.Fatal(err)
	}
	stored, _, err := s.Definition(t.Context(), def.Name)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-time.Minute)
	add := func(id, status, trigger string, at time.Time) {
		t.Helper()
		if err := s.CreateRun(t.Context(), model.Run{ID: id, DefinitionID: stored.ID, Job: def.Name, Kind: model.KindJob, Revision: 1, Status: status, Trigger: trigger, QueuedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	add("run-a", "failed", "manual", base)
	add("run-b", "succeeded", "schedule", base)
	add("run-c", "running", "manual", base.Add(time.Second))
	first, err := s.RunsPage(t.Context(), def.Name, 2, "", "")
	if err != nil || len(first) != 2 || first[0].ID != "run-c" || first[1].ID != "run-b" {
		t.Fatalf("first page = %+v, %v", first, err)
	}
	add("run-new", "running", "manual", base.Add(2*time.Second))
	second, err := s.RunsPage(t.Context(), def.Name, 2, first[1].ID, "")
	if err != nil || len(second) != 1 || second[0].ID != "run-a" {
		t.Fatalf("second page = %+v, %v", second, err)
	}
	failed, err := s.RunsPage(t.Context(), def.Name, 10, "", "failed")
	if err != nil || len(failed) != 1 || failed[0].ID != "run-a" {
		t.Fatalf("failed filter = %+v, %v", failed, err)
	}
}

func TestRetentionKeepsNewestTerminalRun(t *testing.T) {
	s, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	def := model.Definition{Name: "ret", Kind: model.KindJob, Command: "true", Shell: "/bin/sh", Timezone: "UTC", Timeout: 0, Grace: 0, OnOverlap: "skip", CatchUp: "none", SuccessCodes: []int{0}}
	if _, err := s.PutDefinition(t.Context(), def, 0, "test"); err != nil {
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
	stale, err := s.RetentionCandidates(t.Context(), stored.ID, 2, time.Now().UTC().Add(-1000*time.Hour), nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 3 {
		t.Fatalf("expected 3 candidates past keep=2, got %v", stale)
	}
	for _, id := range stale {
		if id.ID == ids[4] {
			t.Fatal("newest terminal run must always be retained")
		}
		if err := s.DeleteRun(t.Context(), id.ID); err != nil {
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
	stale, _ = s.RetentionCandidates(t.Context(), stored.ID, 0, cutoff, nil, 10)
	if len(stale) == 0 || stale[0].ID == ids[4] {
		t.Fatalf("age cap must expire old runs but keep the newest: %v", stale)
	}
}

func TestRetentionCandidatesPagesAndIdempotency(t *testing.T) {
	s, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	def, err := s.PutDefinition(t.Context(), model.Definition{
		Name: "pages", Kind: model.KindJob, Command: "true", Shell: "/bin/sh", Timezone: "UTC",
		OnOverlap: "skip", CatchUp: "none", SuccessCodes: []int{0},
	}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-48 * time.Hour)
	for i := range 7 {
		at := base.Add(time.Duration(i) * time.Hour)
		if i == 3 {
			at = base.Add(4 * time.Hour) // Equal sort keys need a stable run-ID tie break.
		}
		r := model.Run{ID: fmt.Sprintf("run-%d", i), DefinitionID: def.ID, Job: def.Name,
			Kind: def.Kind, Revision: def.Revision, Status: "succeeded", Trigger: "manual", QueuedAt: at, EndedAt: &at}
		if err := s.CreateRun(t.Context(), r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SaveIdempotency(t.Context(), "client", "trigger", "protected", "hash", "run-2"); err != nil {
		t.Fatal(err)
	}
	// An expired key must not hold its run indefinitely.
	if _, err := s.db.ExecContext(t.Context(), `INSERT INTO idempotency(principal,operation,key,request_hash,run_id,created_us)
		VALUES('client','trigger','expired','hash','run-1',?)`, time.Now().Add(-25*time.Hour).UnixMicro()); err != nil {
		t.Fatal(err)
	}
	var got []string
	var cursor *RetentionCandidate
	for {
		page, err := s.RetentionCandidates(t.Context(), def.ID, 2, time.Time{}, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, candidate := range page {
			got = append(got, candidate.ID)
			if err := s.DeleteRun(t.Context(), candidate.ID); err != nil {
				t.Fatal(err)
			}
		}
		cursor = &page[len(page)-1]
	}
	want := []string{"run-4", "run-3", "run-1", "run-0"}
	if !slices.Equal(got, want) {
		t.Fatalf("paged candidates = %v, want %v", got, want)
	}
	page, err := s.RetentionCandidates(t.Context(), def.ID, 0, time.Time{}, nil, 10)
	if err != nil || len(page) != 0 {
		t.Fatalf("retention with no count or age cap = %v, %v", page, err)
	}
	// Age expiration may remove a run inside the count allowance, but never
	// the newest run. The protected run remains until its key expires.
	page, err = s.RetentionCandidates(t.Context(), def.ID, 2, time.Now().Add(-time.Hour), nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].ID != "run-5" {
		t.Fatalf("age candidates = %v, want run-5", page)
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
