package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"time"
)

type Membership struct {
	Chat struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	NewMember struct {
		Status   string `json:"status"`
		IsMember bool   `json:"is_member"`
	} `json:"new_chat_member"`
}

type Update struct {
	ID         int64       `json:"update_id"`
	Membership *Membership `json:"my_chat_member"`
}

func (r *Relay) Updates(ctx context.Context, offset int64) ([]Update, error) {
	u, err := url.Parse(r.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid relay URL")
	}
	u.Path = path.Join(path.Dir(u.Path), "get-updates")
	u.RawQuery = ""
	data, _ := json.Marshal(struct {
		Token   string `json:"bot_token"`
		Offset  int64  `json:"offset"`
		Timeout int    `json:"timeout"`
	}{r.Token, offset, 15})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("invalid updates request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", r.Key)
	resp, err := r.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("updates relay request failed")
	}
	defer resp.Body.Close()
	var result struct {
		OK     bool     `json:"ok"`
		Result []Update `json:"result"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&result) != nil {
		return nil, fmt.Errorf("invalid updates relay response (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK || !result.OK {
		return nil, fmt.Errorf("updates relay rejected request (HTTP %d)", resp.StatusCode)
	}
	return result.Result, nil
}

type subscriptions struct {
	Version int            `json:"version"`
	Offset  int64          `json:"offset"`
	Groups  map[int64]bool `json:"groups"`
}

func openSubscriptions(file string) (*Store, *subscriptions, error) {
	store, err := openLockedStore(file)
	if err != nil {
		return nil, nil, err
	}
	state := &subscriptions{Version: 1, Groups: map[int64]bool{}}
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		err = store.Save(state)
	} else if err == nil {
		state = &subscriptions{}
		err = json.Unmarshal(data, state)
		if err == nil && (state.Version != 1 || state.Offset < 0 || state.Groups == nil) {
			err = fmt.Errorf("invalid subscriptions state")
		}
		if _, exists := state.Groups[0]; exists {
			err = fmt.Errorf("invalid subscription chat ID")
		}
	}
	if err != nil {
		store.Close()
		return nil, nil, fmt.Errorf("load subscriptions (file preserved): %w", err)
	}
	return store, state, nil
}

func (s *subscriptions) apply(updates []Update, manual map[int64]bool) {
	for _, update := range updates {
		if update.ID < s.Offset {
			continue
		}
		s.Offset = update.ID + 1
		m := update.Membership
		if m == nil || m.Chat.ID == 0 || manual[m.Chat.ID] || (m.Chat.Type != "group" && m.Chat.Type != "supergroup") {
			continue
		}
		switch m.NewMember.Status {
		case "member", "administrator", "creator":
			s.Groups[m.Chat.ID] = true
		case "restricted":
			s.Groups[m.Chat.ID] = m.NewMember.IsMember
		case "left", "kicked":
			s.Groups[m.Chat.ID] = false
		}
	}
}

// RunSubscriptions получает только изменения членства самого бота, без сообщений группы.
func RunSubscriptions(ctx context.Context, cfg Config) error {
	store, state, err := openSubscriptions(cfg.StateFile + ".subscriptions.json")
	if err != nil {
		return err
	}
	defer store.Close()
	manual := map[int64]bool{}
	for _, target := range cfg.Targets() {
		manual[target.ChatID] = true
	}
	type worker struct {
		cancel context.CancelFunc
		done   chan struct{}
	}
	workers := map[int64]worker{}
	failures := make(chan error, 1)
	defer func() {
		for _, w := range workers {
			w.cancel()
		}
		for _, w := range workers {
			<-w.done
		}
	}()
	reconcile := func() error {
		for chatID, w := range workers {
			if !state.Groups[chatID] || manual[chatID] {
				w.cancel()
				<-w.done
				delete(workers, chatID)
				slog.Info("group unsubscribed", "chat_id", chatID)
			}
		}
		for chatID, active := range state.Groups {
			if _, exists := workers[chatID]; exists || !active || manual[chatID] {
				continue
			}
			target := Target{ChatID: chatID}
			gameStore, games, err := OpenStore(TargetStateFile(cfg.StateFile, target), chatID)
			if err != nil {
				return err
			}
			workerCtx, cancel := context.WithCancel(ctx)
			done := make(chan struct{})
			workers[chatID] = worker{cancel: cancel, done: done}
			service := &Service{
				Source: NewWebSource(cfg.Timeout, cfg.Location),
				Sender: &Relay{Client: &http.Client{Timeout: cfg.Timeout}, URL: cfg.RelayURL, Key: cfg.RelayKey, Token: cfg.Token, ChatID: chatID},
				State:  games, Store: gameStore, Location: cfg.Location,
				Logger: slog.Default().With("chat_id", chatID, "message_thread_id", 0),
			}
			go func() {
				err := service.Run(workerCtx, cfg.CheckTimes)
				gameStore.Close()
				close(done)
				if err != nil {
					select {
					case failures <- err:
					case <-workerCtx.Done():
					}
				}
			}()
			service.Logger.Info("group subscribed")
		}
		return nil
	}
	if err := reconcile(); err != nil {
		return err
	}
	slog.Info("automatic group discovery started")
	relay := &Relay{Client: &http.Client{Timeout: 25 * time.Second}, URL: cfg.RelayURL, Key: cfg.RelayKey, Token: cfg.Token}
	for ctx.Err() == nil {
		select {
		case err := <-failures:
			return err
		default:
		}
		updates, err := relay.Updates(ctx, state.Offset)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			slog.Warn("group discovery failed; will retry", "error", err)
			timer := time.NewTimer(10 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case err := <-failures:
				timer.Stop()
				return err
			case <-timer.C:
			}
			continue
		}
		if len(updates) == 0 {
			continue
		}
		state.apply(updates, manual)
		// Сохраняем подписки и offset до подтверждения событий следующим getUpdates.
		if err := store.Save(state); err != nil {
			return err
		}
		if err := reconcile(); err != nil {
			return err
		}
	}
	return nil
}
