package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type Sender interface {
	Send(context.Context, string) error
}
type Relay struct {
	Client          *http.Client
	URL, Key, Token string
	ChatID          int64
	MessageThreadID int64
}

func (r *Relay) Send(ctx context.Context, text string) error {
	data, err := json.Marshal(struct {
		Token           string `json:"bot_token"`
		ChatID          int64  `json:"chat_id"`
		Text            string `json:"text"`
		MessageThreadID int64  `json:"message_thread_id,omitempty"`
	}{r.Token, r.ChatID, text, r.MessageThreadID})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("invalid relay request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", r.Key)
	resp, err := r.Client.Do(req)
	if err != nil {
		return fmt.Errorf("relay request failed")
	}
	defer resp.Body.Close()
	var result struct {
		OK          bool   `json:"ok"`
		Code        int    `json:"error_code"`
		Description string `json:"description"`
		Error       string `json:"error"`
		Parameters  struct {
			MigrateToChatID int64 `json:"migrate_to_chat_id"`
		} `json:"parameters"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return fmt.Errorf("invalid relay response (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !result.OK {
		description := result.Description
		if description == "" {
			description = result.Error
		}
		if description == "" {
			description = "no error description"
		}
		for _, secret := range []string{r.Token, r.Key} {
			if secret != "" {
				description = strings.ReplaceAll(description, secret, "[REDACTED]")
			}
		}
		description = strings.Join(strings.Fields(description), " ")
		if len([]rune(description)) > 512 {
			description = string([]rune(description)[:512]) + "…"
		}
		if result.Parameters.MigrateToChatID != 0 {
			description += fmt.Sprintf("; migrate_to_chat_id=%d", result.Parameters.MigrateToChatID)
		}
		return fmt.Errorf("relay rejected message (HTTP %d, Telegram code %d): %s", resp.StatusCode, result.Code, description)
	}
	return nil
}
