package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type Sender interface {
	Send(context.Context, string) error
}
type Relay struct {
	Client          *http.Client
	URL, Key, Token string
	ChatID          int64
}

func (r *Relay) Send(ctx context.Context, text string) error {
	data, err := json.Marshal(struct {
		Token  string `json:"bot_token"`
		ChatID int64  `json:"chat_id"`
		Text   string `json:"text"`
	}{r.Token, r.ChatID, text})
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
		OK   bool `json:"ok"`
		Code int  `json:"error_code"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return fmt.Errorf("invalid relay response (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !result.OK {
		return fmt.Errorf("relay rejected message (HTTP %d, Telegram code %d)", resp.StatusCode, result.Code)
	}
	return nil
}
