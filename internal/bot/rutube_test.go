package bot

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const linkorRutubeURL = "https://rutube.ru/video/49ced94394f1c3e9d90af63c5099d7fc/"
const linkorRutubeJSON = `{"has_next":false,"results":[{"author":{"id":35268475},"title":"Arena Play - Линкор","video_url":"https://rutube.ru/video/49ced94394f1c3e9d90af63c5099d7fc/","publication_ts":"2026-10-22T23:02:30"}]}`

func TestRutubeMatchAndNotification(t *testing.T) {
	videos, next, err := ParseRutubeVideos(linkorRutubeJSON)
	g := Game{ID: "lhl:linkor", Opponent: "Линкор", Starts: moment("2026-10-24 19:45")}
	if err != nil || next || FindVideo(g, videos) != linkorRutubeURL {
		t.Fatal(videos, next, err)
	}
	vk := videos[0]
	vk.URL = "https://vkvideo.ru/live-71330880_456246354"
	if FindVideo(g, append(videos, vk)) != linkorRutubeURL {
		t.Fatal("mirrors became ambiguous")
	}
	duplicate := videos[0]
	duplicate.URL = "https://rutube.ru/video/11111111111111111111111111111111/"
	if FindVideo(g, append(videos, duplicate)) != "" {
		t.Fatal("ambiguous Rutube videos accepted")
	}
	s, source, sender := setup(t)
	source.games = []Game{g}
	now := moment("2026-10-24 13:00")
	check(t, s, now)
	source.videos = videos
	check(t, s, now)
	check(t, s, now)
	if len(sender.messages) != 2 || !strings.Contains(sender.messages[1], linkorRutubeURL) || !strings.Contains(sender.messages[1], "Ссылка на трансляцию матча") {
		t.Fatal(sender.messages)
	}
}

func TestRutubeValidation(t *testing.T) {
	for _, replacement := range []struct{ old, new string }{
		{`35268475`, `1`}, {`"title":`, `"is_deleted":true,"title":`},
		{`"title":`, `"is_hidden":true,"title":`},
		{linkorRutubeURL, "https://example.com/video/"},
		{`2026-10-22T23:02:30`, `invalid`},
	} {
		videos, _, err := ParseRutubeVideos(strings.Replace(linkorRutubeJSON, replacement.old, replacement.new, 1))
		if err != nil || len(videos) != 0 {
			t.Fatal(videos, err)
		}
	}
	if _, _, err := ParseRutubeVideos("<html>Unavailable</html>"); err == nil {
		t.Fatal("invalid response accepted")
	}
}

func TestTeamScheduleRutube(t *testing.T) {
	videos, err := ParseTeamVideos(fmt.Sprintf(`{"nextGame":{"dateISO":"2026-10-24","opponent":"Линкор","videoUrl":%q}}`, linkorRutubeURL))
	g := Game{Opponent: "Линкор", Starts: moment("2026-10-24 19:45")}
	if err != nil || FindVideo(g, videos) != linkorRutubeURL {
		t.Fatal(videos, err)
	}
	g.Starts = g.Starts.AddDate(0, 0, 1)
	if FindVideo(g, videos) != "" {
		t.Fatal("site link matched a different date")
	}
}

func TestWebSourceRutubePaginationAndVKFallback(t *testing.T) {
	for _, rutubeAvailable := range []bool{true, false} {
		t.Run(fmt.Sprint(rutubeAvailable), func(t *testing.T) {
			web := NewWebSource(time.Second, moscow())
			pages := 0
			web.Client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body := `{}`
				switch {
				case r.URL.Host == "rutube.ru":
					pages++
					if !rutubeAvailable {
						return nil, fmt.Errorf("Rutube unavailable")
					}
					body = `{"has_next":true,"results":[]}`
					if r.URL.Query().Get("page") == "2" {
						body = linkorRutubeJSON
					}
				case r.URL.Host == "vkvideo.ru":
					body = `<script type="application/ld+json">{"@type":"VideoObject","name":"Arena Play - Линкор","datePublished":"2026-10-22","contentUrl":"https://vkvideo.ru/live-71330880_456246354"}</script>`
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			videos, err := web.Videos(context.Background())
			want := "https://vkvideo.ru/live-71330880_456246354"
			wantPages := 1
			if rutubeAvailable {
				want = linkorRutubeURL
				wantPages = 2
			}
			got := FindVideo(Game{Opponent: "Линкор", Starts: moment("2026-10-24 19:45")}, videos)
			if err != nil || got != want || pages != wantPages {
				t.Fatal(got, pages, err)
			}
		})
	}
}
