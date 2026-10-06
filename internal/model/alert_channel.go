package model

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// AlertChannel is a registry entry. BotToken is a literal credential, never a
// secret reference. Public responses must use Redacted, including audit data.
type AlertChannel struct {
	Name                string `json:"name"`
	Type                string `json:"type"`
	BotToken            string `json:"bot_token,omitempty"`
	HasBotToken         bool   `json:"has_bot_token,omitempty"`
	ChatID              string `json:"chat_id"`
	DisableNotification bool   `json:"disable_notification"`
	BatchWindow         int    `json:"batch_window"`
}

const MaxAlertChannels = 100

var ErrInvalidAlertChannel = errors.New("invalid alert channel")

var alertNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,99}$`)

func (c AlertChannel) Redacted() AlertChannel {
	c.HasBotToken = c.BotToken != ""
	c.BotToken = ""
	return c
}

// Normalize validates bounded settings without including credentials in errors.
func (c *AlertChannel) Normalize() error {
	if !alertNamePattern.MatchString(c.Name) {
		return fmt.Errorf("%w: invalid name", ErrInvalidAlertChannel)
	}
	if c.Type != "telegram" {
		return fmt.Errorf("%w: unsupported type", ErrInvalidAlertChannel)
	}
	if c.BatchWindow == 0 {
		c.BatchWindow = 10
	}
	if c.BatchWindow < 1 || c.BatchWindow > 3600 {
		return fmt.Errorf("%w: batch_window must be between 1 and 3600 seconds", ErrInvalidAlertChannel)
	}
	if c.ChatID == "" || len(c.ChatID) > 256 || strings.ContainsAny(c.ChatID, "\r\n") {
		return fmt.Errorf("%w: chat_id is required and must be at most 256 characters", ErrInvalidAlertChannel)
	}
	// Telegram credentials use digits, a colon and an opaque URL-safe suffix.
	// Reject secret references and URL/control characters, without echoing tokens.
	id, secret, ok := strings.Cut(c.BotToken, ":")
	if !ok || id == "" || secret == "" || len(c.BotToken) > 256 {
		return fmt.Errorf("%w: a literal Telegram bot_token is required", ErrInvalidAlertChannel)
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return fmt.Errorf("%w: invalid Telegram bot_token", ErrInvalidAlertChannel)
		}
	}
	for _, r := range secret {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
			return fmt.Errorf("%w: invalid Telegram bot_token", ErrInvalidAlertChannel)
		}
	}
	c.HasBotToken = false
	return nil
}
