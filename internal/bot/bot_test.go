package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func moscow() *time.Location { l, _ := time.LoadLocation("Europe/Moscow"); return l }
func moment(s string) time.Time {
	v, err := time.ParseInLocation("2006-01-02 15:04", s, moscow())
	if err != nil {
		panic(err)
	}
	return v
}
func table(rows string) string {
	return `<h4>Ближайшие игры</h4><table><tr><th>Тур</th><th>Круг</th><th>Дивизион</th><th>Число</th><th>Время</th><th>Каток</th><th>Команда А</th><th>Счет</th><th>Команда Б</th></tr>` + rows + `</table>`
}
func row(id, date, hour, a, b string) string {
	return fmt.Sprintf(`<tr><td>1</td><td>1</td><td>Дебютант</td><td>%s</td><td>%s</td><td>Арена<br>2</td><td>%s</td><td><a href="/ngame_sheet/%s/">0:0</a></td><td>%s</td>`, date, hour, a, id, b)
}
func TestParseGames(t *testing.T) {
	games, err := ParseGames(table(row("123", "17/10/2026", "21:45", "Arena Play", "Легенда")+row("124", "24/10/2026", "09:45", "Grizzly team", "Arena Play"))+table(row("123", "17/10/2026", "21:45", "Arena Play", "Легенда")), moscow())
	if err != nil || len(games) != 2 {
		t.Fatalf("%+v %v", games, err)
	}
	if games[0].ID != "lhl:123" || games[0].Arena != "Арена 2" || games[1].Opponent != "Grizzly team" {
		t.Fatalf("%+v", games)
	}
	moved, err := ParseGames(table(row("123", "18/10/2026", "20:00", "Arena Play", "Легенда")), moscow())
	if err != nil || moved[0].ID != games[0].ID {
		t.Fatal("unstable match ID")
	}
	if g, e := ParseGames(table(""), moscow()); e != nil || len(g) != 0 {
		t.Fatal(g, e)
	}
	if _, e := ParseGames("<html>maintenance</html>", moscow()); e == nil {
		t.Fatal("accepted invalid page")
	}
}
func TestVideos(t *testing.T) {
	page := `<script type="application/ld+json">{"@type":"VideoGallery","video":[{"@type":"VideoObject","name":"Arena Play - Легенда","contentUrl":"https://vkvideo.ru/live-71330880_123","publication":{"startDate":"2026-10-17T21:45:00+03:00"}}]}</script>`
	videos, err := ParseVideos(page)
	g := Game{Opponent: "Легенда", Starts: moment("2026-10-17 21:45")}
	if err != nil || FindVideo(g, videos) != "https://vkvideo.ru/live-71330880_123" {
		t.Fatal(videos, err)
	}
	old := videos[0]
	old.Date = old.Date.AddDate(0, 0, -10)
	if FindVideo(g, []Video{old}) != "" {
		t.Fatal("matched old game")
	}
	other := videos[0]
	other.URL = "https://vkvideo.ru/video-71330880_456"
	if FindVideo(g, append(videos, other)) != "" {
		t.Fatal("matched ambiguous game")
	}
	if FindVideo(g, append(videos, videos...)) == "" {
		t.Fatal("duplicate catalog URLs became ambiguous")
	}
}

type fakeSource struct {
	games             []Game
	videos            []Video
	gameErr, videoErr error
}

func (f *fakeSource) Games(context.Context) ([]Game, error)   { return f.games, f.gameErr }
func (f *fakeSource) Videos(context.Context) ([]Video, error) { return f.videos, f.videoErr }

type fakeSender struct {
	messages []string
	err      error
	calls    int
}

