package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/synara-ai/synara/services/control-plane/internal/gvisorattestor"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	config, err := gvisorattestor.LoadConfig()
	if err != nil {
		logger.Error("invalid gVisor node attestor configuration", "error", err)
		os.Exit(1)
	}
	attestor, err := gvisorattestor.New(config, logger)
	if err != nil {
		logger.Error("initialize gVisor node attestor", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := attestor.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("gVisor node attestor stopped", "error", err)
		os.Exit(1)
	}
}
