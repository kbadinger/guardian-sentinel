// Package alert delivers alert text: to Telegram, or to stdout in dry-run.
package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// Environment variables the Telegram sender reads.
const (
	EnvBotToken = "TELEGRAM_BOT_TOKEN"
	EnvChatID   = "TELEGRAM_CHAT_ID"
)

// DefaultAPI is Telegram's Bot API base URL.
const DefaultAPI = "https://api.telegram.org"

// Telegram sends plain-text messages through the Bot API's sendMessage.
type Telegram struct {
	Token  string
	ChatID string
	API    string       // base URL; DefaultAPI when empty (tests point it at httptest)
	Client *http.Client // http.DefaultClient when nil; the timeout comes from ctx
}

// NewTelegramFromEnv reads the bot token and chat id from the environment.
// The error names any missing variable and never prints a value.
func NewTelegramFromEnv(getenv func(string) string) (*Telegram, error) {
	t := &Telegram{Token: getenv(EnvBotToken), ChatID: getenv(EnvChatID)}
	var missing []error
	if t.Token == "" {
		missing = append(missing, fmt.Errorf("environment variable %s is not set", EnvBotToken))
	}
	if t.ChatID == "" {
		missing = append(missing, fmt.Errorf("environment variable %s is not set", EnvChatID))
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("telegram: %w (or set SENTINEL_DRY_RUN=1 to print alerts instead)", errors.Join(missing...))
	}
	return t, nil
}

// Send posts text to the chat. Errors never include the bot token, which
// is part of the request URL.
func (t *Telegram) Send(ctx context.Context, text string) error {
	api := t.API
	if api == "" {
		api = DefaultAPI
	}
	client := t.Client
	if client == nil {
		client = http.DefaultClient
	}

	body, err := json.Marshal(map[string]string{"chat_id": t.ChatID, "text": text})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, api+"/bot"+t.Token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return errors.New("telegram: could not build request")
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		// *url.Error repeats the URL, token included. Keep only the cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("telegram: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Telegram explains failures in {"ok": false, "description": "..."}.
		var reply struct {
			Description string `json:"description"`
		}
		json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&reply)
		return fmt.Errorf("telegram: HTTP %d: %s", resp.StatusCode, reply.Description)
	}
	return nil
}
