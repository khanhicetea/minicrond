package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/khanhicetea/minicrond/internal/model"
)

func channelFixture(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := s.PutAlertChannel(t.Context(), model.AlertChannel{Name: "ops", Type: "telegram", BotToken: "123:private_token", ChatID: "-123"}, "test"); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAlertChannelPersistenceAndRedactedAudit(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	c := model.AlertChannel{Name: "ops", Type: "telegram", BotToken: "123:private_token", ChatID: "-123"}
	if err := s.PutAlertChannel(t.Context(), c, "test"); err != nil {
		t.Fatal(err)
	}
	c.BotToken, c.ChatID = "", "-456"
	if err := s.PutAlertChannel(t.Context(), c, "test"); err != nil {
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
	channels, err := s.AlertChannels(t.Context())
	if err != nil || len(channels) != 1 || channels[0].BotToken != "123:private_token" || channels[0].ChatID != "-456" || channels[0].BatchWindow != 10 {
		t.Fatalf("channels = %+v, %v", channels, err)
	}
	var audit string
	if err := s.db.QueryRowContext(t.Context(), "SELECT group_concat(after) FROM audit").Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(audit, "private_token") || strings.Contains(audit, `"bot_token"`) {
		t.Fatal("credential leaked in audit")
	}
}

func TestDeleteChannelRemovesReferencesAtomically(t *testing.T) {
	s := channelFixture(t)
	var originals []model.Definition
	for _, kind := range []model.Kind{model.KindJob, model.KindWorker} {
		d, err := s.PutDefinition(t.Context(), model.Definition{Name: string(kind), Kind: kind, Command: "true", Alerts: []string{"ops"}}, 0, "test")
		if err != nil {
			t.Fatal(err)
		}
		originals = append(originals, d)
	}
	if err := s.DeleteAlertChannel(t.Context(), "ops", false, "test"); !errors.Is(err, ErrAlertChannelReferenced) {
		t.Fatalf("referenced delete = %v", err)
	}
	for _, d := range originals {
		got, _, err := s.Definition(t.Context(), d.Name)
		if err != nil || got.Revision != d.Revision || !slices.Equal(got.Alerts, d.Alerts) {
			t.Fatalf("failed delete changed definition: %+v, %v", got, err)
		}
	}
	if err := s.DeleteAlertChannel(t.Context(), "ops", true, "test"); err != nil {
		t.Fatal(err)
	}
	for _, d := range originals {
		got, hash, err := s.Definition(t.Context(), d.Name)
		if err != nil || got.Revision != d.Revision+1 || len(got.Alerts) != 0 {
			t.Fatalf("removed reference = %+v, %v", got, err)
		}
		var revisionHash string
		if err := s.db.QueryRowContext(t.Context(), "SELECT spec_hash FROM definition_revisions WHERE definition_id=? AND revision=?", got.ID, got.Revision).Scan(&revisionHash); err != nil || hash != revisionHash {
			t.Fatalf("revision hash = %s, %v", revisionHash, err)
		}
	}
	if err := s.DeleteAlertChannel(t.Context(), "ops", true, "test"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing delete = %v", err)
	}
}

func TestChannelDeleteRefusesConfigOwnedReferencesAndRollsBack(t *testing.T) {
	s := channelFixture(t)
	if _, err := s.PutDefinition(t.Context(), model.Definition{Name: "a-registry", Kind: model.KindJob, Command: "true", Alerts: []string{"ops"}}, 0, "test"); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncConfigDefinitions(t.Context(), []model.Definition{{Name: "z-config", Kind: model.KindJob, Command: "true", Alerts: []string{"ops"}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAlertChannel(t.Context(), "ops", true, "test"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("config-owned delete = %v", err)
	}
	d, _, err := s.Definition(t.Context(), "a-registry")
	if err != nil || d.Revision != 1 || len(d.Alerts) != 1 {
		t.Fatalf("partial reference removal: %+v, %v", d, err)
	}
	channels, err := s.AlertChannels(t.Context())
	if err != nil || len(channels) != 1 {
		t.Fatalf("channel removed on failure: %v", err)
	}
}

func TestDefinitionWritesCheckChannelsInTransaction(t *testing.T) {
	s := channelFixture(t)
	d := model.Definition{Name: "job", Kind: model.KindJob, Command: "true", Alerts: []string{"missing"}}
	if _, err := s.PutDefinition(t.Context(), d, 0, "test"); !errors.Is(err, ErrUnknownAlertChannel) {
		t.Fatalf("put = %v", err)
	}
	if _, err := s.CreateDefinition(t.Context(), d, "test"); !errors.Is(err, ErrUnknownAlertChannel) {
		t.Fatalf("create = %v", err)
	}
	if err := s.ImportDefinitions(t.Context(), []model.Definition{d}, "test"); !errors.Is(err, ErrUnknownAlertChannel) {
		t.Fatalf("import = %v", err)
	}
	if err := s.SyncConfigDefinitions(t.Context(), []model.Definition{d}); !errors.Is(err, ErrUnknownAlertChannel) {
		t.Fatalf("sync = %v", err)
	}
	d.Alerts = []string{"ops", "ops"}
	if _, err := s.PutDefinition(t.Context(), d, 0, "test"); !errors.Is(err, ErrUnknownAlertChannel) {
		t.Fatalf("duplicate = %v", err)
	}
}

func TestConcurrentChannelDeleteAndDefinitionSave(t *testing.T) {
	s := channelFixture(t)
	start := make(chan struct{})
	var wg sync.WaitGroup
	var putErr, deleteErr error
	wg.Go(func() {
		<-start
		_, putErr = s.PutDefinition(t.Context(), model.Definition{Name: "job", Kind: model.KindJob, Command: "true", Alerts: []string{"ops"}}, 0, "test")
	})
	wg.Go(func() { <-start; deleteErr = s.DeleteAlertChannel(t.Context(), "ops", true, "test") })
	close(start)
	wg.Wait()
	if deleteErr != nil {
		t.Fatal(deleteErr)
	}
	if putErr != nil && !errors.Is(putErr, ErrUnknownAlertChannel) {
		t.Fatal(putErr)
	}
	defs, err := s.Definitions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range defs {
		if len(d.Alerts) != 0 {
			t.Fatal("dangling reference after concurrent delete")
		}
	}
}

func TestAlertChannelBoundsAndValidation(t *testing.T) {
	s := channelFixture(t)
	for _, token := range []string{"env:TOKEN", "file:/tmp/token", "123:token/with/slash", "123:token\n"} {
		err := s.PutAlertChannel(t.Context(), model.AlertChannel{Name: "bad", Type: "telegram", ChatID: "123", BotToken: token}, "test")
		if !errors.Is(err, model.ErrInvalidAlertChannel) || strings.Contains(err.Error(), token) {
			t.Fatalf("unsafe validation error: %v", err)
		}
	}
	for i := 1; i < model.MaxAlertChannels; i++ {
		if err := s.PutAlertChannel(t.Context(), model.AlertChannel{Name: fmt.Sprintf("c%d", i), Type: "telegram", ChatID: "123", BotToken: "123:token"}, "test"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutAlertChannel(t.Context(), model.AlertChannel{Name: "overflow", Type: "telegram", ChatID: "123", BotToken: "123:token"}, "test"); !errors.Is(err, ErrAlertChannelLimit) {
		t.Fatalf("limit = %v", err)
	}
	if err := s.PutAlertChannel(t.Context(), model.AlertChannel{Name: "ops", Type: "telegram", ChatID: "456"}, "test"); err != nil {
		t.Fatal(err)
	}
}

func TestMigration11CreatesEmptyChannelRegistry(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "minicron.db"))
	if err != nil {
		t.Fatal(err)
	}
	old := strings.Replace(schema, alertChannelsDDL, "", 1)
	old = strings.Replace(old, "PRAGMA user_version=11;", "PRAGMA user_version=10;", 1)
	if _, err := db.Exec(old); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	channels, err := s.AlertChannels(t.Context())
	if err != nil || len(channels) != 0 {
		t.Fatalf("initial registry = %+v, %v", channels, err)
	}
}
