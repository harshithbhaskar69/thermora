package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"thermora.local/thermora/internal/sim"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	address := flag.String("listen", "tcp://:1502", "private Modbus endpoint")
	state := flag.String("state", "/state/sim.db", "equipment checkpoint")
	rate := flag.Int("rate", 60, "live simulation speed: 1, 10 or 60")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	e, err := sim.Open(*state, 0)
	if err == nil {
		err = e.Run(ctx, *address, *rate)
		err = errors.Join(err, e.Close())
	}
	if err != nil {
		slog.Error("simulator stopped", "error", err)
		os.Exit(1)
	}
}
