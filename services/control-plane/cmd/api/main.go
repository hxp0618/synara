package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/agentd"
	"github.com/synara-ai/synara/services/control-plane/internal/artifacts"
	"github.com/synara-ai/synara/services/control-plane/internal/billing"
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
	"github.com/synara-ai/synara/services/control-plane/internal/leadership"
	"github.com/synara-ai/synara/services/control-plane/internal/lifecyclepolicy"
	"github.com/synara-ai/synara/services/control-plane/internal/memories"
	"github.com/synara-ai/synara/services/control-plane/internal/metricrollup"
	"github.com/synara-ai/synara/services/control-plane/internal/observability"
	"github.com/synara-ai/synara/services/control-plane/internal/outbox"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/poolautoscaling"
	"github.com/synara-ai/synara/services/control-plane/internal/projects"
	"github.com/synara-ai/synara/services/control-plane/internal/quotas"
	"github.com/synara-ai/synara/services/control-plane/internal/reconcilerleadership"
	"github.com/synara-ai/synara/services/control-plane/internal/retention"
	"github.com/synara-ai/synara/services/control-plane/internal/runtimekeys"
	"github.com/synara-ai/synara/services/control-plane/internal/scim"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/internal/serviceaccounts"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
	"github.com/synara-ai/synara/services/control-plane/internal/tenancy"
	controltracing "github.com/synara-ai/synara/services/control-plane/internal/tracing"
	"github.com/synara-ai/synara/services/control-plane/internal/workerreleases"
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
	tracingPolicy := controltracing.ExportPolicyDevelopment
	if cfg.Platform.Profile == platform.ProfileEnterprise {
		tracingPolicy = controltracing.ExportPolicyEnterprise
	}
	tracingShutdown, err := controltracing.Configure(ctx, "synara-control-plane", logger, tracingPolicy)
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
		MaxBatchSize: cfg.OutboxMaxBatchSize, MaxConcurrency: cfg.OutboxMaxConcurrency,
		ScaleUpDepth: int64(cfg.OutboxScaleUpDepth), TargetDelay: cfg.OutboxTargetDelay,
		ThrottleDepth: int64(cfg.OutboxThrottleDepth),
		MaxAttempts:   cfg.OutboxMaxAttempts, BaseBackoff: cfg.OutboxBaseBackoff,
		MaxBackoff: cfg.OutboxMaxBackoff,
	})
	if err != nil {
		logger.Error("failed to configure outbox service", "error", err)
		os.Exit(1)
	}
	incidentPublisher := outbox.Publisher(outbox.PublisherFunc(func(context.Context, outbox.Message) error {
		return errors.New("internal incident publisher is not configured")
	}))
	if cfg.InternalIncidentPublisherURL != "" {
		incidentPublisher, err = outbox.NewIncidentWebhookPublisher(outbox.IncidentWebhookPublisherConfig{
			Endpoint: cfg.InternalIncidentPublisherURL,
			HMACKey:  cfg.InternalIncidentPublisherHMACKey,
			Timeout:  cfg.InternalIncidentPublisherTimeout,
		})
		if err != nil {
			logger.Error("failed to configure internal incident publisher", "error", err)
			os.Exit(1)
		}
	}
	outboxDispatcher, err := outbox.NewDispatcher(
		outboxService,
		outbox.TopicPublisher{
			Default: outbox.DatabasePublisher{},
			Topics:  map[string]outbox.Publisher{outbox.InternalIncidentUpdateTopic: incidentPublisher},
		},
		cfg.OutboxPollInterval, metrics, logger,
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
	if cfg.PlatformOperatorTenantID == uuid.Nil && bootstrapped.Personal {
		cfg.PlatformOperatorTenantID = bootstrapped.TenantID
	}

	identityOptions := make([]identity.PersonalDomain, 0, 1)
	if bootstrapped.Personal {
		identityOptions = append(identityOptions, identity.PersonalDomain{UserID: bootstrapped.UserID, TenantID: bootstrapped.TenantID})
	}
	identityService := identity.NewService(db, cfg.SessionTTL, cfg.SessionIdleTTL, identityOptions...)
	projectService := projects.NewService(db)
	credentialCipher, err := credentialkms.New(ctx, credentialkms.Config{
		Provider: cfg.CredentialKMSProvider, KeyID: cfg.CredentialKMSKeyID,
		LocalKey: cfg.CredentialKMSLocalKey, Region: cfg.CredentialKMSAWSRegion,
		DecryptKeys: credentialKMSDecryptKeys(cfg.CredentialKMSDecryptKeys),
	})
	if err != nil {
		logger.Error("failed to configure provider credential KMS", "provider", cfg.CredentialKMSProvider, "error", err)
		os.Exit(1)
	}
	credentialService := credentials.NewService(db, credentialCipher)
	resolveImagePull := func(
		ctx context.Context,
		tenantID, targetID uuid.UUID,
		registrySelector string,
	) (executiontargets.ImagePullCredentialResolution, error) {
		resolved, err := credentialService.ResolveWorkerImagePullForTarget(ctx, tenantID, targetID, registrySelector)
		resolution := executiontargets.ImagePullCredentialResolution{Authoritative: resolved.Authoritative}
		if resolved.Credential == nil {
			return resolution, err
		}
		resolution.Credential = &executiontargets.ImagePullCredential{
			BindingID: resolved.Credential.BindingID, CredentialID: resolved.Credential.CredentialID,
			CredentialVersion: resolved.Credential.CredentialVersion, Host: resolved.Credential.Host,
			Username: resolved.Credential.Username, Password: resolved.Credential.Password,
			RegistryToken: resolved.Credential.RegistryToken,
		}
		return resolution, err
	}
	cursorCipher, err := runtimekeys.NewProviderCursorCipher(cfg)
	if err != nil {
		logger.Error("failed to configure provider cursor encryption", "error", err)
		os.Exit(1)
	}
	executionTargetService := executiontargets.NewService(db, cfg.Platform, cursorCipher)
	sshProvisioner := executiontargets.NewSSHProvisioner(executionTargetService, executiontargets.SSHProvisioningConfig{
		AgentdBinaryPath: cfg.AgentdBinaryPath, RegistrationToken: cfg.WorkerRegistrationToken,
		PublicControlPlaneURL: cfg.PublicControlPlaneURL, WorkerLeaseTTL: cfg.WorkerLeaseTTL,
		WorkerHeartbeatTimeout: cfg.WorkerHeartbeatTimeout, Timeout: cfg.SSHProvisionTimeout,
		ObservabilityRoot: cfg.SSHWorkerObservabilityRoot,
	})
	dockerReconciler := executiontargets.NewDockerPoolReconciler(executionTargetService, executiontargets.DockerPoolReconcilerConfig{
		RegistrationToken: cfg.WorkerRegistrationToken, PublicControlPlaneURL: cfg.PublicControlPlaneURL,
		WorkerLeaseTTL: cfg.WorkerLeaseTTL, Interval: cfg.DockerReconcileInterval,
		Observer: metrics, ResolveImagePull: resolveImagePull, ObservabilityRoot: cfg.DockerWorkerObservabilityRoot,
	}, logger)
	reconcilerLeadershipConfig, err := loadReconcilerLeadershipConfig(cfg)
	if err != nil {
		logger.Error("failed to configure reconciler leadership", "error", err)
		os.Exit(1)
	}
	reconcilerLeadership, err := leadership.New(db, leadership.Config{
		HolderID: reconcilerLeadershipConfig.HolderID,
		LeaseTTL: reconcilerLeadershipConfig.LeaseTTL,
	})
	if err != nil {
		logger.Error("failed to initialize reconciler leadership", "error", err)
		os.Exit(1)
	}
	lifecyclePolicyService, err := lifecyclepolicy.NewService(db, cfg.ResourceLifecycle)
	if err != nil {
		logger.Error("failed to configure Resource Lifecycle Policy", "error", err)
		os.Exit(1)
	}
	sessionService := sessions.NewService(
		db, projectService, executionTargetService,
		sessions.WithProviderCapabilityHeartbeatTimeout(cfg.WorkerHeartbeatTimeout),
		sessions.WithLifecyclePolicyResolver(lifecyclePolicyService),
	)
	memoryService := memories.NewService(db)
	executionService := executions.NewService(
		db, sessionService, cfg.WorkerLeaseTTL, cfg.WorkerHeartbeatTimeout,
		cfg.WorkerReceiptTTL, cursorCipher, executionTargetService,
		executions.WithProjectService(projectService),
		executions.WithMemoryReferenceResolver(memoryService),
		executions.WithProviderCredentialAccessTTL(cfg.ProviderCredentialAccessTTL),
		executions.WithProviderCursorMaximumAge(cfg.ProviderCursorMaximumAge),
		executions.WithHostedProviderCommercialAuthorization(
			cfg.PlatformOperatorTenantID, cfg.Platform.Profile == platform.ProfileEnterprise,
		),
	)
	workerPoolAutoscalingService := poolautoscaling.NewService(
		db,
		poolautoscaling.WithColdStartExpirer(func(
			ctx context.Context,
			input poolautoscaling.ColdStartExpiry,
		) (int, error) {
			return executionService.ExpireInteractiveColdStarts(ctx, executions.ColdStartDeadlineScope{
				TenantID: input.TenantID, ExecutionTargetID: input.ExecutionTargetID,
				WorkerPoolID: input.WorkerPoolID, WorkerPoolVersion: input.WorkerPoolVersion,
				Cutoff: input.Cutoff, Limit: input.Limit,
			})
		}),
	)
	dockerReconciler.SetWorkerLifecycleCoordinator(executionService)
	sshProvisioner.SetWorkerAuthorityRevoker(executionService.RevokeExecutionTargetWorkersInTransaction)
	billingAdapter, billingImports, billingCloser, err := billing.NewAdapterFromRuntime(ctx, cfg.Billing)
	if err != nil {
		logger.Error("failed to configure billing adapter", "error", err)
		os.Exit(1)
	}
	if billingCloser != nil {
		defer func() {
			if closeErr := billingCloser.Close(); closeErr != nil {
				logger.Warn("billing source shutdown failed", "error", closeErr)
			}
		}()
	}
	billingOperatorTenantID := cfg.Billing.TariffOperatorTenantID
	if billingOperatorTenantID == uuid.Nil && bootstrapped.Personal {
		billingOperatorTenantID = bootstrapped.TenantID
	}
	if len(cfg.Billing.SharedAllocations) > 0 && billingOperatorTenantID == uuid.Nil {
		logger.Error("shared billing allocation scheduler requires a configured platform billing operator Tenant")
		os.Exit(1)
	}
	billingService := billing.NewService(
		db,
		billingAdapter,
		billing.WithConfiguredImports(billingImports),
		billing.WithConfiguredSharedAllocations(cfg.Billing.SharedAllocations),
		billing.WithPlatformBillingOperatorTenant(billingOperatorTenantID),
		billing.WithBuiltInEstimateSweeper(),
	)
	resourceLifecycleController := lifecyclepolicy.NewController(
		executionService, cfg.ResourceLifecycleSweepInterval, logger, metrics,
	)
	tenancyService := tenancy.NewService(db, executionService)
	managedKubernetesObservationTTL := minDuration(
		time.Hour,
		maxDuration(30*time.Second, 3*cfg.KubernetesReconcileInterval),
	)
	managedKubernetesRoutingPublisher := executiontargets.NewManagedKubernetesRoutingPublisher(
		executionTargetService,
		executiontargets.ManagedKubernetesRoutingPublisherConfig{
			PublisherIdentity: "managed-kubernetes-routing-publisher:" + reconcilerLeadershipConfig.HolderID,
			ObservationTTL:    managedKubernetesObservationTTL,
		},
	)
	managedKubernetesWarmCapacityPublisher := executiontargets.NewManagedKubernetesWarmCapacityPublisher(
		executionTargetService,
		executiontargets.ManagedKubernetesWarmCapacityPublisherConfig{
			PublisherIdentity: "managed-kubernetes-warm-capacity-publisher:" + reconcilerLeadershipConfig.HolderID,
			ObservationTTL:    managedKubernetesObservationTTL,
		},
	)
	managedKubernetesTargetCapacityPublisher := executiontargets.NewManagedKubernetesTargetCapacityPublisher(
		executionTargetService,
		executiontargets.ManagedKubernetesTargetCapacityPublisherConfig{
			PublisherIdentity: "managed-kubernetes-target-capacity-publisher:" + reconcilerLeadershipConfig.HolderID,
			ObservationTTL:    managedKubernetesObservationTTL,
		},
	)
	kubernetesReconciler := executiontargets.NewKubernetesReconciler(executionTargetService, executiontargets.KubernetesReconcilerConfig{
		PublicControlPlaneURL: cfg.PublicControlPlaneURL,
		WorkerLeaseTTL:        cfg.WorkerLeaseTTL, WorkerHeartbeatTimeout: cfg.WorkerHeartbeatTimeout,
		Interval:                           cfg.KubernetesReconcileInterval,
		RecoverExpired:                     executionService.RecoverExpired,
		ReconcileEphemeralWorkspaceCleanup: executionService.ReconcileEphemeralWorkspaceCleanup,
		FinalizeResourceSuspend: func(ctx context.Context, observation executiontargets.KubernetesPodTerminalObservation) (bool, error) {
			return executionService.FinalizeKubernetesResourceSuspend(ctx, executions.KubernetesPodTerminalProof{
				ExecutionTargetID: observation.ExecutionTargetID,
				ExecutionID:       observation.ExecutionID, Generation: observation.Generation,
				Namespace: observation.Namespace, PodName: observation.PodName,
				PodUID: observation.PodUID, Phase: observation.Phase, ObservedAt: observation.ObservedAt,
			})
		},
		ObserveWorkerPod:           executionService.ObserveKubernetesWorkerPod,
		ObserveExecutionPod:        executionService.ObserveKubernetesExecutionPod,
		PodPendingFailureThreshold: cfg.KubernetesPodPendingFailureThreshold,
		PublishRoutingHealth:       managedKubernetesRoutingPublisher.PublishReconcile,
		PublishWarmCapacity:        managedKubernetesWarmCapacityPublisher.PublishReconcile,
		PublishTargetCapacity:      managedKubernetesTargetCapacityPublisher.PublishReconcile,
		Observer:                   metrics, ResolveImagePull: resolveImagePull,
	}, logger)
	workerReleaseAutoRollback := workerreleases.NewAutoRollbackController(
		workerreleases.NewService(db),
		workerreleases.AutoRollbackControllerConfig{
			Enabled: cfg.WorkerAutoRollbackEnabled, Interval: cfg.WorkerAutoRollbackInterval, Observer: metrics,
		},
		logger,
	)
	artifactStore, err := artifacts.NewStore(ctx, cfg)
	if err != nil {
		logger.Error("failed to configure artifact store", "kind", cfg.Platform.ArtifactStore, "error", err)
		os.Exit(1)
	}
	artifactService := artifacts.NewService(db, artifactStore, cfg, executionService, sessionService, metrics)
	quotaService := quotas.NewService(db)
	serviceAccountService := serviceaccounts.NewService(db)
	enterpriseIdentityService := enterpriseidentity.NewService(db, identityService, credentialCipher)
	scimService := scim.NewService(db)
	retentionService := retention.NewService(
		db, sessionService, artifactService, executionService, cfg.RetentionSweepInterval, logger, metrics,
	)
	metricRollupService := metricrollup.NewService(db)
	api, err := httpapi.New(
		cfg, db, identityService, tenancyService, projectService, sessionService,
		executionService, executionTargetService, sshProvisioner, artifactService, quotaService,
		credentialService, retentionService, metrics, outboxService, enterpriseIdentityService,
		serviceAccountService, scimService, schemaChecker, logger, httpapi.WithBilling(billingService),
	)
	if err != nil {
		logger.Error("failed to configure HTTP API", "error", err)
		os.Exit(1)
	}
	server := newControlPlaneHTTPServer(cfg.ListenAddress, api.Handler(), runtimeContext, stopRuntime)
	dockerLeaderRunner, err := reconcilerleadership.NewRunner(reconcilerLeadership, reconcilerleadership.RunnerConfig{
		LeaseName:         "synara:docker-worker-pool-reconciler",
		CycleInterval:     cfg.DockerReconcileInterval,
		AcquireRetryDelay: reconcilerLeadershipConfig.AcquireRetryDelay,
		RenewInterval:     reconcilerLeadershipConfig.RenewInterval,
		AssertInterval:    reconcilerLeadershipConfig.AssertInterval,
		Logger:            logger,
	})
	if err != nil {
		logger.Error("failed to configure docker reconciler leadership runner", "error", err)
		os.Exit(1)
	}
	kubernetesLeaderRunner, err := reconcilerleadership.NewRunner(reconcilerLeadership, reconcilerleadership.RunnerConfig{
		LeaseName:         "synara:kubernetes-execution-reconciler",
		CycleInterval:     cfg.KubernetesReconcileInterval,
		AcquireRetryDelay: reconcilerLeadershipConfig.AcquireRetryDelay,
		RenewInterval:     reconcilerLeadershipConfig.RenewInterval,
		AssertInterval:    reconcilerLeadershipConfig.AssertInterval,
		Logger:            logger,
	})
	if err != nil {
		logger.Error("failed to configure kubernetes reconciler leadership runner", "error", err)
		os.Exit(1)
	}
	targetFailoverLeaderRunner, err := reconcilerleadership.NewRunner(reconcilerLeadership, reconcilerleadership.RunnerConfig{
		LeaseName:         "synara:global-target-failover-sweep",
		CycleInterval:     reconcilerLeadershipConfig.TargetFailoverSweepInterval,
		AcquireRetryDelay: reconcilerLeadershipConfig.AcquireRetryDelay,
		RenewInterval:     reconcilerLeadershipConfig.RenewInterval,
		AssertInterval:    reconcilerLeadershipConfig.AssertInterval,
		Logger:            logger,
	})
	if err != nil {
		logger.Error("failed to configure target failover leadership runner", "error", err)
		os.Exit(1)
	}
	resourceLifecycleLeaderRunner, err := reconcilerleadership.NewRunner(reconcilerLeadership, reconcilerleadership.RunnerConfig{
		LeaseName:         "synara:session-resource-lifecycle",
		CycleInterval:     cfg.ResourceLifecycleSweepInterval,
		AcquireRetryDelay: reconcilerLeadershipConfig.AcquireRetryDelay,
		RenewInterval:     reconcilerLeadershipConfig.RenewInterval,
		AssertInterval:    reconcilerLeadershipConfig.AssertInterval,
		Logger:            logger,
	})
	if err != nil {
		logger.Error("failed to configure resource lifecycle leadership runner", "error", err)
		os.Exit(1)
	}
	workerReleaseLeaderRunner, err := reconcilerleadership.NewRunner(reconcilerLeadership, reconcilerleadership.RunnerConfig{
		LeaseName:         "synara:worker-release-auto-rollback",
		CycleInterval:     cfg.WorkerAutoRollbackInterval,
		AcquireRetryDelay: reconcilerLeadershipConfig.AcquireRetryDelay,
		RenewInterval:     reconcilerLeadershipConfig.RenewInterval,
		AssertInterval:    reconcilerLeadershipConfig.AssertInterval,
		Logger:            logger,
	})
	if err != nil {
		logger.Error("failed to configure Worker release rollback leadership runner", "error", err)
		os.Exit(1)
	}
	retentionLeaderRunner, err := reconcilerleadership.NewRunner(reconcilerLeadership, reconcilerleadership.RunnerConfig{
		LeaseName:         "synara:tenant-retention-sweeper",
		CycleInterval:     cfg.RetentionSweepInterval,
		AcquireRetryDelay: reconcilerLeadershipConfig.AcquireRetryDelay,
		RenewInterval:     reconcilerLeadershipConfig.RenewInterval,
		AssertInterval:    reconcilerLeadershipConfig.AssertInterval,
		Logger:            logger,
	})
	if err != nil {
		logger.Error("failed to configure retention leadership runner", "error", err)
		os.Exit(1)
	}
	metricRollupLeaderRunner, err := reconcilerleadership.NewRunner(reconcilerLeadership, reconcilerleadership.RunnerConfig{
		LeaseName:         "synara:metric-rollup",
		CycleInterval:     cfg.MetricRollupInterval,
		AcquireRetryDelay: reconcilerLeadershipConfig.AcquireRetryDelay,
		RenewInterval:     reconcilerLeadershipConfig.RenewInterval,
		AssertInterval:    reconcilerLeadershipConfig.AssertInterval,
		Logger:            logger,
	})
	if err != nil {
		logger.Error("failed to configure metric rollup leadership runner", "error", err)
		os.Exit(1)
	}
	workerPoolAutoscalingLeaderRunner, err := reconcilerleadership.NewRunner(reconcilerLeadership, reconcilerleadership.RunnerConfig{
		LeaseName:         "synara:worker-pool-autoscaling",
		CycleInterval:     cfg.WorkerPoolAutoscalingInterval,
		AcquireRetryDelay: reconcilerLeadershipConfig.AcquireRetryDelay,
		RenewInterval:     reconcilerLeadershipConfig.RenewInterval,
		AssertInterval:    reconcilerLeadershipConfig.AssertInterval,
		Logger:            logger,
	})
	if err != nil {
		logger.Error("failed to configure Worker Pool autoscaling leadership runner", "error", err)
		os.Exit(1)
	}
	var billingImportLeaderRunner *reconcilerleadership.Runner
	if cycleInterval := billingImportScheduleInterval(cfg.Billing); cycleInterval > 0 {
		billingImportLeaderRunner, err = reconcilerleadership.NewRunner(reconcilerLeadership, reconcilerleadership.RunnerConfig{
			LeaseName:         "synara:billing-import-scheduler",
			CycleInterval:     cycleInterval,
			AcquireRetryDelay: reconcilerLeadershipConfig.AcquireRetryDelay,
			RenewInterval:     reconcilerLeadershipConfig.RenewInterval,
			AssertInterval:    reconcilerLeadershipConfig.AssertInterval,
			Logger:            logger,
		})
		if err != nil {
			logger.Error("failed to configure billing import leadership runner", "error", err)
			os.Exit(1)
		}
	}
	var billingSharedAllocationLeaderRunner *reconcilerleadership.Runner
	if cycleInterval := billingSharedAllocationScheduleInterval(cfg.Billing); cycleInterval > 0 {
		billingSharedAllocationLeaderRunner, err = reconcilerleadership.NewRunner(reconcilerLeadership, reconcilerleadership.RunnerConfig{
			LeaseName:         "synara:billing-shared-allocation-scheduler",
			CycleInterval:     cycleInterval,
			AcquireRetryDelay: reconcilerLeadershipConfig.AcquireRetryDelay,
			RenewInterval:     reconcilerLeadershipConfig.RenewInterval,
			AssertInterval:    reconcilerLeadershipConfig.AssertInterval,
			Logger:            logger,
		})
		if err != nil {
			logger.Error("failed to configure shared billing allocation leadership runner", "error", err)
			os.Exit(1)
		}
	}
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
	startBackground(func() {
		dockerLeaderRunner.Run(runtimeContext, func(run reconcilerleadership.RunContext) error {
			return observeLeadershipBackground(metrics, "docker", func() error {
				return dockerReconciler.ReconcileOnce(run.Context)
			})
		})
	})
	startBackground(func() {
		kubernetesLeaderRunner.Run(runtimeContext, func(run reconcilerleadership.RunContext) error {
			return observeLeadershipBackground(metrics, "kubernetes", func() error {
				return kubernetesReconciler.ReconcileOnce(run.Context)
			})
		})
	})
	startBackground(func() {
		targetFailoverLeaderRunner.Run(runtimeContext, func(run reconcilerleadership.RunContext) error {
			return observeLeadershipBackground(metrics, "target-failover", func() error {
				return reconcileTargetFailovers(run, sessionService)
			})
		})
	})
	startBackground(func() {
		resourceLifecycleLeaderRunner.Run(runtimeContext, func(run reconcilerleadership.RunContext) error {
			return observeLeadershipBackground(metrics, "resource-lifecycle", func() error {
				return resourceLifecycleController.RunOnce(run.Context, 200)
			})
		})
	})
	startBackground(func() {
		workerReleaseLeaderRunner.Run(runtimeContext, func(run reconcilerleadership.RunContext) error {
			return observeLeadershipBackground(metrics, "worker-release-auto-rollback", func() error {
				return workerReleaseAutoRollback.EvaluateOnce(run.Context)
			})
		})
	})
	startBackground(func() {
		retentionLeaderRunner.Run(runtimeContext, func(run reconcilerleadership.RunContext) error {
			return observeLeadershipBackground(metrics, "retention", func() error {
				return retentionService.RunOnce(run.Context, 200)
			})
		})
	})
	startBackground(func() {
		metricRollupLeaderRunner.Run(runtimeContext, func(run reconcilerleadership.RunContext) error {
			return observeLeadershipBackground(metrics, "metric-rollup", func() error {
				summary, err := metricRollupService.RunOnce(run.Context, cfg.MetricRollupBatchSize)
				logger.Debug(
					"metric rollup cycle completed",
					"processedFacts", summary.ProcessedFacts,
					"processedWorkerFacts", summary.ProcessedWorkerFacts,
					"processedGenerationFacts", summary.ProcessedGenerationFacts,
					"processedPodFailureFacts", summary.ProcessedPodFailureFacts,
					"updatedBuckets", summary.UpdatedBuckets,
					"error", err,
				)
				return err
			})
		})
	})
	startBackground(func() {
		workerPoolAutoscalingLeaderRunner.Run(runtimeContext, func(run reconcilerleadership.RunContext) error {
			return observeLeadershipBackground(metrics, "worker-pool-autoscaling", func() error {
				summary, err := workerPoolAutoscalingService.RunOnce(run.Context, cfg.WorkerPoolAutoscalingBatchSize)
				logger.Debug(
					"Worker Pool autoscaling cycle completed",
					"evaluated", summary.Evaluated, "scaledUp", summary.ScaledUp,
					"scaledDown", summary.ScaledDown,
					"coldStartViolated", summary.ColdStartViolated,
					"expiredExecutions", summary.ExpiredExecutions, "error", err,
				)
				return err
			})
		})
	})
	if billingImportLeaderRunner != nil {
		startBackground(func() {
			billingImportLeaderRunner.Run(runtimeContext, func(run reconcilerleadership.RunContext) error {
				return observeLeadershipBackground(metrics, "billing-import-scheduler", func() error {
					summary, err := billingService.RunImportSchedulerOnce(run.Context)
					log := logger.Debug
					if err != nil {
						log = logger.Warn
					}
					log(
						"billing import scheduler cycle completed",
						"checked", summary.Checked,
						"imported", summary.Imported,
						"reconciled", summary.Reconciled,
						"estimateWorkers", summary.EstimateWorkers,
						"estimateRows", summary.EstimateSweeps,
						"estimateWorkerFailures", summary.EstimateWorkerFailures,
						"skipped", summary.Skipped,
						"failed", summary.Failed,
						"error", err,
					)
					return err
				})
			})
		})
	}
	if billingSharedAllocationLeaderRunner != nil {
		startBackground(func() {
			billingSharedAllocationLeaderRunner.Run(runtimeContext, func(run reconcilerleadership.RunContext) error {
				return observeLeadershipBackground(metrics, "billing-shared-allocation-scheduler", func() error {
					summary, err := billingService.RunSharedAllocationSchedulerOnce(run.Context)
					log := logger.Debug
					if err != nil {
						log = logger.Warn
					}
					log(
						"shared billing allocation scheduler cycle completed",
						"checked", summary.Checked,
						"generatedCalendarPeriods", summary.GeneratedCalendarPeriods,
						"attempted", summary.Attempted,
						"completed", summary.Completed,
						"workers", summary.Workers,
						"allocationRuns", summary.AllocationRuns,
						"allocationSlices", summary.AllocationSlices,
						"failedWorkers", summary.FailedWorkers,
						"notSettled", summary.NotSettled,
						"skipped", summary.Skipped,
						"failed", summary.Failed,
						"error", err,
					)
					return err
				})
			})
		})
	}
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

func credentialKMSDecryptKeys(values []config.CredentialKMSDecryptKeyConfig) []credentialkms.DecryptKeyConfig {
	result := make([]credentialkms.DecryptKeyConfig, 0, len(values))
	for _, value := range values {
		result = append(result, credentialkms.DecryptKeyConfig{
			Provider: value.Provider, KeyID: value.KeyID, LocalKey: value.LocalKey, Region: value.Region,
		})
	}
	return result
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

type reconcilerLeadershipConfig struct {
	HolderID                    string
	LeaseTTL                    time.Duration
	RenewInterval               time.Duration
	AssertInterval              time.Duration
	AcquireRetryDelay           time.Duration
	TargetFailoverSweepInterval time.Duration
}

func loadReconcilerLeadershipConfig(cfg config.Config) (reconcilerLeadershipConfig, error) {
	holderID := strings.TrimSpace(os.Getenv("SYNARA_RECONCILER_LEASE_HOLDER_ID"))
	if holderID == "" {
		holderID = defaultReconcilerLeaseHolderID()
	}

	failoverInterval, err := envDuration("SYNARA_TARGET_FAILOVER_SWEEP_INTERVAL", 10*time.Second)
	if err != nil {
		return reconcilerLeadershipConfig{}, err
	}
	leaseTTL, err := envDuration("SYNARA_RECONCILER_LEASE_TTL", 30*time.Second)
	if err != nil {
		return reconcilerLeadershipConfig{}, err
	}
	renewInterval, err := envDuration("SYNARA_RECONCILER_LEASE_RENEW_INTERVAL", maxDuration(2*time.Second, leaseTTL/3))
	if err != nil {
		return reconcilerLeadershipConfig{}, err
	}
	assertInterval, err := envDuration("SYNARA_RECONCILER_LEASE_ASSERT_INTERVAL", minDuration(5*time.Second, maxDuration(time.Second, leaseTTL/4)))
	if err != nil {
		return reconcilerLeadershipConfig{}, err
	}
	acquireRetryDelay, err := envDuration("SYNARA_RECONCILER_LEASE_ACQUIRE_RETRY_DELAY", 2*time.Second)
	if err != nil {
		return reconcilerLeadershipConfig{}, err
	}
	if leaseTTL <= 0 {
		return reconcilerLeadershipConfig{}, errors.New("SYNARA_RECONCILER_LEASE_TTL must be positive")
	}
	if renewInterval <= 0 || renewInterval >= leaseTTL {
		return reconcilerLeadershipConfig{}, errors.New("SYNARA_RECONCILER_LEASE_RENEW_INTERVAL must be positive and less than SYNARA_RECONCILER_LEASE_TTL")
	}
	if assertInterval <= 0 || assertInterval >= leaseTTL {
		return reconcilerLeadershipConfig{}, errors.New("SYNARA_RECONCILER_LEASE_ASSERT_INTERVAL must be positive and less than SYNARA_RECONCILER_LEASE_TTL")
	}
	if acquireRetryDelay <= 0 {
		return reconcilerLeadershipConfig{}, errors.New("SYNARA_RECONCILER_LEASE_ACQUIRE_RETRY_DELAY must be positive")
	}
	if failoverInterval <= 0 {
		return reconcilerLeadershipConfig{}, errors.New("SYNARA_TARGET_FAILOVER_SWEEP_INTERVAL must be positive")
	}
	return reconcilerLeadershipConfig{
		HolderID:                    holderID,
		LeaseTTL:                    leaseTTL,
		RenewInterval:               renewInterval,
		AssertInterval:              assertInterval,
		AcquireRetryDelay:           acquireRetryDelay,
		TargetFailoverSweepInterval: failoverInterval,
	}, nil
}

func defaultReconcilerLeaseHolderID() string {
	host := strings.TrimSpace(os.Getenv("HOSTNAME"))
	if host == "" {
		if resolved, err := os.Hostname(); err == nil {
			host = strings.TrimSpace(resolved)
		}
	}
	if host == "" {
		host = "unknown-host"
	}
	if len(host) > 80 {
		host = host[:80]
	}
	return fmt.Sprintf("%s:%d:%s", host, os.Getpid(), uuid.NewString())
}

func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a valid duration: %w", name, err)
	}
	return duration, nil
}

