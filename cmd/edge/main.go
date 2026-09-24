package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"thermora.local/thermora/internal/edge"
	"thermora.local/thermora/internal/local"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	path := flag.String("config", "/run/edge.json", "local edge configuration")
	state := flag.String("state", "/state/edge.db", "durable edge journal")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var cfg local.EdgeConfig
	err := local.Read(*path, &cfg)
	if err == nil {
		err = edge.Run(ctx, cfg, *state)
	}
	if err != nil && ctx.Err() == nil {
		slog.Error("edge stopped", "error", err)
		os.Exit(1)
	}
}
