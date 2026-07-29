package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/synara-ai/synara/services/control-plane/internal/cocoonsupervisor"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	config, err := cocoonsupervisor.LoadConfig()
	if err != nil {
		logger.Error("invalid Cocoon supervisor configuration", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := cocoonsupervisor.NewDaemon(config, logger).Run(ctx); err != nil {
		logger.Error("Cocoon supervisor stopped", "error", err)
		os.Exit(1)
	}
}