func maxDuration(left, right time.Duration) time.Duration {
	if left > right {
		return left
	}
	return right
}

func minDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}

func billingImportScheduleInterval(config billing.RuntimeConfig) time.Duration {
	return config.MinimumScheduleInterval()
}

func billingSharedAllocationScheduleInterval(config billing.RuntimeConfig) time.Duration {
	return config.MinimumSharedAllocationScheduleInterval()
}

func observeLeadershipBackground(observer interface {
	ObserveBackground(kind string, started time.Time, err error)
}, kind string, work func() error) error {
	started := time.Now()
	err := work()
	observedErr := err
	if errors.Is(err, reconcilerleadership.ErrLeadershipLost) || errors.Is(err, context.Canceled) {
		observedErr = nil
	}
	if observer != nil {
		observer.ObserveBackground(kind, started, observedErr)
	}
	return err
}

func reconcileTargetFailovers(run reconcilerleadership.RunContext, sessionService *sessions.Service) error {
	for {
		if err := run.AssertActive(run.Context); err != nil {
			return err
		}
		summary, err := sessionService.ReconcileTargetFailovers(run.Context, sessions.TargetFailoverFence{
			LeaseName: run.Lease.Name, HolderID: run.Lease.HolderID, FencingToken: run.Lease.FencingToken,
		}, 1)
		if err != nil {
			return err
		}
		if summary.Candidates == 0 || summary.Committed == 0 {
			return nil
		}
		select {
		case <-run.Context.Done():
			return run.Context.Err()
		default:
		}
	}
}
