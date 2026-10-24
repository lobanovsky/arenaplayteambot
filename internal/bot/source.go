package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

const ClubURL = "https://lhl-77.ru/clubs/686"
const VideoCatalog = "https://vkvideo.ru/@lhl_77/all"
const RutubeCatalogAPI = "https://rutube.ru/api/video/person/35268475/"
const TeamScheduleURL = "https://arenaplayteam.ru/data/schedule.json"

type Game struct {
	ID         string    `json:"id"`
	Opponent   string    `json:"opponent"`
	Starts     time.Time `json:"starts"`
	Arena      string    `json:"arena"`
	Tournament string    `json:"tournament"`
	VideoURL   string    `json:"video_url,omitempty"`
}
type Video struct {
	Title, URL string
	Date       time.Time
	ExactDate  bool
}
type Source interface {
	Games(context.Context) ([]Game, error)
	Videos(context.Context) ([]Video, error)
}
type WebSource struct {
	Client   *http.Client
	Location *time.Location
}

func NewWebSource(timeout time.Duration, loc *time.Location) *WebSource {
	jar, _ := cookiejar.New(nil)
	return &WebSource{Client: &http.Client{Timeout: timeout, Jar: jar}, Location: loc}
}
func (s *WebSource) fetch(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)")
	resp, err := s.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("source request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("source HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20+1))
	if err != nil {
		return "", err
	}
	if len(raw) > 16<<20 {
		return "", fmt.Errorf("source response too large")
	}
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(url, "vkvideo.ru") && !strings.Contains(strings.ToLower(contentType), "charset=") {
		contentType = "text/html; charset=windows-1251"
	}
	reader, err := charset.NewReader(strings.NewReader(string(raw)), contentType)
	if err != nil {
		return "", err
	}
	decoded, err := io.ReadAll(reader)
	return string(decoded), err
}
func (s *WebSource) Games(ctx context.Context) ([]Game, error) {
	page, err := s.fetch(ctx, ClubURL)
	if err != nil {
		return nil, err
	}
	return ParseGames(page, s.Location)
}
func (s *WebSource) Videos(ctx context.Context) ([]Video, error) {
	var out []Video
	var last error
	// Сайт сохраняет найденные ссылки, даже если VK больше не показывает их в каталоге.
	if page, err := s.fetch(ctx, TeamScheduleURL); err == nil {
		if videos, err := ParseTeamVideos(page); err == nil {
			out = append(out, videos...)
		} else {
			last = err
		}
	} else {
		last = err
	}
	// Rutube отдаёт метаданные анонсов через публичный JSON-каталог.
	for page := 1; page <= 3; page++ {
		body, err := s.fetch(ctx, fmt.Sprintf("%s?page=%d", RutubeCatalogAPI, page))
		if err != nil {
			last = err
			break
		}
		videos, hasNext, err := ParseRutubeVideos(body)
		if err != nil {
			last = err
			break
		}
		out = append(out, videos...)
		if !hasNext {
			break
		}
	}
	for _, url := range []string{VideoCatalog, "https://vkvideo.ru/videos-71330880"} {
		page, err := s.fetch(ctx, url)
		if err != nil {
			last = err
			continue
		}
		v, err := ParseVideos(page)
		if err != nil {
			last = err
			continue
		}
		out = append(out, v...)
	}
	if len(out) == 0 {
		if last == nil {
			last = fmt.Errorf("video catalogs contain no readable videos")
		}
		return nil, last
	}
	return out, nil
}

func ParseTeamVideos(page string) ([]Video, error) {
	type entry struct {
		Opponent string `json:"opponent"`
		Date     string `json:"dateISO"`
		URL      string `json:"videoUrl"`
	}
	var schedule struct {
		Next    entry   `json:"nextGame"`
		Results []entry `json:"results"`
	}
	if err := json.Unmarshal([]byte(page), &schedule); err != nil {
		return nil, fmt.Errorf("invalid team schedule: %w", err)
	}
	var videos []Video
	for _, game := range append(schedule.Results, schedule.Next) {
		date, err := time.Parse("2006-01-02", game.Date)
		if err != nil || strings.TrimSpace(game.Opponent) == "" || !videoURLRE.MatchString(game.URL) {
			continue
		}
		videos = append(videos, Video{Title: "Arena Play " + game.Opponent, URL: game.URL, Date: date, ExactDate: true})
	}
	return videos, nil
}
func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		if n.Data == "br" {
			b.WriteByte(' ')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}
