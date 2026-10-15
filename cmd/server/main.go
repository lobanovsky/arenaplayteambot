package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"arenaplayteambot/internal/bot"
)

func main() {
	if err := run(); err != nil {
		slog.Error("service stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	cfg, err := bot.LoadConfig()
	if err != nil {
		return err
	}
	store, state, err := bot.OpenStore(cfg.StateFile, cfg.ChatID)
	if err != nil {
		return err
	}
	defer store.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	server := &http.Server{Addr: cfg.ListenAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	results := make(chan error, 2)
	go func() {
		err := server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		results <- err
	}()
	service := &bot.Service{Source: bot.NewWebSource(cfg.Timeout, cfg.Location), Sender: &bot.Relay{Client: &http.Client{Timeout: cfg.Timeout}, URL: cfg.RelayURL, Key: cfg.RelayKey, Token: cfg.Token, ChatID: cfg.ChatID}, State: state, Store: store, Location: cfg.Location}
	go func() { results <- service.Run(ctx, cfg.CheckTimes) }()
	slog.Info("started", "check_times", cfg.CheckTimes, "timezone", cfg.Location.String())
	var result error
	completed := false
	select {
	case result = <-results:
		completed = true
	case <-ctx.Done():
	}
	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server.Shutdown(shutdown)
	// Дожидаемся завершения рабочего цикла перед освобождением файловой блокировки.
	remaining := 2
	if completed {
		remaining--
	}
	for i := 0; i < remaining; i++ {
		if err := <-results; result == nil {
			result = err
		}
	}
	return result
}
