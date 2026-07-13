package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/synara-ai/synara/services/control-plane/internal/agentd"
)

func main() {
	if handled, err := agentd.RunGitAskPassHelperFromEnvironment(context.Background(), os.Args, os.Stdout); handled {
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "Git Credential helper failed")
			os.Exit(1)
		}
		return
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := agentd.LoadConfig()
	if err != nil {
		logger.Error("invalid agentd configuration", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := agentd.NewDaemon(cfg, logger).Run(ctx); err != nil {
		logger.Error("agentd stopped", "error", err)
		os.Exit(1)
	}
}
