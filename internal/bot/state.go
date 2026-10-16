package bot

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type Record struct {
	Current  Game      `json:"current"`
	Sent     *Game     `json:"sent,omitempty"`
	Attempts int       `json:"attempts,omitempty"`
	RetryAt  time.Time `json:"retry_at,omitempty"`
}
type State struct {
	Version int                `json:"version"`
	ChatID  int64              `json:"chat_id"`
	Games   map[string]*Record `json:"games"`
}
type Store struct {
	Path string
	lock *os.File
}

func TargetStateFile(base string, target Target) string {
	ext := filepath.Ext(base)
	return fmt.Sprintf("%s.chat-%d.thread-%d%s", strings.TrimSuffix(base, ext), target.ChatID, target.MessageThreadID, ext)
}

func OpenStore(path string, chatID int64) (*Store, *State, error) {
	store, err := openLockedStore(path)
	if err != nil {
		return nil, nil, err
	}
	state := &State{Version: 1, ChatID: chatID, Games: map[string]*Record{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		err = store.Save(state)
	} else if err == nil {
		state = &State{}
		err = json.Unmarshal(data, state)
		if err == nil && (state.Version != 1 || state.ChatID != chatID || state.Games == nil) {
			err = fmt.Errorf("unsupported state version or different TELEGRAM_CHAT_ID")
		}
		if err == nil {
			for id, r := range state.Games {
				if r == nil || r.Current.ID != id || r.Current.Starts.IsZero() || r.Current.Opponent == "" {
					err = fmt.Errorf("invalid game record in state")
					break
				}
			}
		}
	}
	if err != nil {
		store.Close()
		return nil, nil, fmt.Errorf("load state (file preserved): %w", err)
	}
	return store, state, nil
}

func openLockedStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("state already in use: %w", err)
	}
	return &Store{Path: path, lock: lock}, nil
}
func (s *Store) Save(state any) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.Path), ".state-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, s.Path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(s.Path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (s *Store) Close() {
	if s.lock != nil {
		syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		s.lock.Close()
		s.lock = nil
	}
}
