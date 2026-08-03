package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/synara-ai/synara/services/control-plane/internal/agentd"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	controltracing "github.com/synara-ai/synara/services/control-plane/internal/tracing"
)

func main() {
	if handled, err := agentd.RunGVisorRuntimeVerifier(os.Args); handled {
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "gVisor runtime verification failed")
			os.Exit(1)
		}
		return
	}
	if handled, err := agentd.RunKubernetesNetworkBoundaryVerifier(os.Args); handled {
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "Kubernetes network boundary verification failed")
			os.Exit(1)
		}
		return
	}
	if handled, err := agentd.RunProviderCredentialScopeVerifier(os.Args); handled {
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "Provider credential scope verification failed")
			os.Exit(1)
		}
		return
	}
	if handled, err := agentd.RunKubernetesRegistrationTokenStager(os.Args); handled {
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "Kubernetes registration token staging failed")
			os.Exit(1)
		}
		return
	}
	if handled, err := agentd.RunGitAskPassHelperFromEnvironment(context.Background(), os.Args, os.Stdout); handled {
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "Git Credential helper failed")
			os.Exit(1)
		}
		return
	}
	if handled, err := agentd.RunProtectedCgroupCommand(context.Background(), os.Args, os.Stdout); handled {
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "Protected cgroup command failed")
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
	if err := agentd.LoadObservabilityEnvironment(cfg); err != nil {
		logger.Error("invalid agentd observability configuration", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	tracingPolicy := controltracing.ExportPolicyDevelopment
	if cfg.TargetKind != platform.TargetLocal {
		tracingPolicy = controltracing.ExportPolicyEnterpriseWorker
	}
	tracingShutdown, err := controltracing.Configure(ctx, "synara-agentd", logger, tracingPolicy)
	if err != nil {
		logger.Error("failed to configure OpenTelemetry tracing", "error", err)
		os.Exit(1)
	}
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tracingShutdown(shutdownContext); err != nil {
			logger.Warn("OpenTelemetry trace shutdown failed", "error", err)
		}
	}()
	if err := agentd.NewDaemon(cfg, logger).Run(ctx); err != nil {
		logger.Error("agentd stopped", "error", err)
		os.Exit(1)
	}
}
