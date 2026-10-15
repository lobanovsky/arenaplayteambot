package bot

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Token, RelayURL, RelayKey, StateFile, ListenAddr string
	ChatID                                           int64
	MessageThreadID                                  int64
	Location                                         *time.Location
	CheckTimes                                       []string
	Timeout                                          time.Duration
}

func LoadConfig() (Config, error) {
	c := Config{Token: os.Getenv("TELEGRAM_BOT_TOKEN"), RelayURL: os.Getenv("TELEGRAM_RELAY_URL"), RelayKey: os.Getenv("TELEGRAM_RELAY_API_KEY"), StateFile: env("STATE_FILE", "/data/state.json"), ListenAddr: env("LISTEN_ADDR", ":8080")}
	for k, v := range map[string]string{"TELEGRAM_BOT_TOKEN": c.Token, "TELEGRAM_RELAY_URL": c.RelayURL, "TELEGRAM_RELAY_API_KEY": c.RelayKey} {
		if strings.TrimSpace(v) == "" {
			return c, fmt.Errorf("required setting: %s", k)
		}
	}
	u, err := url.Parse(c.RelayURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return c, fmt.Errorf("invalid TELEGRAM_RELAY_URL")
	}
	c.ChatID, err = strconv.ParseInt(os.Getenv("TELEGRAM_CHAT_ID"), 10, 64)
	if err != nil || c.ChatID == 0 {
		return c, fmt.Errorf("TELEGRAM_CHAT_ID must be a nonzero integer")
	}
	c.MessageThreadID, err = strconv.ParseInt(env("TELEGRAM_MESSAGE_THREAD_ID", "0"), 10, 64)
	if err != nil || c.MessageThreadID < 0 {
		return c, fmt.Errorf("TELEGRAM_MESSAGE_THREAD_ID must be a nonnegative integer")
	}
	c.Location, err = time.LoadLocation(env("TZ", "Europe/Moscow"))
	if err != nil {
		return c, err
	}
	c.Timeout, err = time.ParseDuration(env("REQUEST_TIMEOUT", "25s"))
	if err != nil || c.Timeout <= 0 {
		return c, fmt.Errorf("invalid REQUEST_TIMEOUT")
	}
	for _, s := range strings.Split(env("CHECK_TIMES", "07:00,13:00,19:00"), ",") {
		s = strings.TrimSpace(s)
		if _, err := time.Parse("15:04", s); err != nil {
			return c, fmt.Errorf("invalid CHECK_TIMES")
		}
		c.CheckTimes = append(c.CheckTimes, s)
	}
	return c, nil
}
func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func NextCheck(now time.Time, loc *time.Location, slots []string) time.Time {
	local := now.In(loc)
	var best time.Time
	for day := 0; day <= 1; day++ {
		date := local.AddDate(0, 0, day)
		for _, slot := range slots {
			t, _ := time.Parse("15:04", slot)
			candidate := time.Date(date.Year(), date.Month(), date.Day(), t.Hour(), t.Minute(), 0, 0, loc)
			if candidate.After(now) && (best.IsZero() || candidate.Before(best)) {
				best = candidate
			}
		}
	}
	return best
}
