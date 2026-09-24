package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"thermora.local/thermora/internal/local"
	"thermora.local/thermora/internal/manufacturer"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx); err != nil {
		slog.Error("manufacturer stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) (err error) {
	path := flag.String("config", "/run/manufacturer.json", "local fixture configuration")
	flag.Parse()
	var cfg manufacturer.MockConfig
	if err := local.Read(*path, &cfg); err != nil {
		return err
	}
	mock, err := manufacturer.NewMock(cfg)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, mock.Close()) }()
	work, stopWork := context.WithCancel(ctx)
	defer stopWork()
	if err = mock.StartEquipment(work); err != nil {
		return err
	}
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); mock.RunEquipment(work) }()
	defer func() { stopWork(); <-workerDone }()
	server := &http.Server{
		Addr:              ":8444",
		Handler:           mock.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 * 1024,
	}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServeTLS(cfg.Certificate, cfg.Key) }()
	select {
	case <-ctx.Done():
	case err = <-done:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stopErr := server.Shutdown(shutdown)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return errors.Join(err, stopErr)
}
