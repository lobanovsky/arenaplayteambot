package bot

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
)

type Service struct {
	Source   Source
	Sender   Sender
	State    *State
	Store    *Store
	Location *time.Location
	Logger   *slog.Logger
}

func (s *Service) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

func sameSchedule(a, b Game) bool {
	return a.Opponent == b.Opponent && a.Starts.Equal(b.Starts) && a.Arena == b.Arena
}
func textFor(r *Record, loc *time.Location) string {
	g := r.Current
	header := "🏒 Новая игра Arena Play"
	if r.Sent != nil {
		if !sameSchedule(*r.Sent, g) {
			header = "🏒 Изменилось расписание игры"
		} else {
			header = "📺 Ссылка на трансляцию матча"
		}
	}
	body := fmt.Sprintf("%s\n\nArena Play — %s\nКогда: %s МСК\nГде: %s", header, g.Opponent, g.Starts.In(loc).Format("02.01.2006 в 15:04"), g.Arena)
	if r.Sent != nil && !sameSchedule(*r.Sent, g) {
		body += fmt.Sprintf("\n\nРанее: %s МСК, %s", r.Sent.Starts.In(loc).Format("02.01.2006 в 15:04"), r.Sent.Arena)
	}
	if g.VideoURL != "" {
		body += "\n\nТрансляция: " + g.VideoURL
	}
	return body + "\n\nРасписание: " + ClubURL
}
func pending(r *Record, now time.Time, loc *time.Location) bool {
	if r.Sent == nil {
		return r.Current.Starts.After(now)
	}
	if r.Current.Starts.After(now) && !sameSchedule(*r.Sent, r.Current) {
		return true
	}
	return r.Current.VideoURL != "" && r.Current.VideoURL != r.Sent.VideoURL && now.Before(videoDeadline(r.Current, loc))
}
func videoDeadline(g Game, loc *time.Location) time.Time {
	d := g.Starts.In(loc)
	return time.Date(d.Year(), d.Month(), d.Day()+2, 0, 0, 0, 0, loc)
}

func (s *Service) Check(ctx context.Context, now time.Time) error {
	games, err := s.Source.Games(ctx)
	if err != nil {
		s.logger().Error("schedule check failed; preserving state", "error", err)
		return nil
	}
	videos, videoErr := s.Source.Videos(ctx)
	if videoErr != nil {
		s.logger().Warn("video sources unavailable; keeping saved links", "error", videoErr)
	}
	seen := map[string]bool{}
	for _, g := range games {
		seen[g.ID] = true
		r := s.State.Games[g.ID]
		if r == nil {
			// Привязываем старый запасной ключ к появившемуся протоколу только при точном совпадении.
			var match string
			for id, old := range s.State.Games {
				if (!strings.HasPrefix(id, "lhl:") || !strings.HasPrefix(g.ID, "lhl:")) && norm(old.Current.Opponent) == norm(g.Opponent) && old.Current.Starts.Equal(g.Starts) {
					if match != "" {
						match = ""
						break
					}
					match = id
				}
			}
			if match != "" {
				r = s.State.Games[match]
				delete(s.State.Games, match)
				r.Current.ID = g.ID
				if r.Sent != nil {
					r.Sent.ID = g.ID
				}
			}
		}
		if r == nil {
			if !g.Starts.After(now) {
				continue
			}
			r = &Record{}
			s.State.Games[g.ID] = r
		} else {
			s.State.Games[g.ID] = r
		}
		if !sameSchedule(r.Current, g) {
			r.Attempts = 0
			r.RetryAt = time.Time{}
		}
		g.VideoURL = r.Current.VideoURL
		if !r.Current.Starts.IsZero() && r.Current.Starts.In(s.Location).Format("2006-01-02") != g.Starts.In(s.Location).Format("2006-01-02") {
			g.VideoURL = ""
		}
		if now.Before(videoDeadline(g, s.Location)) && videoErr == nil {
			if url := FindVideo(g, videos); url != "" {
				g.VideoURL = url
			}
		}
		r.Current = g
	}
	// Для временно исчезнувшего матча не отправляем неподтверждённые изменения.
	if err = s.Store.Save(s.State); err != nil {
		return fmt.Errorf("save observed state: %w", err)
	}
	ids := make([]string, 0, len(s.State.Games))
	for id := range s.State.Games {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := s.State.Games[ids[i]].Current, s.State.Games[ids[j]].Current
		if a.Starts.Equal(b.Starts) {
			return ids[i] < ids[j]
		}
		return a.Starts.Before(b.Starts)
	})
	sent := 0
	for _, id := range ids {
		r := s.State.Games[id]
		if !seen[id] || !pending(r, now, s.Location) || r.RetryAt.After(now) {
			continue
		}
		if err = s.Sender.Send(ctx, textFor(r, s.Location)); err != nil {
			r.Attempts++
			delay := time.Minute
			for i := 1; i < r.Attempts && delay < 15*time.Minute; i++ {
				delay *= 2
			}
			if delay > 15*time.Minute {
				delay = 15 * time.Minute
			}
			r.RetryAt = now.Add(delay)
			s.logger().Error("notification failed", "game", id, "retry_at", r.RetryAt, "error", err)
		} else {
			snapshot := r.Current
			r.Sent = &snapshot
			r.Attempts = 0
			r.RetryAt = time.Time{}
			sent++
		}
		if err = s.Store.Save(s.State); err != nil {
			return fmt.Errorf("save delivery state: %w", err)
		}
	}
	s.logger().Info("check complete", "games", len(games), "messages", sent)
	return nil
}
func (s *Service) Run(ctx context.Context, slots []string) error {
	next := time.Now()
	for {
		now := time.Now()
		if err := s.Check(ctx, now); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		if !next.After(now) {
			next = NextCheck(time.Now(), s.Location, slots)
		}
		wake := next
		for _, r := range s.State.Games {
			if pending(r, now, s.Location) && !r.RetryAt.IsZero() {
				retry := r.RetryAt
				if retry.Before(time.Now()) {
					retry = time.Now().Add(time.Minute)
				}
				if retry.Before(wake) {
					wake = retry
				}
			}
		}
		timer := time.NewTimer(time.Until(wake))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
