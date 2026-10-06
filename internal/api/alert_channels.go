package api

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/store"
)

type alertChannelInput struct {
	Type                string `json:"type" enum:"telegram"`
	BotToken            string `json:"bot_token,omitempty" writeOnly:"true" maxLength:"256" doc:"Literal Telegram credential, required when creating. Empty or omitted retains the saved credential on edit."`
	ChatID              string `json:"chat_id" minLength:"1" maxLength:"256"`
	DisableNotification bool   `json:"disable_notification,omitempty"`
	BatchWindow         int    `json:"batch_window,omitempty" minimum:"0" maximum:"3600" default:"10" doc:"Batch window in seconds; zero or omitted selects 10."`
}

func (s *Server) putAlertChannel(w http.ResponseWriter, r *http.Request) {
	var in alertChannelInput
	if err := decodeJSON(r.Body, &in); err != nil {
		// Decoder errors can quote malformed input; never echo credentials.
		writeError(w, 422, "validation_failed", "invalid alert channel JSON")
		return
	}
	channel := model.AlertChannel{Name: r.PathValue("name"), Type: in.Type, BotToken: in.BotToken, ChatID: in.ChatID, DisableNotification: in.DisableNotification, BatchWindow: in.BatchWindow}
	check := channel
	if check.BotToken == "" && s.alertChannels != nil {
		for _, existing := range s.alertChannels() {
			if existing.Name == channel.Name {
				check.BotToken = existing.BotToken
				break
			}
		}
	}
	if err := check.Normalize(); err != nil {
		writeError(w, 422, "validation_failed", err.Error())
		return
	}
	channel.BatchWindow = check.BatchWindow
	if s.saveAlertChannel == nil {
		writeError(w, 503, "unavailable", "alert registry is unavailable")
		return
	}
	if err := s.saveAlertChannel(r.Context(), channel); err != nil {
		if errors.Is(err, model.ErrInvalidAlertChannel) {
			writeError(w, 422, "validation_failed", err.Error())
			return
		}
		if errors.Is(err, store.ErrAlertChannelLimit) {
			writeError(w, 409, "channel_limit", err.Error())
			return
		}
		internal(w, r, err)
		return
	}
	// Do not return the submitted credential, even to its authenticated owner.
	writeJSON(w, 200, check.Redacted())
}

func (s *Server) removeAlertChannel(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("remove_from_definitions")
	if raw != "" && raw != "true" && raw != "false" {
		writeError(w, 422, "validation_failed", "remove_from_definitions must be true or false")
		return
	}
	if s.deleteAlertChannel == nil {
		writeError(w, 503, "unavailable", "alert registry is unavailable")
		return
	}
	if err := s.deleteAlertChannel(r.Context(), r.PathValue("name"), raw == "true"); err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			writeError(w, 404, "not_found", "alert channel not found")
		case errors.Is(err, store.ErrAlertChannelReferenced):
			writeError(w, 409, "channel_referenced", err.Error())
		case errors.Is(err, store.ErrReadOnly):
			writeError(w, 409, "read_only", err.Error())
		default:
			internal(w, r, err)
		}
		return
	}
	writeJSON(w, 200, map[string]bool{"deleted": true})
}