func (f *fakeSender) Send(_ context.Context, s string) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	f.messages = append(f.messages, s)
	return nil
}
func setup(t *testing.T) (*Service, *fakeSource, *fakeSender) {
	t.Helper()
	store, state, err := OpenStore(filepath.Join(t.TempDir(), "data/state.json"), -100123)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	source := &fakeSource{}
	sender := &fakeSender{}
	return &Service{Source: source, Sender: sender, Store: store, State: state, Location: moscow()}, source, sender
}
func check(t *testing.T, s *Service, now time.Time) {
	t.Helper()
	if err := s.Check(context.Background(), now); err != nil {
		t.Fatal(err)
	}
}
func sampleGame() Game {
	return Game{ID: "lhl:123", Opponent: "Легенда", Starts: moment("2026-10-17 21:45"), Arena: "Арена"}
}
func TestLifecycle(t *testing.T) {
	s, source, sender := setup(t)
	g := sampleGame()
	old := g
	old.ID = "old"
	old.Starts = moment("2026-10-10 18:00")
	source.games = []Game{g, old}
	now := moment("2026-10-15 19:00")
	check(t, s, now)
	if len(sender.messages) != 1 || !strings.Contains(sender.messages[0], "Новая игра") {
		t.Fatal(sender.messages)
	}
	check(t, s, now)
	if len(sender.messages) != 1 {
		t.Fatal("duplicate send")
	}
	s.Store.Close()
	store, state, err := OpenStore(s.Store.Path, -100123)
	if err != nil {
		t.Fatal(err)
	}
	s.Store = store
	s.State = state
	t.Cleanup(store.Close)
	check(t, s, now)
	if len(sender.messages) != 1 {
		t.Fatal("duplicate after restart")
	}
	source.games[0].Starts = moment("2026-10-18 20:00")
	source.games[0].Arena = "Новая арена"
	check(t, s, now)
	if len(sender.messages) != 2 || !strings.Contains(sender.messages[1], "Ранее:") {
		t.Fatal(sender.messages)
	}
	source.videos = []Video{{Title: "Легенда vs Arena Play", Date: time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC), URL: "https://vkvideo.ru/live-71330880_123"}}
	check(t, s, now)
	if len(sender.messages) != 3 || !strings.Contains(sender.messages[2], "Появилась трансляция") {
		t.Fatal(sender.messages)
	}
	check(t, s, now)
	if len(sender.messages) != 3 {
		t.Fatal("duplicate video")
	}
	source.gameErr = errors.New("offline")
	check(t, s, now)
	if len(s.State.Games) != 1 {
		t.Fatal("erased state")
	}
}
func TestRetryAndRevalidation(t *testing.T) {
	s, source, sender := setup(t)
	source.games = []Game{sampleGame()}
	sender.err = errors.New("offline")
	now := moment("2026-10-15 19:00")
	check(t, s, now)
	r := s.State.Games["lhl:123"]
	if r.Sent != nil || !r.RetryAt.Equal(now.Add(time.Minute)) {
		t.Fatal(r)
	}
	check(t, s, now.Add(30*time.Second))
	if sender.calls != 1 {
		t.Fatal("retried too early")
	}
	source.games = nil
	sender.err = nil
	check(t, s, now.Add(time.Minute))
	if sender.calls != 1 {
		t.Fatal("sent vanished game")
	}
	source.games = []Game{sampleGame()}
	source.games[0].Arena = "Изменённая арена"
	check(t, s, now.Add(2*time.Minute))
	if len(sender.messages) != 1 || !strings.Contains(sender.messages[0], "Изменённая арена") {
		t.Fatal(sender.messages)
	}
}
func TestVKFailureDoesNotBlockGames(t *testing.T) {
	s, source, sender := setup(t)
	source.games = []Game{sampleGame()}
	source.videoErr = errors.New("offline")
	check(t, s, moment("2026-10-15 19:00"))
	if len(sender.messages) != 1 {
		t.Fatal(sender.messages)
	}
}
func TestNoStaleVideoAfterDeadline(t *testing.T) {
	s, source, sender := setup(t)
	source.games = []Game{sampleGame()}
	check(t, s, moment("2026-10-15 19:00"))
	source.videos = []Video{{Title: "Arena Play Легенда", URL: "https://vkvideo.ru/live-71330880_123", Date: time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)}}
	check(t, s, moment("2026-10-19 07:00"))
	if len(sender.messages) != 1 {
		t.Fatal(sender.messages)
	}
}
func TestStoreSafety(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, state, err := OpenStore(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if other, _, err := OpenStore(path, 1); err == nil {
		other.Close()
		t.Fatal("two writers")
	}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	store.Close()
	if other, _, err := OpenStore(path, 2); err == nil {
		other.Close()
		t.Fatal("accepted changed chat")
	}
	bad := []byte(`{"version":1,"games":`)
	if err := os.WriteFile(path, bad, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := OpenStore(path, 1); err == nil {
		t.Fatal("accepted corrupt state")
	}
	data, _ := os.ReadFile(path)
	if string(data) != string(bad) {
		t.Fatal("overwrote corrupt state")
	}
}
func TestNextCheck(t *testing.T) {
	for _, tc := range []struct{ now, want string }{{"2026-10-15 06:59", "2026-10-15 07:00"}, {"2026-10-15 07:00", "2026-10-15 13:00"}, {"2026-10-15 19:00", "2026-10-16 07:00"}} {
		got := NextCheck(moment(tc.now).UTC(), moscow(), []string{"07:00", "13:00", "19:00"})
		if !got.Equal(moment(tc.want)) {
			t.Fatal(got, tc)
		}
	}
}
func TestRelay(t *testing.T) {
	for _, tc := range []struct {
		status  int
		body    string
		success bool
	}{{200, `{"ok":true}`, true}, {403, `{"ok":false,"error_code":403}`, false}, {200, `{"ok":false}`, false}, {500, `{"ok":true}`, false}, {200, `oops`, false}} {
		t.Run(fmt.Sprint(tc.status, tc.body), func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method != "POST" || r.URL.Path != "/send-message" || r.Header.Get("X-Api-Key") != "key" {
					t.Error("bad request")
				}
				var body map[string]any
				json.NewDecoder(r.Body).Decode(&body)
				if body["bot_token"] != "token" || body["chat_id"] != float64(-100123) || body["text"] != "hello" {
					t.Error(body)
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
			})}
			relay := &Relay{Client: client, URL: "https://relay.example/send-message", Token: "token", Key: "key", ChatID: -100123}
			if err := relay.Send(context.Background(), "hello"); (err == nil) != tc.success {
				t.Fatal(err)
			}
		})
	}
}
func TestFatalSaveError(t *testing.T) {
	s, source, sender := setup(t)
	source.games = []Game{sampleGame()}
	s.Store.Path = filepath.Join(t.TempDir(), "missing", "state.json")
	if err := s.Check(context.Background(), moment("2026-10-15 19:00")); err == nil || sender.calls != 0 {
		t.Fatal("sent without saving state", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRejectIncompleteState(t *testing.T) {
	for _, body := range []string{`{}`, `null`, `{"version":1,"chat_id":1}`, `{"version":1,"chat_id":1,"games":{"bad":null}}`} {
		t.Run(body, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			os.WriteFile(path, []byte(body), 0600)
			store, _, err := OpenStore(path, 1)
			if err == nil {
				store.Close()
				t.Fatal("accepted incomplete state")
			}
		})
	}
}
func TestRetryBackoffSurvivesRestart(t *testing.T) {
	s, source, sender := setup(t)
	source.games = []Game{sampleGame()}
	sender.err = errors.New("offline")
	now := moment("2026-10-15 19:00")
	for _, delay := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute} {
		check(t, s, now)
		r := s.State.Games["lhl:123"]
		if !r.RetryAt.Equal(now.Add(delay)) {
			t.Fatal(r.RetryAt, delay)
		}
		now = r.RetryAt
	}
	s.Store.Close()
	store, state, err := OpenStore(s.Store.Path, -100123)
	if err != nil {
		t.Fatal(err)
	}
	s.Store = store
	s.State = state
	t.Cleanup(store.Close)
	calls := sender.calls
	check(t, s, now.Add(-time.Second))
	if sender.calls != calls {
		t.Fatal("restart lost retry deadline")
	}
	sender.err = nil
	check(t, s, now)
	if len(sender.messages) != 1 {
		t.Fatal(sender.messages)
	}
}
func TestFallbackIDUpgrade(t *testing.T) {
	s, source, sender := setup(t)
	g := sampleGame()
	g.ID = "date:2026-10-17|легенда"
	source.games = []Game{g}
	now := moment("2026-10-15 19:00")
	check(t, s, now)
	source.games[0].ID = "lhl:123"
	check(t, s, now)
	if len(sender.messages) != 1 || len(s.State.Games) != 1 || s.State.Games["lhl:123"] == nil {
		t.Fatal("ID upgrade resent game")
	}
}
func TestRescheduleClearsOldVideo(t *testing.T) {
	s, source, sender := setup(t)
	source.games = []Game{sampleGame()}
	source.videos = []Video{{Title: "Arena Play Легенда", URL: "https://vkvideo.ru/live-71330880_123", Date: time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)}}
	now := moment("2026-10-15 19:00")
	check(t, s, now)
	source.games[0].Starts = moment("2026-10-24 20:00")
	source.videoErr = errors.New("offline")
	check(t, s, now)
	if len(sender.messages) != 2 || strings.Contains(sender.messages[1], "vkvideo.ru/live") {
		t.Fatal(sender.messages)
	}
}
func TestSourceFetch(t *testing.T) {
	source := NewWebSource(time.Second, moscow())
	source.Client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("User-Agent") == "" {
			t.Error("no user agent")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(table(row("123", "17/10/2026", "21:45", "Arena Play", "Легенда"))))}, nil
	})
	games, err := source.Games(context.Background())
	if err != nil || len(games) != 1 || games[0].Opponent != "Легенда" {
		t.Fatal(games, err)
	}
	source.Client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("unavailable"))}, nil
	})
	if _, err := source.Games(context.Background()); err == nil {
		t.Fatal("accepted HTTP 503")
	}
	source.Client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("captcha"))}, nil
	})
	if _, err := source.Videos(context.Background()); err == nil {
		t.Fatal("unreadable catalog reported success")
	}
}
func TestConfigValidation(t *testing.T) {
	for k, v := range map[string]string{"TELEGRAM_BOT_TOKEN": "token", "TELEGRAM_RELAY_URL": "https://relay.example/send-message", "TELEGRAM_RELAY_API_KEY": "key", "TELEGRAM_CHAT_ID": "-100123", "TZ": "Europe/Moscow", "CHECK_TIMES": "07:00,13:00,19:00", "REQUEST_TIMEOUT": "25s"} {
		t.Setenv(k, v)
	}
	c, err := LoadConfig()
	if err != nil || c.ChatID != -100123 || len(c.CheckTimes) != 3 {
		t.Fatal(c, err)
	}
	for _, tc := range []struct{ key, value string }{{"TELEGRAM_CHAT_ID", "0"}, {"TELEGRAM_RELAY_URL", "ftp://relay"}, {"REQUEST_TIMEOUT", "-1s"}, {"CHECK_TIMES", "24:01"}, {"TZ", "bad/timezone"}, {"TELEGRAM_BOT_TOKEN", ""}} {
		t.Run(tc.key, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if _, err := LoadConfig(); err == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
}
