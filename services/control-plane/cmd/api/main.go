package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/synara-ai/synara/services/control-plane/internal/agentd"
	"github.com/synara-ai/synara/services/control-plane/internal/artifacts"
	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/config"
	"github.com/synara-ai/synara/services/control-plane/internal/credentials"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/enterpriseidentity"
	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/httpapi"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	credentialkms "github.com/synara-ai/synara/services/control-plane/internal/kms"
	"github.com/synara-ai/synara/services/control-plane/internal/observability"
	"github.com/synara-ai/synara/services/control-plane/internal/outbox"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/projects"
	"github.com/synara-ai/synara/services/control-plane/internal/quotas"
	"github.com/synara-ai/synara/services/control-plane/internal/retention"
	"github.com/synara-ai/synara/services/control-plane/internal/scim"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/internal/serviceaccounts"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
	"github.com/synara-ai/synara/services/control-plane/internal/tenancy"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func main() {
	if handled, err := agentd.RunGitAskPassHelperFromEnvironment(context.Background(), os.Args, os.Stdout); handled {
		if err != nil {
			_, _ = os.Stderr.WriteString("Git Credential helper failed\n")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := runHealthcheck(); err != nil {
			_, _ = os.Stderr.WriteString(err.Error() + "\n")
			os.Exit(1)
		}
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := config.Load()
	if err != nil {
		logger.Error("invalid control-plane configuration", "error", err)
		os.Exit(1)
	}
	if len(cfg.LocalAgentdRunnerCommand) > 0 && cfg.WorkerRegistrationToken == "" {
		registrationToken, _, err := secret.NewToken()
		if err != nil {
			logger.Error("failed to generate internal local agentd registration token", "error", err)
			os.Exit(1)
		}
		cfg.WorkerRegistrationToken = registrationToken
	}

	ctx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	runtimeContext, stopRuntime := context.WithCancel(context.Background())
	defer stopRuntime()
	databaseOptions := database.Options{
		MaxOpenConnections: cfg.DatabaseMaxOpenConnections, MaxIdleConnections: cfg.DatabaseMaxIdleConnections,
		ConnectionMaxLifetime: cfg.DatabaseConnectionMaxLifetime, ConnectionMaxIdleTime: cfg.DatabaseConnectionMaxIdleTime,
		MigrationLockTimeout: cfg.DatabaseMigrationLockTimeout,
	}
	metadataStore, err := database.OpenMetadataStore(ctx, cfg.Platform, cfg.DatabaseURL, cfg.SQLitePath, databaseOptions)
	if err != nil {
		logger.Error("failed to open metadata store", "kind", cfg.Platform.MetadataStore, "error", err)
		os.Exit(1)
	}
	defer func() { _ = metadataStore.Close() }()
	if err := metadataStore.Migrate(ctx, migrations.Files); err != nil {
		logger.Error("failed to apply metadata migrations", "kind", metadataStore.Kind(), "error", err)
		os.Exit(1)
	}
	db := metadataStore.DB()
	schemaChecker, err := database.NewSchemaChecker(db, metadataStore.Kind(), migrations.Files)
	if err != nil {
		logger.Error("failed to configure schema readiness", "kind", metadataStore.Kind(), "error", err)
		os.Exit(1)
	}
	metrics := observability.New(db, observability.Config{
		SessionIdleTTL: cfg.SessionIdleTTL, WorkerHeartbeatTimeout: cfg.WorkerHeartbeatTimeout,
	})
	if cfg.Platform.QueueDriver == platform.QueueExternal {
		logger.Error("external queue driver requires a publisher adapter that is not configured in this build")
		os.Exit(1)
	}
	outboxService, err := outbox.NewService(db, outbox.Config{
		BatchSize: cfg.OutboxBatchSize, ClaimTTL: cfg.OutboxClaimTTL,
		MaxAttempts: cfg.OutboxMaxAttempts, BaseBackoff: cfg.OutboxBaseBackoff,
		MaxBackoff: cfg.OutboxMaxBackoff,
	})
	if err != nil {
		logger.Error("failed to configure outbox service", "error", err)
		os.Exit(1)
	}
	outboxDispatcher, err := outbox.NewDispatcher(
		outboxService, outbox.DatabasePublisher{}, cfg.OutboxPollInterval, metrics, logger,
	)
	if err != nil {
		logger.Error("failed to configure outbox dispatcher", "error", err)
		os.Exit(1)
	}
	bootstrapped, err := bootstrap.Ensure(ctx, db, cfg.Platform.Profile, cfg.InstallationID)
	if err != nil {
		logger.Error("failed to bootstrap control-plane installation", "error", err)
		os.Exit(1)
	}

	identityOptions := make([]identity.PersonalDomain, 0, 1)
	if bootstrapped.Personal {
		identityOptions = append(identityOptions, identity.PersonalDomain{UserID: bootstrapped.UserID, TenantID: bootstrapped.TenantID})
	}
	identityService := identity.NewService(db, cfg.SessionTTL, cfg.SessionIdleTTL, identityOptions...)
	projectService := projects.NewService(db)
	cursorCipher, err := secret.NewCursorCipher(cfg.ProviderCursorKey)
	if err != nil {
		logger.Error("failed to configure provider cursor encryption", "error", err)
		os.Exit(1)
	}
	executionTargetService := executiontargets.NewService(db, cfg.Platform, cursorCipher)
	sshProvisioner := executiontargets.NewSSHProvisioner(executionTargetService, executiontargets.SSHProvisioningConfig{
		AgentdBinaryPath: cfg.AgentdBinaryPath, RegistrationToken: cfg.WorkerRegistrationToken,
		PublicControlPlaneURL: cfg.PublicControlPlaneURL, Timeout: cfg.SSHProvisionTimeout,
	})
	dockerReconciler := executiontargets.NewDockerPoolReconciler(executionTargetService, executiontargets.DockerPoolReconcilerConfig{
		RegistrationToken: cfg.WorkerRegistrationToken, PublicControlPlaneURL: cfg.PublicControlPlaneURL,
		Interval: cfg.DockerReconcileInterval, Observer: metrics,
	}, logger)
	sessionService := sessions.NewService(db, projectService, executionTargetService)
	executionService := executions.NewService(
		db, sessionService, cfg.WorkerLeaseTTL, cfg.WorkerHeartbeatTimeout,
		cfg.WorkerReceiptTTL, cursorCipher, executionTargetService,
		executions.WithProviderCursorMaximumAge(cfg.ProviderCursorMaximumAge),
	)
	tenancyService := tenancy.NewService(db, executionService)
	kubernetesReconciler := executiontargets.NewKubernetesReconciler(executionTargetService, executiontargets.KubernetesReconcilerConfig{
		RegistrationToken: cfg.WorkerRegistrationToken, PublicControlPlaneURL: cfg.PublicControlPlaneURL,
		Interval: cfg.KubernetesReconcileInterval, RecoverExpired: executionService.RecoverExpired,
		ReconcileEphemeralWorkspaceCleanup: executionService.ReconcileEphemeralWorkspaceCleanup,
		Observer:                           metrics,
	}, logger)
	artifactStore, err := artifacts.NewStore(ctx, cfg)
	if err != nil {
		logger.Error("failed to configure artifact store", "kind", cfg.Platform.ArtifactStore, "error", err)
		os.Exit(1)
	}
	artifactService := artifacts.NewService(db, artifactStore, cfg, executionService, sessionService, metrics)
	quotaService := quotas.NewService(db)
	credentialCipher, err := credentialkms.New(ctx, credentialkms.Config{
		Provider: cfg.CredentialKMSProvider, KeyID: cfg.CredentialKMSKeyID,
		LocalKey: cfg.CredentialKMSLocalKey, Region: cfg.CredentialKMSAWSRegion,
	})
	if err != nil {
		logger.Error("failed to configure provider credential KMS", "provider", cfg.CredentialKMSProvider, "error", err)
		os.Exit(1)
	}
	credentialService := credentials.NewService(db, credentialCipher)
	serviceAccountService := serviceaccounts.NewService(db)
	enterpriseIdentityService := enterpriseidentity.NewService(db, identityService, credentialCipher)
	scimService := scim.NewService(db)
	retentionService := retention.NewService(
		db, sessionService, artifactService, executionService, cfg.RetentionSweepInterval, logger, metrics,
	)
	api, err := httpapi.New(
		cfg, db, identityService, tenancyService, projectService, sessionService,
		executionService, executionTargetService, sshProvisioner, artifactService, quotaService,
		credentialService, retentionService, metrics, outboxService, enterpriseIdentityService,
		serviceAccountService, scimService, schemaChecker, logger,
	)
	if err != nil {
		logger.Error("failed to configure HTTP API", "error", err)
		os.Exit(1)
	}
	server := newControlPlaneHTTPServer(cfg.ListenAddress, api.Handler(), runtimeContext, stopRuntime)
	var localAgentd *agentd.LocalSupervisor
	if len(cfg.LocalAgentdRunnerCommand) > 0 {
		localTarget, _, resolveErr := executionTargetService.ResolveWorkerTarget(
			ctx, bootstrapped.ExecutionTargetID, string(platform.TargetLocal),
		)
		if resolveErr != nil {
			logger.Error("failed to resolve local agentd execution target", "error", resolveErr)
			os.Exit(1)
		}
		localAgentd, err = agentd.NewLocalSupervisor(agentd.LocalSupervisorInput{
			ListenAddress: cfg.ListenAddress, RegistrationToken: cfg.WorkerRegistrationToken,
			ExecutionTargetID: bootstrapped.ExecutionTargetID, RunnerCommand: cfg.LocalAgentdRunnerCommand,
			Capabilities:  localTarget.Capabilities,
			WorkspaceRoot: cfg.LocalAgentdWorkspaceRoot, GitCacheRoot: cfg.LocalAgentdGitCacheRoot,
			WorkerLeaseTTL:   cfg.WorkerLeaseTTL,
			HeartbeatTimeout: cfg.WorkerHeartbeatTimeout, DrainTimeout: cfg.ShutdownTimeout / 2,
			RestartBackoff: cfg.LocalAgentdRestartBackoff,
		}, logger)
		if err != nil {
			logger.Error("failed to configure local agentd supervisor", "error", err)
			os.Exit(1)
		}
	}

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("control plane listening", "address", cfg.ListenAddress)
		serverErrors <- server.ListenAndServe()
	}()
	var background sync.WaitGroup
	startBackground := func(run func()) {
		background.Add(1)
		go func() {
			defer background.Done()
			run()
		}()
	}
	startBackground(func() { dockerReconciler.Run(runtimeContext) })
	startBackground(func() { kubernetesReconciler.Run(runtimeContext) })
	startBackground(func() { retentionService.Run(runtimeContext) })
	startBackground(func() { outboxDispatcher.Run(runtimeContext) })
	if localAgentd != nil {
		logger.Info(
			"local agentd supervisor enabled", "executionTargetId", bootstrapped.ExecutionTargetID,
			"workspaceRoot", cfg.LocalAgentdWorkspaceRoot, "gitCacheRoot", cfg.LocalAgentdGitCacheRoot,
		)
		startBackground(func() {
			localAgentd.Run(runtimeContext)
		})
	}

	var serveErr error
	select {
	case <-ctx.Done():
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = err
			logger.Error("control plane stopped unexpectedly", "error", err)
		}
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	httpShutdownErr := server.Shutdown(shutdownContext)
	if httpShutdownErr != nil {
		logger.Error("control plane HTTP shutdown failed", "error", httpShutdownErr)
		if closeErr := server.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			logger.Error("control plane forced HTTP close failed", "error", closeErr)
		}
	}

	stopRuntime()
	backgroundDone := make(chan struct{})
	go func() {
		background.Wait()
		close(backgroundDone)
	}()
	select {
	case <-backgroundDone:
	case <-shutdownContext.Done():
		logger.Warn("control plane background shutdown did not finish before the deadline")
	}
	if serveErr != nil || httpShutdownErr != nil {
		os.Exit(1)
	}
}

func newControlPlaneHTTPServer(
	address string,
	handler http.Handler,
	runtimeContext context.Context,
	stopRuntime context.CancelFunc,
) *http.Server {
	server := &http.Server{
		Addr: address, Handler: handler,
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second,
		BaseContext: func(net.Listener) context.Context { return runtimeContext },
	}
	// Shutdown closes listeners before invoking this callback, so no new HTTP
	// request can arrive after request contexts and background loops start draining.
	server.RegisterOnShutdown(stopRuntime)
	return server
}

func runHealthcheck() error {
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get("http://127.0.0.1:3780/ready")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("control plane is not ready")
	}
	return nil
}
