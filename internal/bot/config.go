package bot

import (
	"encoding/json"
	"fmt"
	"io"
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
	AdditionalTargets                                []Target
	AutoGroups                                       bool
	Location                                         *time.Location
	CheckTimes                                       []string
	Timeout                                          time.Duration
}

type Target struct {
	ChatID          int64 `json:"chat_id"`
	MessageThreadID int64 `json:"message_thread_id,omitempty"`
}

func (c Config) Targets() []Target {
	var targets []Target
	if c.ChatID != 0 {
		targets = append(targets, Target{ChatID: c.ChatID, MessageThreadID: c.MessageThreadID})
	}
	return append(targets, c.AdditionalTargets...)
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
	if raw := strings.TrimSpace(os.Getenv("TELEGRAM_CHAT_ID")); raw != "" {
		c.ChatID, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || c.ChatID == 0 {
			return c, fmt.Errorf("TELEGRAM_CHAT_ID must be a nonzero integer when set")
		}
	}
	c.MessageThreadID, err = strconv.ParseInt(env("TELEGRAM_MESSAGE_THREAD_ID", "0"), 10, 64)
	if err != nil || c.MessageThreadID < 0 {
		return c, fmt.Errorf("TELEGRAM_MESSAGE_THREAD_ID must be a nonnegative integer")
	}
	if c.ChatID == 0 && c.MessageThreadID != 0 {
		return c, fmt.Errorf("TELEGRAM_MESSAGE_THREAD_ID requires TELEGRAM_CHAT_ID")
	}
	if raw := strings.TrimSpace(os.Getenv("TELEGRAM_ADDITIONAL_TARGETS")); raw != "" {
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&c.AdditionalTargets); err != nil {
			return c, fmt.Errorf("invalid TELEGRAM_ADDITIONAL_TARGETS: %w", err)
		}
		if decoder.Decode(new(any)) != io.EOF {
			return c, fmt.Errorf("invalid TELEGRAM_ADDITIONAL_TARGETS: expected one JSON array")
		}
	}
	seen := map[Target]bool{}
	for _, target := range c.Targets() {
		if target.ChatID == 0 || target.MessageThreadID < 0 {
			return c, fmt.Errorf("invalid Telegram target: nonzero chat_id and nonnegative message_thread_id required")
		}
		if seen[target] {
			return c, fmt.Errorf("duplicate Telegram target: chat_id=%d message_thread_id=%d", target.ChatID, target.MessageThreadID)
		}
		seen[target] = true
	}
	c.AutoGroups, err = strconv.ParseBool(env("TELEGRAM_AUTO_GROUPS", "true"))
	if err != nil {
		return c, fmt.Errorf("invalid TELEGRAM_AUTO_GROUPS")
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