func attr(n *html.Node, k string) string {
	for _, a := range n.Attr {
		if a.Key == k {
			return a.Val
		}
	}
	return ""
}
func norm(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(strings.ToLower(s), "ё", "е")), " ")
}

var headingRE = regexp.MustCompile(`^h[3-6]$`)
var protocolRE = regexp.MustCompile(`/ngame_sheet/(\d+)/?`)

func ParseGames(page string, loc *time.Location) ([]Game, error) {
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		return nil, err
	}
	heading := ""
	validTables := 0
	games := map[string]Game{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && headingRE.MatchString(n.Data) {
			heading = nodeText(n)
		}
		if n.Type == html.ElementNode && n.Data == "table" {
			var rows []*html.Node
			var collect func(*html.Node)
			collect = func(x *html.Node) {
				if x != n && x.Data == "table" {
					return
				}
				if x.Data == "tr" {
					rows = append(rows, x)
				}
				for c := x.FirstChild; c != nil; c = c.NextSibling {
					collect(c)
				}
			}
			collect(n)
			cols := map[string]int{}
			recognized := false
			for _, row := range rows {
				var cells []*html.Node
				for c := row.FirstChild; c != nil; c = c.NextSibling {
					if c.Data == "td" || c.Data == "th" {
						cells = append(cells, c)
					}
				}
				if !recognized {
					for i, c := range cells {
						label := norm(nodeText(c))
						switch {
						case strings.Contains(label, "команда а"):
							cols["a"] = i
						case strings.Contains(label, "команда б"):
							cols["b"] = i
						case label == "число" || label == "дата":
							cols["date"] = i
						case strings.Contains(label, "время"):
							cols["time"] = i
						case strings.Contains(label, "каток") || strings.Contains(label, "арена"):
							cols["arena"] = i
						case strings.Contains(label, "дивизион") || strings.Contains(label, "турнир"):
							cols["division"] = i
						case label == "тур":
							cols["round"] = i
						case label == "круг":
							cols["circle"] = i
						}
					}
					recognized = true
					for _, key := range []string{"a", "b", "date", "time", "arena"} {
						if _, ok := cols[key]; !ok {
							recognized = false
						}
					}
					if recognized {
						validTables++
					}
					continue
				}
				get := func(key string) string {
					idx, ok := cols[key]
					if !ok || idx >= len(cells) {
						return ""
					}
					return nodeText(cells[idx])
				}
				a, b := get("a"), get("b")
				if (norm(a) == "arena play") == (norm(b) == "arena play") {
					continue
				}
				opponent := a
				if norm(a) == "arena play" {
					opponent = b
				}
				var starts time.Time
				for _, layout := range []string{"02/01/2006 15:04", "02.01.2006 15:04", "02-01-2006 15:04"} {
					starts, err = time.ParseInLocation(layout, get("date")+" "+get("time"), loc)
					if err == nil {
						break
					}
				}
				if err != nil {
					continue
				}
				if opponent == "" {
					continue
				}
				tournament := get("division")
				if tournament == "" {
					tournament = strings.TrimSpace(strings.ReplaceAll(heading, "(смотреть)", ""))
				}
				id := ""
				var links func(*html.Node)
				links = func(x *html.Node) {
					if m := protocolRE.FindStringSubmatch(attr(x, "href")); m != nil {
						id = "lhl:" + m[1]
					}
					for c := x.FirstChild; c != nil; c = c.NextSibling {
						links(c)
					}
				}
				links(row)
				if id == "" {
					if tournament != "" && get("round") != "" && get("circle") != "" {
						id = "round:" + norm(tournament) + "|" + get("round") + "|" + get("circle") + "|" + norm(opponent)
					} else {
						id = "date:" + starts.Format("2006-01-02") + "|" + norm(opponent)
					}
				}
				g := Game{ID: id, Opponent: opponent, Starts: starts, Arena: get("arena"), Tournament: tournament}
				duplicate := false
				for key, old := range games {
					if norm(old.Opponent) == norm(g.Opponent) && old.Starts.Equal(g.Starts) {
						duplicate = true
						if strings.HasPrefix(g.ID, "lhl:") && !strings.HasPrefix(old.ID, "lhl:") {
							delete(games, key)
							games[id] = g
						}
						break
					}
				}
				if !duplicate {
					games[id] = g
				}
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if validTables == 0 {
		return nil, fmt.Errorf("LHL schedule tables not found")
	}
	out := make([]Game, 0, len(games))
	for _, g := range games {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Starts.Equal(out[j].Starts) {
			return out[i].ID < out[j].ID
		}
		return out[i].Starts.Before(out[j].Starts)
	})
	return out, nil
}

var videoURLRE = regexp.MustCompile(`^https://(?:vkvideo\.ru/(?:video|live)-71330880_\d+|rutube\.ru/video/[a-f0-9]{32}/)$`)

// ParseRutubeVideos читает только публичные видео канала ЛХЛ-77.
func ParseRutubeVideos(page string) ([]Video, bool, error) {
	var catalog struct {
		HasNext bool `json:"has_next"`
		Results []struct {
			Title     string `json:"title"`
			URL       string `json:"video_url"`
			Published string `json:"publication_ts"`
			Created   string `json:"created_ts"`
			Deleted   bool   `json:"is_deleted"`
			Hidden    bool   `json:"is_hidden"`
			Author    struct {
				ID int `json:"id"`
			} `json:"author"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(page), &catalog); err != nil {
		return nil, false, fmt.Errorf("invalid Rutube catalog: %w", err)
	}
	var videos []Video
	for _, item := range catalog.Results {
		if item.Author.ID != 35268475 || item.Deleted || item.Hidden || !strings.HasPrefix(item.URL, "https://rutube.ru/video/") || !videoURLRE.MatchString(item.URL) {
			continue
		}
		published := item.Published
		if published == "" {
			published = item.Created
		}
		if len(published) < 10 {
			continue
		}
		day, err := time.Parse("2006-01-02", published[:10])
		if err == nil {
			videos = append(videos, Video{Title: item.Title, URL: item.URL, Date: day})
		}
	}
	return videos, catalog.HasNext, nil
}

func ParseVideos(page string) ([]Video, error) {
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		return nil, err
	}
	var out []Video
	var collect func(any)
	collect = func(v any) {
		switch x := v.(type) {
		case []any:
			for _, child := range x {
				collect(child)
			}
		case map[string]any:
			if x["@type"] == "VideoObject" {
				str := func(k string) string { s, _ := x[k].(string); return s }
				url := str("url")
				if !videoURLRE.MatchString(url) {
					url = str("contentUrl")
				}
				date := ""
				if pub, ok := x["publication"].(map[string]any); ok {
					date, _ = pub["startDate"].(string)
				}
				for _, k := range []string{"startDate", "datePublished", "uploadDate"} {
					if date == "" {
						date = str(k)
					}
				}
				if len(date) >= 10 && videoURLRE.MatchString(url) {
					d, e := time.Parse("2006-01-02", date[:10])
					if e == nil {
						out = append(out, Video{Title: html.UnescapeString(str("name")), URL: url, Date: d})
					}
				}
			}
			for _, child := range x {
				collect(child)
			}
		}
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Data == "script" && attr(n, "type") == "application/ld+json" && n.FirstChild != nil {
			var value any
			if json.Unmarshal([]byte(n.FirstChild.Data), &value) == nil {
				collect(value)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return out, nil
}
func titleKey(s string) string {
	var words []string
	for _, word := range strings.FieldsFunc(norm(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if word != "vs" {
			words = append(words, word)
		}
	}
	return strings.Join(words, " ")
}
func FindVideo(g Game, videos []Video) string {
	opponent := titleKey(g.Opponent)
	day, _ := time.Parse("2006-01-02", g.Starts.Format("2006-01-02"))
	best := 100
	urls := map[string]bool{}
	for _, v := range videos {
		k := titleKey(v.Title)
		if k != "arena play "+opponent && k != opponent+" arena play" {
			continue
		}
		delta := int(v.Date.Sub(day).Hours() / 24)
		if v.ExactDate && delta != 0 {
			continue
		}
		if delta < -3 || delta > 1 {
			continue
		}
		distance := delta
		if distance < 0 {
			distance = -distance
		}
		if distance < best {
			best = distance
			urls = map[string]bool{v.URL: true}
		} else if distance == best {
			urls[v.URL] = true
		}
	}
	// Зеркала одного матча на разных платформах не создают неоднозначность.
	rutube := map[string]bool{}
	for url := range urls {
		if strings.HasPrefix(url, "https://rutube.ru/video/") {
			rutube[url] = true
		}
	}
	if len(rutube) > 0 {
		urls = rutube
	}
	if len(urls) == 1 {
		for u := range urls {
			return u
		}
	}
	return ""
}
