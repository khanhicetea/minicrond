package store

import (
	"strings"
	"testing"

	"github.com/khanhicetea/minicrond/internal/model"
)

func TestAlertChannelMutationRollsBackOnAuditFailure(t *testing.T) {
	s := channelFixture(t)
	d, err := s.PutDefinition(t.Context(), model.Definition{Name: "job", Kind: model.KindJob, Command: "true", Alerts: []string{"ops"}}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	// Fail after the primary write, exercising transaction rollback rather than
	// merely validation. Only this test's isolated metadata database is changed.
	if _, err := s.db.ExecContext(t.Context(), "DROP TABLE audit"); err != nil {
		t.Fatal(err)
	}
	c := model.AlertChannel{Name: "ops", Type: "telegram", BotToken: "456:new_private_token", ChatID: "-456"}
	if err := s.PutAlertChannel(t.Context(), c, "test"); err == nil || strings.Contains(err.Error(), c.BotToken) {
		t.Fatalf("unsafe/absent save failure: %v", err)
	}
	if err := s.DeleteAlertChannel(t.Context(), "ops", true, "test"); err == nil {
		t.Fatal("expected reference-removal audit failure")
	}
	channels, err := s.AlertChannels(t.Context())
	if err != nil || len(channels) != 1 || channels[0].BotToken != "123:private_token" || channels[0].ChatID != "-123" {
		t.Fatal("failed mutation changed channel")
	}
	got, _, err := s.Definition(t.Context(), d.Name)
	if err != nil || got.Revision != d.Revision || len(got.Alerts) != 1 {
		t.Fatalf("partial reference removal: %+v, %v", got, err)
	}
	var revisions int
	if err := s.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM definition_revisions WHERE definition_id=?", d.ID).Scan(&revisions); err != nil || revisions != 1 {
		t.Fatalf("revision rollback = %d, %v", revisions, err)
	}
}
