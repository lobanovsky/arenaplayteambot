package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestUpdatesUseFreshConnection(t *testing.T) {
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/get-updates" || !r.Close {
			t.Error("poll must close its relay connection", r.URL, r.Close)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true,"result":[]}`)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	relay := &Relay{URL: server.URL + "/send-message", Client: server.Client(), Token: "token", Key: "key"}
	for i := 0; i < 3; i++ {
		if _, err := relay.Updates(context.Background(), 42); err != nil {
			t.Fatal(err)
		}
	}
	if connections.Load() != 3 {
		t.Fatal("polling reused a relay connection", connections.Load())
	}
}

func TestUpdatesNetworkErrorIsUsefulAndRedacted(t *testing.T) {
	relay := &Relay{URL: "https://relay.example/send-message", Token: "private-token", Key: "private-key", Client: &http.Client{}}
	relay.Client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("read: connection reset by peer private-token private-key")
	})
	_, err := relay.Updates(context.Background(), 42)
	if err == nil || !strings.Contains(err.Error(), "connection reset by peer") || strings.Contains(err.Error(), relay.Token) || strings.Contains(err.Error(), relay.Key) {
		t.Fatal("missing or unsafe network error", err)
	}
}

func TestAutomaticGroupJoinDeliveryAndRemoval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sent, secondSent, removed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var polls, sends atomic.Int32
	date := time.Now().In(moscow()).AddDate(0, 0, 2).Format("02/01/2006")
	previous := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := ""
		switch r.URL.String() {
		case "https://relay.example/get-updates":
			switch polls.Add(1) {
			case 1:
				body = `{"ok":true,"result":[{"update_id":10,"my_chat_member":{"chat":{"id":-100456,"type":"supergroup"},"new_chat_member":{"status":"member"}}}]}`
			case 2:
				select {
				case <-sent:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
				body = `{"ok":true,"result":[{"update_id":11,"my_chat_member":{"chat":{"id":-100456,"type":"supergroup"},"new_chat_member":{"status":"kicked"}}}]}`
			case 3:
				body = `{"ok":true,"result":[{"update_id":12,"my_chat_member":{"chat":{"id":-100456,"type":"supergroup"},"new_chat_member":{"status":"member"}}}]}`
			case 4:
				select {
				case <-secondSent:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
				body = `{"ok":true,"result":[{"update_id":13,"my_chat_member":{"chat":{"id":-100456,"type":"supergroup"},"new_chat_member":{"status":"left"}}}]}`
			default:
				close(removed)
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
		case "https://relay.example/send-message":
			var message map[string]any
			if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
				return nil, err
			}
			if message["chat_id"] != float64(-100456) || message["message_thread_id"] != nil {
				return nil, fmt.Errorf("wrong destination: %v", message)
			}
			switch sends.Add(1) {
			case 1:
				close(sent)
			case 2:
				close(secondSent)
			}
			body = `{"ok":true}`
		case ClubURL:
			body = table(row("123", date, "21:45", "Arena Play", "Легенда"))
		case TeamScheduleURL:
			body = `{}`
		case VideoCatalog, "https://vkvideo.ru/videos-71330880":
			body = `<script type="application/ld+json">{"@type":"VideoObject","name":"Другие команды","datePublished":"2026-10-15","contentUrl":"https://vkvideo.ru/video-71330880_1"}</script>`
		default:
			return nil, fmt.Errorf("unexpected URL: %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	defer func() { http.DefaultTransport = previous }()
	cfg := Config{Token: "token", RelayURL: "https://relay.example/send-message", RelayKey: "key", StateFile: filepath.Join(t.TempDir(), "state.json"), Timeout: time.Second, Location: moscow(), CheckTimes: []string{"07:00"}}
	done := make(chan error, 1)
	go func() { done <- RunSubscriptions(ctx, cfg) }()
	select {
	case <-removed:
	case err := <-done:
		t.Fatalf("auto groups stopped early: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("automatic group did not send and unsubscribe")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if sends.Load() != 2 {
		t.Fatal("wrong message count", sends.Load())
	}
	store, state, err := openSubscriptions(cfg.StateFile + ".subscriptions.json")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if state.Groups[-100456] || state.Offset != 14 {
		t.Fatal(state)
	}
	gameStore, games, err := OpenStore(TargetStateFile(cfg.StateFile, Target{ChatID: -100456}), -100456)
	if err != nil {
		t.Fatal("worker did not release state lock", err)
	}
	defer gameStore.Close()
	if games.Games["lhl:123"] == nil || games.Games["lhl:123"].Sent == nil {
		t.Fatal("delivery state not preserved")
	}
}

func TestMembershipSubscriptionsPersist(t *testing.T) {
	file := filepath.Join(t.TempDir(), "subscriptions.json")
	store, state, err := openSubscriptions(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	apply := func(id int64, chat int64, kind, status string, member bool) {
		t.Helper()
		m := &Membership{}
		m.Chat.ID, m.Chat.Type = chat, kind
		m.NewMember.Status, m.NewMember.IsMember = status, member
		state.apply([]Update{{ID: id, Membership: m}}, map[int64]bool{-100123: true})
	}
	apply(1, -100456, "supergroup", "member", false)
	apply(2, -100123, "supergroup", "administrator", false)
	apply(3, 123, "private", "member", false)
	apply(4, -100789, "channel", "administrator", false)
	if !state.Groups[-100456] || len(state.Groups) != 1 || state.Offset != 5 {
		t.Fatal(state)
	}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, state, err = openSubscriptions(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if !state.Groups[-100456] || state.Offset != 5 {
		t.Fatal("subscriptions lost after restart")
	}
	apply(5, -100456, "supergroup", "kicked", false)
	apply(4, -100456, "supergroup", "member", false)
	if state.Groups[-100456] {
		t.Fatal("removed group resubscribed by an old update")
	}
	apply(6, -100456, "supergroup", "member", false)
	if !state.Groups[-100456] {
		t.Fatal("re-add did not subscribe")
	}
	apply(7, -100456, "supergroup", "restricted", true)
	if !state.Groups[-100456] {
		t.Fatal("restricted member is still in group")
	}
	apply(8, -100456, "supergroup", "restricted", false)
	if state.Groups[-100456] {
		t.Fatal("nonmember subscribed")
	}
	apply(9, -100456, "supergroup", "administrator", false)
	apply(10, -100456, "supergroup", "left", false)
	if state.Groups[-100456] {
		t.Fatal("left group still subscribed")
	}
	if other, _, err := openSubscriptions(file); err == nil {
		other.Close()
		t.Fatal("second subscriber reader allowed")
	}
}

func TestSubscriptionsRejectBrokenState(t *testing.T) {
	for _, body := range []string{`{`, `{}`, `{"version":1,"groups":null}`, `{"version":1,"groups":{"0":true}}`, `{"version":1,"offset":-1,"groups":{}}`} {
		file := filepath.Join(t.TempDir(), "subscriptions.json")
		if err := os.WriteFile(file, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if store, _, err := openSubscriptions(file); err == nil {
			store.Close()
			t.Fatalf("accepted %s", body)
		}
		data, _ := os.ReadFile(file)
		if string(data) != body {
			t.Fatal("broken subscriptions overwritten")
		}
	}
}

func TestRelayMembershipUpdates(t *testing.T) {
	relay := &Relay{URL: "https://relay.example/api/send-message", Key: "key", Token: "token", Client: &http.Client{Timeout: time.Second}}
	relay.Client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://relay.example/api/get-updates" || r.Method != http.MethodPost || r.Header.Get("X-Api-Key") != "key" {
			t.Fatal("wrong relay request", r.URL)
		}
		var body struct {
			Token   string `json:"bot_token"`
			Offset  int64  `json:"offset"`
			Timeout int    `json:"timeout"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Token != "token" || body.Offset != 41 || body.Timeout != 15 {
			t.Fatal(body)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":[{"update_id":42,"my_chat_member":{"chat":{"id":-100456,"type":"supergroup"},"new_chat_member":{"status":"member"}}}]}`))}, nil
	})
	updates, err := relay.Updates(context.Background(), 41)
	if err != nil || len(updates) != 1 || updates[0].Membership.Chat.ID != -100456 {
		t.Fatal(updates, err)
	}
	for _, body := range []string{`{"ok":false,"description":"token key"}`, `not json`} {
		relay.Client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 502, Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		if _, err := relay.Updates(context.Background(), 41); err == nil || strings.Contains(err.Error(), "token") || strings.Contains(err.Error(), "key") {
			t.Fatal("unsafe or missing relay error", err)
		}
	}
}
