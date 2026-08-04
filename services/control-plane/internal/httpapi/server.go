package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/netip"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/artifacts"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/billing"
	"github.com/synara-ai/synara/services/control-plane/internal/capacitygovernance"
	"github.com/synara-ai/synara/services/control-plane/internal/compliancegovernance"
	"github.com/synara-ai/synara/services/control-plane/internal/config"
	"github.com/synara-ai/synara/services/control-plane/internal/credentialbindings"
	"github.com/synara-ai/synara/services/control-plane/internal/credentials"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/desktopenrollment"
	"github.com/synara-ai/synara/services/control-plane/internal/developerapi"
	"github.com/synara-ai/synara/services/control-plane/internal/developerwebhooks"
	"github.com/synara-ai/synara/services/control-plane/internal/enterpriseidentity"
	"github.com/synara-ai/synara/services/control-plane/internal/entitlements"
	"github.com/synara-ai/synara/services/control-plane/internal/eventstream"
	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/governanceauthority"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/incidentexercisegovernance"
	"github.com/synara-ai/synara/services/control-plane/internal/incidentgovernance"
	"github.com/synara-ai/synara/services/control-plane/internal/internalcostgovernance"
	"github.com/synara-ai/synara/services/control-plane/internal/legalholds"
	"github.com/synara-ai/synara/services/control-plane/internal/lifecyclepolicy"
	"github.com/synara-ai/synara/services/control-plane/internal/memories"
	"github.com/synara-ai/synara/services/control-plane/internal/observability"
	"github.com/synara-ai/synara/services/control-plane/internal/operationsexercisegovernance"
	"github.com/synara-ai/synara/services/control-plane/internal/outbox"
	"github.com/synara-ai/synara/services/control-plane/internal/penetrationgovernance"
	"github.com/synara-ai/synara/services/control-plane/internal/placement"
	"github.com/synara-ai/synara/services/control-plane/internal/podlifecycle"
	"github.com/synara-ai/synara/services/control-plane/internal/poolautoscaling"
	"github.com/synara-ai/synara/services/control-plane/internal/privacy"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/projects"
	"github.com/synara-ai/synara/services/control-plane/internal/providercommercial"
	"github.com/synara-ai/synara/services/control-plane/internal/quotas"
	"github.com/synara-ai/synara/services/control-plane/internal/recoverygovernance"
	"github.com/synara-ai/synara/services/control-plane/internal/releasegovernance"
	"github.com/synara-ai/synara/services/control-plane/internal/retention"
	"github.com/synara-ai/synara/services/control-plane/internal/routing"
	"github.com/synara-ai/synara/services/control-plane/internal/schedulingpolicy"
	"github.com/synara-ai/synara/services/control-plane/internal/scim"
	"github.com/synara-ai/synara/services/control-plane/internal/serviceaccounts"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
	"github.com/synara-ai/synara/services/control-plane/internal/slogovernance"
	"github.com/synara-ai/synara/services/control-plane/internal/supportaccess"
	"github.com/synara-ai/synara/services/control-plane/internal/tenancy"
	controltracing "github.com/synara-ai/synara/services/control-plane/internal/tracing"
	"github.com/synara-ai/synara/services/control-plane/internal/usage"
	"github.com/synara-ai/synara/services/control-plane/internal/workerreleases"
)

const maxJSONBodyBytes = 1 << 20

type principalContextKey struct{}
type requestIDContextKey struct{}
type traceIDContextKey struct{}
type clientIPContextKey struct{}
type workerContextKey struct{}
type serviceAccountContextKey struct{}
type requestLogScopeContextKey struct{}

type requestLogScope struct {
	tenantID       uuid.UUID
	organizationID uuid.UUID
	workerID       uuid.UUID
	generation     int64
}

type Server struct {
	config                       config.Config
	apiRoutes                    []apiRoute
	db                           *gorm.DB
	identity                     *identity.Service
	tenancy                      *tenancy.Service
	projects                     *projects.Service
	sessions                     *sessions.Service
	executions                   *executions.Service
	targets                      *executiontargets.Service
	sshTargets                   *executiontargets.SSHProvisioner
	artifacts                    *artifacts.Service
	billing                      *billing.Service
	memories                     *memories.Service
	lifecyclePolicies            *lifecyclepolicy.Service
	quotas                       *quotas.Service
	entitlements                 *entitlements.Service
	usage                        *usage.Service
	credentials                  *credentials.Service
	credentialBindings           *credentialbindings.Service
	workerReleases               *workerreleases.Service
	placement                    *placement.Service
	poolAutoscaling              *poolautoscaling.Service
	routing                      *routing.Service
	platformRouting              *routing.PlatformAuthorityService
	schedulingPolicies           *schedulingpolicy.Service
	retention                    *retention.Service
	legalHolds                   *legalholds.Service
	privacy                      *privacy.Service
	releaseGovernance            *releasegovernance.Service
	incidentGovernance           *incidentgovernance.Service
	incidentExerciseGovernance   *incidentexercisegovernance.Service
	operationsExerciseGovernance *operationsexercisegovernance.Service
	internalCostGovernance       *internalcostgovernance.Service
	sloGovernance                *slogovernance.Service
	recoveryGovernance           *recoverygovernance.Service
	penetrationGovernance        *penetrationgovernance.Service
	capacityGovernance           *capacitygovernance.Service
	complianceGovernance         *compliancegovernance.Service
	providerCommercial           *providercommercial.Service
	governanceAuthority          *governanceauthority.Service
	metrics                      *observability.Registry
	outbox                       *outbox.Service
	enterpriseIdentity           *enterpriseidentity.Service
	serviceAccounts              *serviceaccounts.Service
	developerAPIUsage            *developerapi.UsageService
	developerWebhooks            *developerwebhooks.Service
	supportAccess                *supportaccess.Service
	desktopEnrollment            *desktopenrollment.Service
	desktopRedeemLimit           *desktopRedemptionRateLimiter
	scim                         *scim.Service
	logger                       *slog.Logger
	schema                       schemaReadiness
	eventStreams                 *eventstream.Service
	sessionEventPoll             time.Duration
	sessionEventBeat             time.Duration
	sessionEventWrite            time.Duration
	handler                      http.Handler
}

type schemaReadiness interface {
	Check(context.Context) (database.SchemaStatus, error)
	CheckWrite(context.Context) error
}

type Option func(*Server)

func WithBilling(service *billing.Service) Option {
	return func(server *Server) {
		if server == nil {
			return
		}
		server.billing = service
	}
}

func WithDeveloperWebhooks(service *developerwebhooks.Service) Option {
	return func(server *Server) {
		if server == nil {
			return
		}
		server.developerWebhooks = service
	}
}

func New(
	cfg config.Config,
	db *gorm.DB,
	identityService *identity.Service,
	tenancyService *tenancy.Service,
	projectService *projects.Service,
	sessionService *sessions.Service,
	executionService *executions.Service,
	executionTargetService *executiontargets.Service,
	sshProvisioner *executiontargets.SSHProvisioner,
	artifactService *artifacts.Service,
	quotaService *quotas.Service,
	credentialService *credentials.Service,
	retentionService *retention.Service,
	metrics *observability.Registry,
	outboxService *outbox.Service,
	enterpriseIdentityService *enterpriseidentity.Service,
	serviceAccountService *serviceaccounts.Service,
	scimService *scim.Service,
	schemaChecker schemaReadiness,
	logger *slog.Logger,
	options ...Option,
) (*Server, error) {
	eventStreams, err := eventstream.New(db, eventstream.Config{
		InstanceID: uuid.NewString(), LeaseTTL: cfg.SSELeaseTTL,
		MaxConnectionsPerUser:   cfg.SSEMaxConnectionsPerUser,
		MaxConnectionsPerTenant: cfg.SSEMaxConnectionsPerTenant,
	})
	if err != nil {
		return nil, err
	}
	resourceLifecycleConfig := cfg.ResourceLifecycle
	if reflect.DeepEqual(resourceLifecycleConfig, lifecyclepolicy.Config{}) {
		resourceLifecycleConfig = lifecyclepolicy.DefaultConfig(cfg.Platform.Profile)
	}
	lifecyclePolicies, err := lifecyclepolicy.NewService(db, resourceLifecycleConfig)
	if err != nil {
		return nil, err
	}
	platformRouting, err := routing.NewPlatformAuthorityService(db, cfg.PlatformRoutingPublishers)
	if err != nil {
		return nil, fmt.Errorf("configure Platform routing authority publishers: %w", err)
	}
	supportAccessService := supportaccess.NewService(db, cfg.PlatformOperatorTenantID)
	governanceAuthorityService := governanceauthority.NewService(db, cfg.PlatformOperatorTenantID)
	server := &Server{
		config: cfg, db: db, identity: identityService, tenancy: tenancyService,
		projects: projectService, sessions: sessionService, executions: executionService,
		targets: executionTargetService, sshTargets: sshProvisioner,
		artifacts: artifactService, memories: memories.NewService(db), lifecyclePolicies: lifecyclePolicies,
		quotas: quotaService, entitlements: entitlements.NewService(db), usage: usage.NewService(db),
		credentials: credentialService, credentialBindings: credentialbindings.NewService(db, credentialService),
		workerReleases: workerreleases.NewService(db), placement: placement.NewService(db),
		poolAutoscaling: poolautoscaling.NewService(db), routing: routing.NewService(db),
		platformRouting:    platformRouting,
		schedulingPolicies: schedulingpolicy.NewService(db),
		retention:          retentionService, legalHolds: legalholds.NewService(db),
		privacy:                      privacy.NewService(db, artifactService),
		releaseGovernance:            releasegovernance.NewService(db, cfg.PlatformOperatorTenantID),
		incidentGovernance:           incidentgovernance.NewService(db, cfg.PlatformOperatorTenantID, cfg.InternalStatusBoardURL),
		incidentExerciseGovernance:   incidentexercisegovernance.NewService(db, cfg.PlatformOperatorTenantID),
		operationsExerciseGovernance: operationsexercisegovernance.NewService(db, cfg.PlatformOperatorTenantID),
		internalCostGovernance:       internalcostgovernance.NewService(db, cfg.PlatformOperatorTenantID),
		sloGovernance:                slogovernance.NewService(db, cfg.PlatformOperatorTenantID),
		recoveryGovernance:           recoverygovernance.NewService(db, cfg.PlatformOperatorTenantID),
		penetrationGovernance:        penetrationgovernance.NewService(db, cfg.PlatformOperatorTenantID),
		capacityGovernance:           capacitygovernance.NewService(db, cfg.PlatformOperatorTenantID),
		complianceGovernance:         compliancegovernance.NewService(db, cfg.PlatformOperatorTenantID),
		providerCommercial:           providercommercial.NewService(db, cfg.PlatformOperatorTenantID),
		governanceAuthority:          governanceAuthorityService,
		metrics:                      metrics, outbox: outboxService,
		enterpriseIdentity: enterpriseIdentityService, serviceAccounts: serviceAccountService,
		developerAPIUsage: developerapi.NewUsageService(db),
		supportAccess:     supportAccessService,
		desktopEnrollment: desktopenrollment.NewService(db, supportAccessService, desktopenrollment.Config{
			ControlPlaneOrigin: cfg.PublicControlPlaneURL, EnrollmentTTL: cfg.DesktopEnrollmentTTL,
			SessionTTL: cfg.SessionTTL, SessionIdleTTL: cfg.SessionIdleTTL,
		}),
		desktopRedeemLimit: newDesktopRedemptionRateLimiter(20, time.Minute),
		scim:               scimService, schema: schemaChecker, logger: logger, eventStreams: eventStreams,
		sessionEventPoll: cfg.SSEPollInterval, sessionEventBeat: cfg.SSEHeartbeatInterval,
		sessionEventWrite: cfg.SSEWriteTimeout,
	}
	for _, option := range options {
		if option != nil {
			option(server)
		}
	}
	routes := newClassifiedServeMux()
	routes.InternalFunc("GET /health", server.health)
	routes.InternalFunc("GET /ready", server.ready)
	routes.InternalFunc("GET /metrics", server.prometheusMetrics)
	routes.InternalFunc("GET /v1/platform/profile", server.getPlatformProfile)
	routes.InternalFunc("PUT /v1/platform/routing-authority/execution-targets/{executionTargetID}/observations", server.publishPlatformRoutingAuthority)
	routes.InternalFunc("POST /v1/auth/dev-login", server.devLogin)
	routes.InternalFunc("GET /v1/auth/sso/connections", server.listPublicIdentityConnections)
	routes.InternalFunc("GET /v1/auth/sso/{connectionID}/start", server.startSSO)
	routes.InternalFunc("GET /v1/auth/sso/{connectionID}/metadata", server.samlMetadata)
	routes.InternalFunc("GET /v1/auth/sso/{connectionID}/callback", server.completeSSO)
	routes.InternalFunc("POST /v1/auth/sso/{connectionID}/callback", server.completeSSO)
	routes.InternalFunc("POST /v1/desktop-enrollments/redeem", server.redeemDesktopEnrollment)
	routes.InternalFunc("POST /v1/workers/register", server.registerWorker)
	routes.Internal("POST /v1/workers/heartbeat", server.requireWorker(http.HandlerFunc(server.workerHeartbeat)))
	routes.Internal("POST /v1/workers/storage-scrubs/claim", server.requireWorker(http.HandlerFunc(server.claimWorkerStorageScrub)))
	routes.Internal("POST /v1/workers/storage-scrubs/{scrubID}/acknowledged", server.requireWorker(http.HandlerFunc(server.acknowledgeWorkerStorageScrub)))
	routes.Internal("POST /v1/workers/storage-scrubs/{scrubID}/failed", server.requireWorker(http.HandlerFunc(server.failWorkerStorageScrub)))
	routes.Internal("POST /v1/workers/workspace-cleanups/claim", server.requireWorker(http.HandlerFunc(server.claimWorkspaceCleanup)))
	routes.Internal("POST /v1/workers/workspace-cleanups/{cleanupID}/renew", server.requireWorker(http.HandlerFunc(server.renewWorkspaceCleanup)))
	routes.Internal("POST /v1/workers/workspace-cleanups/{cleanupID}/started", server.requireWorker(http.HandlerFunc(server.startWorkspaceCleanup)))
	routes.Internal("POST /v1/workers/workspace-cleanups/{cleanupID}/acknowledged", server.requireWorker(http.HandlerFunc(server.acknowledgeWorkspaceCleanup)))
	routes.Internal("POST /v1/workers/workspace-cleanups/{cleanupID}/failed", server.requireWorker(http.HandlerFunc(server.failWorkspaceCleanup)))
	routes.Internal("POST /v1/workers/workspace-cleanups/{cleanupID}/release", server.requireWorker(http.HandlerFunc(server.releaseWorkspaceCleanup)))
	routes.Internal("POST /v1/workers/executions/claim", server.requireWorker(http.HandlerFunc(server.claimExecution)))
	routes.Internal("POST /v1/workers/executions/{executionID}/renew", server.requireWorker(http.HandlerFunc(server.renewExecutionLease)))
	routes.Internal("POST /v1/workers/executions/{executionID}/start", server.requireWorker(http.HandlerFunc(server.startExecution)))
	routes.Internal("POST /v1/workers/executions/{executionID}/workspace/ready", server.requireWorker(http.HandlerFunc(server.markWorkspaceReady)))
	routes.Internal("POST /v1/workers/executions/{executionID}/workspace/dirty", server.requireWorker(http.HandlerFunc(server.markWorkspaceDirty)))
	routes.Internal("POST /v1/workers/executions/{executionID}/workspace/failed", server.requireWorker(http.HandlerFunc(server.markWorkspaceFailed)))
	routes.Internal("POST /v1/workers/executions/{executionID}/workspace/checkpoints", server.requireWorker(http.HandlerFunc(server.createWorkspaceCheckpoint)))
	routes.Internal("POST /v1/workers/executions/{executionID}/workspace/checkpoints/{checkpointID}/ready", server.requireWorker(http.HandlerFunc(server.markWorkspaceCheckpointReady)))
	routes.Internal("POST /v1/workers/executions/{executionID}/workspace/checkpoints/{checkpointID}/failed", server.requireWorker(http.HandlerFunc(server.markWorkspaceCheckpointFailed)))
	routes.Internal("POST /v1/workers/executions/{executionID}/complete", server.requireWorker(http.HandlerFunc(server.completeExecution)))
	routes.Internal("POST /v1/workers/executions/{executionID}/fail", server.requireWorker(http.HandlerFunc(server.failExecution)))
	routes.Internal("POST /v1/workers/executions/{executionID}/release", server.requireWorker(http.HandlerFunc(server.releaseExecution)))
	routes.Internal("POST /v1/workers/executions/{executionID}/resource-directives/pull", server.requireWorker(http.HandlerFunc(server.pullExecutionResourceDirective)))
	routes.Internal("POST /v1/workers/executions/{executionID}/resource-suspend/quiesced", server.requireWorker(http.HandlerFunc(server.markExecutionResourceSuspendQuiesced)))
	routes.Internal("POST /v1/workers/executions/{executionID}/resource-suspend/checkpoint-ready", server.requireWorker(http.HandlerFunc(server.markExecutionResourceSuspendCheckpointReady)))
	routes.Internal("POST /v1/workers/executions/{executionID}/resource-suspend/complete", server.requireWorker(http.HandlerFunc(server.completeExecutionResourceSuspend)))
	routes.Internal("POST /v1/workers/executions/{executionID}/resource-suspend/abort", server.requireWorker(http.HandlerFunc(server.abortExecutionResourceSuspend)))
	routes.Internal("POST /v1/workers/executions/{executionID}/events", server.requireWorker(http.HandlerFunc(server.appendRuntimeEvent)))
	routes.Internal("POST /v1/workers/executions/{executionID}/usage", server.requireWorker(http.HandlerFunc(server.reportExecutionUsage)))
	routes.Internal("POST /v1/workers/executions/{executionID}/interaction-resolutions/pull", server.requireWorker(http.HandlerFunc(server.pullInteractionResolutions)))
	routes.Internal("POST /v1/workers/executions/{executionID}/interaction-resolutions/{interactionID}/delivered", server.requireWorker(http.HandlerFunc(server.markInteractionResolutionDelivered)))
	routes.Internal("POST /v1/workers/executions/{executionID}/interaction-resolutions/{interactionID}/acknowledged", server.requireWorker(http.HandlerFunc(server.acknowledgeInteractionResolution)))
	routes.Internal("POST /v1/workers/executions/{executionID}/control-updates/pull", server.requireWorker(http.HandlerFunc(server.pullControlUpdates)))
	routes.Internal("POST /v1/workers/executions/{executionID}/control-commands/pull", server.requireWorker(http.HandlerFunc(server.pullControlCommands)))
	routes.Internal("POST /v1/workers/executions/{executionID}/control-commands/{controlCommandID}/delivered", server.requireWorker(http.HandlerFunc(server.markControlCommandDelivered)))
	routes.Internal("POST /v1/workers/executions/{executionID}/control-commands/{controlCommandID}/acknowledged", server.requireWorker(http.HandlerFunc(server.acknowledgeControlCommand)))
	routes.Internal("POST /v1/workers/executions/{executionID}/artifacts", server.requireWorker(http.HandlerFunc(server.createWorkerArtifact)))
	routes.Internal("POST /v1/workers/executions/{executionID}/artifacts/{artifactID}/complete", server.requireWorker(http.HandlerFunc(server.completeWorkerArtifact)))
	routes.Internal("POST /v1/workers/executions/{executionID}/workspace/checkpoints/{checkpointID}/artifact/download", server.requireWorker(http.HandlerFunc(server.downloadWorkerCheckpointArtifact)))
	routes.Internal("POST /v1/workers/executions/{executionID}/memory-revisions/{revisionID}/artifact/download", server.requireWorker(http.HandlerFunc(server.downloadWorkerMemoryArtifact)))
	routes.Internal("POST /v1/workers/executions/{executionID}/credentials/{credentialID}/resolve", server.requireWorker(http.HandlerFunc(server.resolveExecutionCredential)))
	routes.Internal("POST /v1/workers/executions/{executionID}/provider-credential-grants/{grantID}/resolve", server.requireWorker(http.HandlerFunc(server.resolveExecutionProviderCredentialGrant)))
	routes.Internal("POST /v1/workers/executions/{executionID}/credential-grants/{grantID}/resolve", server.requireWorker(http.HandlerFunc(server.resolveExecutionCredentialGrant)))
	routes.Internal("GET /v1/auth/session", server.requireAuth(http.HandlerFunc(server.getSession)))
	routes.Internal("POST /v1/auth/logout", server.requireAuth(http.HandlerFunc(server.logout)))
	routes.Internal("PUT /v1/auth/active-tenant", server.requireAuth(http.HandlerFunc(server.setActiveTenant)))
	routes.Internal("POST /v1/desktop-sessions/rotate", server.requireAuth(http.HandlerFunc(server.rotateDesktopSession)))
	routes.Internal("POST /v1/desktop/disconnect", server.requireAuth(http.HandlerFunc(server.disconnectDesktop)))
	routes.Internal("POST /v1/invitations/{token}/accept", server.requireAuth(http.HandlerFunc(server.acceptInvitation)))

	routes.Internal("GET /v1/tenants", server.requireAuth(http.HandlerFunc(server.listTenants)))
	routes.Internal("POST /v1/tenants", server.requireAuth(http.HandlerFunc(server.createTenant)))
	routes.Internal("GET /v1/tenants/deletion-requests", server.requireAuth(http.HandlerFunc(server.listTenantDeletionRequests)))
	routes.Internal("GET /v1/tenants/{tenantID}", server.requireAuth(http.HandlerFunc(server.getTenant)))
	routes.Internal("PATCH /v1/tenants/{tenantID}", server.requireAuth(http.HandlerFunc(server.updateTenant)))
	routes.Internal("POST /v1/tenants/{tenantID}/lifecycle-transitions", server.requireAuth(http.HandlerFunc(server.transitionTenant)))
	routes.Internal("POST /v1/tenants/{tenantID}/deletion-requests", server.requireAuth(http.HandlerFunc(server.requestTenantDeletion)))
	routes.Internal("POST /v1/tenants/{tenantID}/restore", server.requireAuth(http.HandlerFunc(server.restoreTenant)))
	routes.Internal("DELETE /v1/tenants/{tenantID}", server.requireAuth(http.HandlerFunc(server.deleteTenant)))
	routes.Internal("GET /v1/tenants/{tenantID}/members", server.requireAuth(http.HandlerFunc(server.listTenantMembers)))
	routes.Internal("POST /v1/tenants/{tenantID}/invitations", server.requireAuth(http.HandlerFunc(server.inviteTenantMember)))
	routes.Internal("PATCH /v1/tenants/{tenantID}/members/{userID}", server.requireAuth(http.HandlerFunc(server.updateTenantMember)))
	routes.Internal("DELETE /v1/tenants/{tenantID}/members/{userID}", server.requireAuth(http.HandlerFunc(server.removeTenantMember)))
	routes.Internal("POST /v1/tenants/{tenantID}/members/{userID}/revoke-sessions", server.requireAuth(http.HandlerFunc(server.revokeTenantUserSessions)))
	routes.Internal("GET /v1/tenants/{tenantID}/audit-logs", server.requireAuth(http.HandlerFunc(server.listAuditLogs)))
	routes.Internal("GET /v1/tenants/{tenantID}/audit-logs/export", server.requireAuth(http.HandlerFunc(server.exportAuditLogs)))
	routes.Internal("GET /v1/tenants/{tenantID}/outbox-messages", server.requireAuth(http.HandlerFunc(server.listOutboxMessages)))
	routes.Internal("POST /v1/tenants/{tenantID}/outbox-messages/{messageID}/replay", server.requireAuth(http.HandlerFunc(server.replayOutboxMessage)))
	routes.Internal("GET /v1/tenants/{tenantID}/developer-webhooks", server.requireAuth(http.HandlerFunc(server.listDeveloperWebhooks)))
	routes.Internal("POST /v1/tenants/{tenantID}/developer-webhooks", server.requireAuth(http.HandlerFunc(server.createDeveloperWebhook)))
	routes.Internal("POST /v1/tenants/{tenantID}/developer-webhooks/{webhookID}/rotate-secret", server.requireAuth(http.HandlerFunc(server.rotateDeveloperWebhookSecret)))
	routes.Internal("POST /v1/tenants/{tenantID}/developer-webhooks/{webhookID}/enable", server.requireAuth(http.HandlerFunc(server.enableDeveloperWebhook)))
	routes.Internal("POST /v1/tenants/{tenantID}/developer-webhooks/{webhookID}/disable", server.requireAuth(http.HandlerFunc(server.disableDeveloperWebhook)))
	routes.Internal("POST /v1/tenants/{tenantID}/developer-webhooks/{webhookID}/revoke", server.requireAuth(http.HandlerFunc(server.revokeDeveloperWebhook)))
	routes.Internal("GET /v1/tenants/{tenantID}/developer-webhooks/{webhookID}/deliveries", server.requireAuth(http.HandlerFunc(server.listDeveloperWebhookDeliveries)))
	routes.Internal("POST /v1/tenants/{tenantID}/developer-webhooks/{webhookID}/deliveries/{deliveryID}/replay", server.requireAuth(http.HandlerFunc(server.replayDeveloperWebhookDelivery)))
	routes.Internal("GET /v1/tenants/{tenantID}/execution-targets", server.requireAuth(http.HandlerFunc(server.listExecutionTargets)))
	routes.Internal("GET /v1/tenants/{tenantID}/execution-scheduling-policy", server.requireAuth(http.HandlerFunc(server.getTenantExecutionSchedulingPolicy)))
	routes.Internal("PUT /v1/tenants/{tenantID}/execution-scheduling-policy", server.requireAuth(http.HandlerFunc(server.putTenantExecutionSchedulingPolicy)))
	routes.Internal("GET /v1/tenants/{tenantID}/data-residency-statement", server.requireAuth(http.HandlerFunc(server.getTenantDataResidencyStatement)))
	routes.Internal("GET /v1/tenants/{tenantID}/workers", server.requireAuth(http.HandlerFunc(server.listTenantWorkers)))
	routes.Internal("POST /v1/tenants/{tenantID}/workers/{workerID}/revoke", server.requireAuth(http.HandlerFunc(server.revokeTenantWorker)))
	routes.Internal("GET /v1/tenants/{tenantID}/worker-manifests", server.requireAuth(http.HandlerFunc(server.listWorkerManifests)))
	routes.Internal("GET /v1/tenants/{tenantID}/execution-targets/{executionTargetID}/worker-releases", server.requireAuth(http.HandlerFunc(server.listWorkerReleases)))
	routes.Internal("POST /v1/tenants/{tenantID}/execution-targets/{executionTargetID}/worker-releases", server.requireAuth(http.HandlerFunc(server.createWorkerRelease)))
	routes.Internal("POST /v1/tenants/{tenantID}/execution-targets/{executionTargetID}/worker-releases/{releaseRevisionID}/canary", server.requireAuth(http.HandlerFunc(server.startWorkerReleaseCanary)))
	routes.Internal("POST /v1/tenants/{tenantID}/execution-targets/{executionTargetID}/worker-releases/{releaseRevisionID}/promote", server.requireAuth(http.HandlerFunc(server.promoteWorkerRelease)))
	routes.Internal("POST /v1/tenants/{tenantID}/execution-targets/{executionTargetID}/worker-releases/{releaseRevisionID}/rollback", server.requireAuth(http.HandlerFunc(server.rollbackWorkerRelease)))
	routes.Internal("GET /v1/tenants/{tenantID}/execution-targets/{executionTargetID}/worker-pools", server.requireAuth(http.HandlerFunc(server.listWorkerPools)))
	routes.Internal("POST /v1/tenants/{tenantID}/execution-targets/{executionTargetID}/worker-pools", server.requireAuth(http.HandlerFunc(server.createWorkerPool)))
	routes.Internal("PATCH /v1/tenants/{tenantID}/execution-targets/{executionTargetID}/worker-pools/{workerPoolID}", server.requireAuth(http.HandlerFunc(server.updateWorkerPool)))
	routes.Internal("GET /v1/tenants/{tenantID}/execution-targets/{executionTargetID}/worker-pools/{workerPoolID}/autoscaling", server.requireAuth(http.HandlerFunc(server.getWorkerPoolAutoscaling)))
	routes.Internal("PUT /v1/tenants/{tenantID}/execution-targets/{executionTargetID}/worker-pools/{workerPoolID}/autoscaling", server.requireAuth(http.HandlerFunc(server.putWorkerPoolAutoscaling)))
	routes.Internal("PUT /v1/tenants/{tenantID}/execution-targets/{executionTargetID}/placement-policy", server.requireAuth(http.HandlerFunc(server.updateExecutionPlacementPolicy)))
	routes.Internal("GET /v1/tenants/{tenantID}/execution-target-groups", server.requireAuth(http.HandlerFunc(server.listExecutionTargetGroups)))
	routes.Internal("POST /v1/tenants/{tenantID}/execution-target-groups", server.requireAuth(http.HandlerFunc(server.createExecutionTargetGroup)))
	routes.Internal("POST /v1/tenants/{tenantID}/execution-target-groups/{targetGroupID}/members", server.requireAuth(http.HandlerFunc(server.addExecutionTargetGroupMember)))
	routes.Internal("PATCH /v1/tenants/{tenantID}/execution-target-groups/{targetGroupID}/members/{targetGroupMemberID}", server.requireAuth(http.HandlerFunc(server.updateExecutionTargetGroupMember)))
	routes.Internal("PUT /v1/tenants/{tenantID}/execution-targets/{executionTargetID}/health-observation", server.requireAuth(http.HandlerFunc(server.observeExecutionTargetHealth)))
	routes.Internal("GET /v1/tenants/{tenantID}/location-outages", server.requireAuth(http.HandlerFunc(server.listLocationOutages)))
	routes.Internal("PUT /v1/tenants/{tenantID}/location-outages", server.requireAuth(http.HandlerFunc(server.observeLocationOutage)))
	routes.PublicBeta("POST /v1/tenants/{tenantID}/execution-targets", server.requireDeveloperAuth(http.HandlerFunc(server.createExecutionTarget)))
	routes.PublicBeta("GET /v1/tenants/{tenantID}/execution-targets/{executionTargetID}", server.requireDeveloperAuth(http.HandlerFunc(server.getExecutionTarget)))
	routes.PublicBeta("POST /v1/tenants/{tenantID}/execution-targets/{executionTargetID}/provisioning-operations", server.requireDeveloperAuth(http.HandlerFunc(server.createExecutionTargetProvisioningOperation)))
	routes.PublicBeta("GET /v1/tenants/{tenantID}/execution-targets/{executionTargetID}/provisioning-operations/{provisioningOperationID}", server.requireDeveloperAuth(http.HandlerFunc(server.getExecutionTargetProvisioningOperation)))
	routes.Internal("PATCH /v1/tenants/{tenantID}/execution-targets/{executionTargetID}/provider-policy", server.requireAuth(http.HandlerFunc(server.updateExecutionTargetProviderPolicy)))
	routes.Internal("PATCH /v1/tenants/{tenantID}/execution-targets/{executionTargetID}/process-containment-policy", server.requireAuth(http.HandlerFunc(server.updateExecutionTargetProcessContainmentPolicy)))
	routes.Internal("PUT /v1/tenants/{tenantID}/execution-targets/{executionTargetID}/runtime-isolation-policy", server.requireAuth(http.HandlerFunc(server.updateExecutionTargetRuntimeIsolationPolicy)))
	routes.Internal("POST /v1/tenants/{tenantID}/execution-targets/{executionTargetID}/kubernetes/disable", server.requireAuth(http.HandlerFunc(server.disableManagedKubernetesExecutionTarget)))
	routes.Internal("POST /v1/tenants/{tenantID}/execution-targets/{executionTargetID}/ssh/install", server.requireAuth(http.HandlerFunc(server.installSSHExecutionTarget)))
	routes.Internal("POST /v1/tenants/{tenantID}/execution-targets/{executionTargetID}/ssh/upgrade", server.requireAuth(http.HandlerFunc(server.upgradeSSHExecutionTarget)))
	routes.Internal("POST /v1/tenants/{tenantID}/execution-targets/{executionTargetID}/ssh/revoke", server.requireAuth(http.HandlerFunc(server.revokeSSHExecutionTarget)))
	routes.Internal("GET /v1/tenants/{tenantID}/quota", server.requireAuth(http.HandlerFunc(server.getTenantQuota)))
	routes.Internal("GET /v1/tenants/{tenantID}/entitlements", server.requireAuth(http.HandlerFunc(server.getTenantEntitlements)))
	routes.Internal("GET /v1/tenants/{tenantID}/usage", server.requireAuth(http.HandlerFunc(server.getTenantUsage)))
	routes.Internal("GET /v1/tenants/{tenantID}/usage/export.json", server.requireAuth(http.HandlerFunc(server.exportTenantUsageJSON)))
	routes.Internal("GET /v1/tenants/{tenantID}/support-diagnostic.json", server.requireAuth(http.HandlerFunc(server.exportTenantSupportDiagnosticJSON)))
	routes.Internal("GET /v1/tenants/{tenantID}/support-policy", server.requireAuth(http.HandlerFunc(server.getTenantSupportPolicy)))
	routes.Internal("PUT /v1/tenants/{tenantID}/support-policy", server.requireAuth(http.HandlerFunc(server.updateTenantSupportPolicy)))
	routes.Internal("GET /v1/tenants/{tenantID}/support-access", server.requireAuth(http.HandlerFunc(server.listTenantSupportAccess)))
	routes.Internal("POST /v1/tenants/{tenantID}/support-access/{grantID}/revoke", server.requireAuth(http.HandlerFunc(server.revokeTenantSupportAccess)))
	routes.Internal("GET /v1/platform/support-access", server.requireAuth(http.HandlerFunc(server.listPlatformSupportAccess)))
	routes.Internal("GET /v1/platform/tenants", server.requireAuth(http.HandlerFunc(server.listPlatformTenants)))
	routes.Internal("POST /v1/platform/tenants", server.requireAuth(http.HandlerFunc(server.provisionPlatformTenant)))
	routes.Internal("GET /v1/platform/tenants/{tenantID}/entitlements", server.requireAuth(http.HandlerFunc(server.getPlatformTenantEntitlements)))
	routes.Internal("PUT /v1/platform/tenants/{tenantID}/entitlement-profile", server.requireAuth(http.HandlerFunc(server.assignPlatformTenantEntitlementProfile)))
	routes.Internal("GET /v1/platform/tenants/{tenantID}/desktop-access", server.requireAuth(http.HandlerFunc(server.getPlatformDesktopAccess)))
	routes.Internal("POST /v1/platform/tenants/{tenantID}/desktop-enrollments", server.requireAuth(http.HandlerFunc(server.issuePlatformDesktopEnrollment)))
	routes.Internal("POST /v1/platform/desktop-enrollments/{enrollmentID}/opened", server.requireAuth(http.HandlerFunc(server.markPlatformDesktopEnrollmentOpened)))
	routes.Internal("POST /v1/platform/desktop-enrollments/{enrollmentID}/revoke", server.requireAuth(http.HandlerFunc(server.revokePlatformDesktopEnrollment)))
	routes.Internal("POST /v1/platform/desktop-devices/{deviceID}/revoke", server.requireAuth(http.HandlerFunc(server.revokePlatformDesktopDevice)))
	routes.Internal("POST /v1/platform/support-access/requests", server.requireAuth(http.HandlerFunc(server.requestPlatformSupportAccess)))
	routes.Internal("POST /v1/platform/support-access/{grantID}/approve", server.requireAuth(http.HandlerFunc(server.approvePlatformSupportAccess)))
	routes.Internal("POST /v1/platform/support-access/{grantID}/deny", server.requireAuth(http.HandlerFunc(server.denyPlatformSupportAccess)))
	routes.Internal("POST /v1/platform/support-access/{grantID}/revoke", server.requireAuth(http.HandlerFunc(server.revokePlatformSupportAccess)))
	routes.Internal("GET /v1/platform/release-candidates", server.requireAuth(http.HandlerFunc(server.listPlatformReleaseCandidates)))
	routes.Internal("POST /v1/platform/release-candidates", server.requireAuth(http.HandlerFunc(server.createPlatformReleaseCandidate)))
	routes.Internal("GET /v1/platform/release-candidates/{candidateRecordID}/readiness", server.requireAuth(http.HandlerFunc(server.getPlatformReleaseCandidateReadiness)))
	routes.Internal("POST /v1/platform/release-candidates/{candidateRecordID}/approvals", server.requireAuth(http.HandlerFunc(server.recordPlatformReleaseApproval)))
	routes.Internal("POST /v1/platform/release-candidates/{candidateRecordID}/final-review", server.requireAuth(http.HandlerFunc(server.recordPlatformReleaseFinalReview)))
	routes.Internal("POST /v1/platform/release-candidates/{candidateRecordID}/transitions", server.requireAuth(http.HandlerFunc(server.transitionPlatformReleaseCandidate)))
	routes.Internal("GET /v1/platform/incidents", server.requireAuth(http.HandlerFunc(server.listPlatformIncidents)))
	routes.Internal("POST /v1/platform/incidents", server.requireAuth(http.HandlerFunc(server.createPlatformIncident)))
	routes.Internal("POST /v1/platform/incidents/{incidentID}/status-board", server.requireAuth(http.HandlerFunc(server.bindPlatformIncidentStatusBoard)))
	routes.Internal("POST /v1/platform/incidents/{incidentID}/internal-updates", server.requireAuth(http.HandlerFunc(server.addPlatformIncidentInternalUpdate)))
	routes.Internal("POST /v1/platform/incidents/{incidentID}/resolution-approval", server.requireAuth(http.HandlerFunc(server.recordPlatformIncidentResolutionApproval)))
	routes.Internal("POST /v1/platform/incidents/{incidentID}/transitions", server.requireAuth(http.HandlerFunc(server.transitionPlatformIncident)))
	routes.Internal("GET /v1/platform/incident-exercises", server.requireAuth(http.HandlerFunc(server.listPlatformIncidentExercises)))
	routes.Internal("POST /v1/platform/incident-exercises", server.requireAuth(http.HandlerFunc(server.importPlatformIncidentExercise)))
	routes.Internal("POST /v1/platform/incident-exercises/{incidentExerciseID}/approvals", server.requireAuth(http.HandlerFunc(server.recordPlatformIncidentExerciseApproval)))
	routes.Internal("GET /v1/platform/operations-exercises", server.requireAuth(http.HandlerFunc(server.listPlatformOperationsExercises)))
	routes.Internal("POST /v1/platform/operations-exercises", server.requireAuth(http.HandlerFunc(server.importPlatformOperationsExercise)))
	routes.Internal("POST /v1/platform/operations-exercises/{operationsExerciseID}/approvals", server.requireAuth(http.HandlerFunc(server.recordPlatformOperationsExerciseApproval)))
	routes.Internal("GET /v1/platform/internal-cost-reviews", server.requireAuth(http.HandlerFunc(server.listPlatformInternalCostReviews)))
	routes.Internal("POST /v1/platform/internal-cost-reviews", server.requireAuth(http.HandlerFunc(server.importPlatformInternalCostReview)))
	routes.Internal("POST /v1/platform/internal-cost-reviews/{internalCostReviewID}/approvals", server.requireAuth(http.HandlerFunc(server.recordPlatformInternalCostApproval)))
	routes.Internal("GET /v1/platform/slo-windows", server.requireAuth(http.HandlerFunc(server.listPlatformSLOWindows)))
	routes.Internal("POST /v1/platform/slo-windows", server.requireAuth(http.HandlerFunc(server.importPlatformSLOWindow)))
	routes.Internal("POST /v1/platform/slo-windows/{sloWindowRecordID}/approvals", server.requireAuth(http.HandlerFunc(server.recordPlatformSLOApproval)))
	routes.Internal("GET /v1/platform/recovery-drills", server.requireAuth(http.HandlerFunc(server.listPlatformRecoveryDrills)))
	routes.Internal("POST /v1/platform/recovery-drills", server.requireAuth(http.HandlerFunc(server.importPlatformRecoveryDrill)))
	routes.Internal("POST /v1/platform/recovery-drills/{recoveryDrillRecordID}/approvals", server.requireAuth(http.HandlerFunc(server.recordPlatformRecoveryApproval)))
	routes.Internal("GET /v1/platform/penetration-engagements", server.requireAuth(http.HandlerFunc(server.listPlatformPenetrationEngagements)))
	routes.Internal("POST /v1/platform/penetration-engagements", server.requireAuth(http.HandlerFunc(server.importPlatformPenetrationEngagement)))
	routes.Internal("POST /v1/platform/penetration-engagements/{penetrationEngagementID}/approvals", server.requireAuth(http.HandlerFunc(server.recordPlatformPenetrationApproval)))
	routes.Internal("GET /v1/platform/capacity-runs", server.requireAuth(http.HandlerFunc(server.listPlatformCapacityRuns)))
	routes.Internal("POST /v1/platform/capacity-runs", server.requireAuth(http.HandlerFunc(server.importPlatformCapacityRun)))
	routes.Internal("POST /v1/platform/capacity-runs/{capacityRunID}/approvals", server.requireAuth(http.HandlerFunc(server.recordPlatformCapacityApproval)))
	routes.Internal("GET /v1/platform/compliance-programs", server.requireAuth(http.HandlerFunc(server.listPlatformCompliancePrograms)))
	routes.Internal("POST /v1/platform/compliance-programs", server.requireAuth(http.HandlerFunc(server.createPlatformComplianceProgram)))
	routes.Internal("POST /v1/platform/compliance-programs/{programID}/controls", server.requireAuth(http.HandlerFunc(server.createPlatformComplianceControl)))
	routes.Internal("POST /v1/platform/compliance-programs/{programID}/evidence", server.requireAuth(http.HandlerFunc(server.submitPlatformComplianceEvidence)))
	routes.Internal("POST /v1/platform/compliance-programs/{programID}/evidence/{evidenceRecordID}/review", server.requireAuth(http.HandlerFunc(server.reviewPlatformComplianceEvidence)))
	routes.Internal("POST /v1/platform/compliance-programs/{programID}/decisions", server.requireAuth(http.HandlerFunc(server.recordPlatformComplianceDecision)))
	routes.Internal("POST /v1/platform/compliance-programs/{programID}/transitions", server.requireAuth(http.HandlerFunc(server.transitionPlatformComplianceProgram)))
	routes.Internal("GET /v1/platform/provider-commercial-authorizations", server.requireAuth(http.HandlerFunc(server.listPlatformProviderCommercialAuthorizations)))
	routes.Internal("POST /v1/platform/provider-commercial-authorizations", server.requireAuth(http.HandlerFunc(server.createPlatformProviderCommercialAuthorization)))
	routes.Internal("POST /v1/platform/provider-commercial-authorizations/{authorizationID}/approvals", server.requireAuth(http.HandlerFunc(server.recordPlatformProviderCommercialApproval)))
	routes.Internal("POST /v1/platform/provider-commercial-authorizations/{authorizationID}/transitions", server.requireAuth(http.HandlerFunc(server.transitionPlatformProviderCommercialAuthorization)))
	routes.Internal("GET /v1/platform/governance-authorities", server.requireAuth(http.HandlerFunc(server.listPlatformGovernanceAuthorities)))
	routes.Internal("POST /v1/platform/governance-authorities", server.requireAuth(http.HandlerFunc(server.createPlatformGovernanceAuthority)))
	routes.Internal("POST /v1/platform/governance-authorities/{grantID}/revoke", server.requireAuth(http.HandlerFunc(server.revokePlatformGovernanceAuthority)))
	routes.Internal("PUT /v1/tenants/{tenantID}/quota", server.requireAuth(http.HandlerFunc(server.putTenantQuota)))
	routes.Internal("GET /v1/tenants/{tenantID}/execution-quotas/{scopeKind}/{scopeID}", server.requireAuth(http.HandlerFunc(server.getScopedExecutionQuota)))
	routes.Internal("PUT /v1/tenants/{tenantID}/execution-quotas/{scopeKind}/{scopeID}", server.requireAuth(http.HandlerFunc(server.putScopedExecutionQuota)))
	routes.Internal("GET /v1/tenants/{tenantID}/cost-accounting/tariffs", server.requireAuth(http.HandlerFunc(server.listBillingTariffs)))
	routes.Internal("POST /v1/tenants/{tenantID}/cost-accounting/tariffs", server.requireAuth(http.HandlerFunc(server.createBillingTariff)))
	routes.Internal("GET /v1/tenants/{tenantID}/cost-accounting/shared-targets/{executionTargetID}/ledger-coverage", server.requireAuth(http.HandlerFunc(server.getBillingSharedTargetLedgerCoverage)))
	routes.Internal("POST /v1/tenants/{tenantID}/cost-accounting/shared-targets/{executionTargetID}/ledger-coverage", server.requireAuth(http.HandlerFunc(server.sealBillingSharedTargetLedgerCoverage)))
	routes.Internal("POST /v1/tenants/{tenantID}/cost-accounting/shared-targets/{executionTargetID}/allocations:sweep", server.requireAuth(http.HandlerFunc(server.sweepBillingSharedTargetAllocations)))
	routes.Internal("POST /v1/tenants/{tenantID}/cost-accounting/shared-targets/{executionTargetID}/actual-invoices/{invoiceImportID}/allocations", server.requireAuth(http.HandlerFunc(server.allocateBillingSharedTargetActualInvoice)))
	routes.Internal("POST /v1/tenants/{tenantID}/cost-accounting/imports/{provider}/{externalImportID}", server.requireAuth(http.HandlerFunc(server.triggerBillingImport)))
	routes.Internal("POST /v1/tenants/{tenantID}/cost-accounting/imports/{importID}/reconcile", server.requireAuth(http.HandlerFunc(server.reconcileBillingImport)))
	routes.Internal("GET /v1/tenants/{tenantID}/cost-accounting/report", server.requireAuth(http.HandlerFunc(server.getInternalCostAllocationReport)))
	routes.Internal("GET /v1/tenants/{tenantID}/cost-accounting/export.csv", server.requireAuth(http.HandlerFunc(server.exportInternalCostAllocationCSV)))
	routes.Internal("PUT /v1/tenants/{tenantID}/cost-accounting/projects/{projectID}", server.requireAuth(http.HandlerFunc(server.putProjectCostAllocation)))
	routes.Internal("GET /v1/tenants/{tenantID}/retention-policy", server.requireAuth(http.HandlerFunc(server.getRetentionPolicy)))
	routes.Internal("PUT /v1/tenants/{tenantID}/retention-policy", server.requireAuth(http.HandlerFunc(server.putRetentionPolicy)))
	routes.Internal("GET /v1/tenants/{tenantID}/legal-holds", server.requireAuth(http.HandlerFunc(server.listLegalHolds)))
	routes.Internal("POST /v1/tenants/{tenantID}/legal-holds", server.requireAuth(http.HandlerFunc(server.createLegalHold)))
	routes.Internal("POST /v1/tenants/{tenantID}/legal-holds/{holdID}/release", server.requireAuth(http.HandlerFunc(server.releaseLegalHold)))
	routes.Internal("GET /v1/tenants/{tenantID}/privacy-requests", server.requireAuth(http.HandlerFunc(server.listPrivacyRequests)))
	routes.Internal("POST /v1/tenants/{tenantID}/privacy-requests", server.requireAuth(http.HandlerFunc(server.createPrivacyRequest)))
	routes.Internal("GET /v1/tenants/{tenantID}/privacy-requests/{privacyRequestID}", server.requireAuth(http.HandlerFunc(server.getPrivacyRequest)))
	routes.Internal("POST /v1/tenants/{tenantID}/privacy-requests/{privacyRequestID}/transitions", server.requireAuth(http.HandlerFunc(server.transitionPrivacyRequest)))
	routes.Internal("POST /v1/tenants/{tenantID}/privacy-requests/{privacyRequestID}/export", server.requireAuth(http.HandlerFunc(server.executePrivacyExport)))
	routes.Internal("POST /v1/tenants/{tenantID}/privacy-requests/{privacyRequestID}/erasure", server.requireAuth(http.HandlerFunc(server.executePrivacyErasure)))
	routes.Internal("POST /v1/tenants/{tenantID}/data-export", server.requireAuth(http.HandlerFunc(server.executeTenantDataExport)))
	routes.Internal("GET /v1/tenants/{tenantID}/resource-lifecycle-policy", server.requireAuth(http.HandlerFunc(server.getTenantResourceLifecyclePolicy)))
	routes.Internal("PUT /v1/tenants/{tenantID}/resource-lifecycle-policy", server.requireAuth(http.HandlerFunc(server.putTenantResourceLifecyclePolicy)))
	routes.Internal("POST /v1/tenants/{tenantID}/memory-revisions", server.requireAuth(http.HandlerFunc(server.publishMemoryRevision)))
	routes.Internal("GET /v1/tenants/{tenantID}/credentials", server.requireAuth(http.HandlerFunc(server.listCredentials)))
	routes.Internal("POST /v1/tenants/{tenantID}/credentials", server.requireAuth(http.HandlerFunc(server.createCredential)))
	routes.Internal("POST /v1/tenants/{tenantID}/credentials/{credentialID}/rotate", server.requireAuth(http.HandlerFunc(server.rotateCredential)))
	routes.Internal("POST /v1/tenants/{tenantID}/credentials/{credentialID}/revoke", server.requireAuth(http.HandlerFunc(server.revokeCredential)))
	routes.Internal("PUT /v1/tenants/{tenantID}/credentials/{credentialID}/auto-select", server.requireAuth(http.HandlerFunc(server.putCredentialAutoSelect)))
	routes.Internal("GET /v1/tenants/{tenantID}/provider-credential-scope-policy", server.requireAuth(http.HandlerFunc(server.getProviderCredentialScopePolicy)))
	routes.Internal("PUT /v1/tenants/{tenantID}/provider-credential-scope-policy", server.requireAuth(http.HandlerFunc(server.putProviderCredentialScopePolicy)))
	routes.Internal("GET /v1/tenants/{tenantID}/credential-bindings", server.requireAuth(http.HandlerFunc(server.listCredentialBindings)))
	routes.Internal("POST /v1/tenants/{tenantID}/credential-bindings", server.requireAuth(http.HandlerFunc(server.createCredentialBinding)))
	routes.Internal("POST /v1/tenants/{tenantID}/credential-bindings/{bindingID}/disable", server.requireAuth(http.HandlerFunc(server.disableCredentialBinding)))
	routes.Internal("GET /v1/tenants/{tenantID}/identity-connections", server.requireAuth(http.HandlerFunc(server.listIdentityConnections)))
	routes.Internal("POST /v1/tenants/{tenantID}/identity-connections", server.requireAuth(http.HandlerFunc(server.createIdentityConnection)))
	routes.Internal("POST /v1/tenants/{tenantID}/identity-connections/{connectionID}/disable", server.requireAuth(http.HandlerFunc(server.disableIdentityConnection)))
	routes.Internal("GET /v1/tenants/{tenantID}/identity-domains", server.requireAuth(http.HandlerFunc(server.listIdentityDomains)))
	routes.Internal("POST /v1/tenants/{tenantID}/identity-domains", server.requireAuth(http.HandlerFunc(server.createIdentityDomain)))
	routes.Internal("POST /v1/tenants/{tenantID}/identity-domains/{domainID}/verify", server.requireAuth(http.HandlerFunc(server.verifyIdentityDomain)))
	routes.Internal("POST /v1/tenants/{tenantID}/identity-domains/{domainID}/revoke", server.requireAuth(http.HandlerFunc(server.revokeIdentityDomain)))
	routes.Internal("GET /v1/tenants/{tenantID}/identity-policy", server.requireAuth(http.HandlerFunc(server.getTenantIdentityPolicy)))
	routes.Internal("PUT /v1/tenants/{tenantID}/identity-policy", server.requireAuth(http.HandlerFunc(server.putTenantIdentityPolicy)))
	routes.Internal("GET /v1/tenants/{tenantID}/identity-connections/{connectionID}/group-mappings", server.requireAuth(http.HandlerFunc(server.listIdentityGroupMappings)))
	routes.Internal("PUT /v1/tenants/{tenantID}/identity-connections/{connectionID}/group-mappings", server.requireAuth(http.HandlerFunc(server.replaceIdentityGroupMappings)))
	routes.Internal("GET /v1/tenants/{tenantID}/service-accounts", server.requireAuth(http.HandlerFunc(server.listServiceAccounts)))
	routes.Internal("POST /v1/tenants/{tenantID}/service-accounts", server.requireAuth(http.HandlerFunc(server.createServiceAccount)))
	routes.Internal("GET /v1/tenants/{tenantID}/service-accounts/{serviceAccountID}/usage", server.requireAuth(http.HandlerFunc(server.getServiceAccountAPIUsage)))
	routes.Internal("POST /v1/tenants/{tenantID}/service-accounts/{serviceAccountID}/rotate-token", server.requireAuth(http.HandlerFunc(server.rotateServiceAccountToken)))
	routes.Internal("POST /v1/tenants/{tenantID}/service-accounts/{serviceAccountID}/revoke", server.requireAuth(http.HandlerFunc(server.revokeServiceAccount)))

	routes.Internal("GET /scim/v2/ServiceProviderConfig", server.requireServiceAccount(http.HandlerFunc(server.scimServiceProviderConfig)))
	routes.Internal("GET /scim/v2/ResourceTypes", server.requireServiceAccount(http.HandlerFunc(server.scimResourceTypes)))
	routes.Internal("GET /scim/v2/Schemas", server.requireServiceAccount(http.HandlerFunc(server.scimSchemas)))
	routes.Internal("GET /scim/v2/Users", server.requireServiceAccount(http.HandlerFunc(server.scimListUsers)))
	routes.Internal("POST /scim/v2/Users", server.requireServiceAccount(http.HandlerFunc(server.scimCreateUser)))
	routes.Internal("GET /scim/v2/Users/{userID}", server.requireServiceAccount(http.HandlerFunc(server.scimGetUser)))
	routes.Internal("PUT /scim/v2/Users/{userID}", server.requireServiceAccount(http.HandlerFunc(server.scimReplaceUser)))
	routes.Internal("PATCH /scim/v2/Users/{userID}", server.requireServiceAccount(http.HandlerFunc(server.scimPatchUser)))
	routes.Internal("DELETE /scim/v2/Users/{userID}", server.requireServiceAccount(http.HandlerFunc(server.scimDeleteUser)))
	routes.Internal("GET /scim/v2/Groups", server.requireServiceAccount(http.HandlerFunc(server.scimListGroups)))
	routes.Internal("POST /scim/v2/Groups", server.requireServiceAccount(http.HandlerFunc(server.scimCreateGroup)))
	routes.Internal("GET /scim/v2/Groups/{groupID}", server.requireServiceAccount(http.HandlerFunc(server.scimGetGroup)))
	routes.Internal("PUT /scim/v2/Groups/{groupID}", server.requireServiceAccount(http.HandlerFunc(server.scimReplaceGroup)))
	routes.Internal("PATCH /scim/v2/Groups/{groupID}", server.requireServiceAccount(http.HandlerFunc(server.scimPatchGroup)))
	routes.Internal("DELETE /scim/v2/Groups/{groupID}", server.requireServiceAccount(http.HandlerFunc(server.scimDeleteGroup)))

	routes.Internal("GET /v1/tenants/{tenantID}/organizations", server.requireAuth(http.HandlerFunc(server.listOrganizations)))
	routes.Internal("POST /v1/tenants/{tenantID}/organizations", server.requireAuth(http.HandlerFunc(server.createOrganization)))
	routes.Internal("GET /v1/tenants/{tenantID}/organizations/{organizationID}", server.requireAuth(http.HandlerFunc(server.getOrganization)))
	routes.Internal("GET /v1/tenants/{tenantID}/organizations/{organizationID}/execution-scheduling-policy", server.requireAuth(http.HandlerFunc(server.getOrganizationExecutionSchedulingPolicy)))
	routes.Internal("PUT /v1/tenants/{tenantID}/organizations/{organizationID}/execution-scheduling-policy", server.requireAuth(http.HandlerFunc(server.putOrganizationExecutionSchedulingPolicy)))
	routes.Internal("PATCH /v1/tenants/{tenantID}/organizations/{organizationID}", server.requireAuth(http.HandlerFunc(server.updateOrganization)))
	routes.Internal("DELETE /v1/tenants/{tenantID}/organizations/{organizationID}", server.requireAuth(http.HandlerFunc(server.archiveOrganization)))
	routes.Internal("GET /v1/tenants/{tenantID}/organizations/{organizationID}/members", server.requireAuth(http.HandlerFunc(server.listOrganizationMembers)))
	routes.Internal("POST /v1/tenants/{tenantID}/organizations/{organizationID}/members", server.requireAuth(http.HandlerFunc(server.putOrganizationMember)))
	routes.Internal("PATCH /v1/tenants/{tenantID}/organizations/{organizationID}/members/{userID}", server.requireAuth(http.HandlerFunc(server.updateOrganizationMember)))
	routes.Internal("DELETE /v1/tenants/{tenantID}/organizations/{organizationID}/members/{userID}", server.requireAuth(http.HandlerFunc(server.removeOrganizationMember)))
	routes.PublicBeta("GET /v1/tenants/{tenantID}/organizations/{organizationID}/projects", server.requireDeveloperAuth(http.HandlerFunc(server.listProjects)))
	routes.PublicBeta("POST /v1/tenants/{tenantID}/organizations/{organizationID}/projects", server.requireDeveloperAuth(http.HandlerFunc(server.createProject)))

	routes.PublicBeta("GET /v1/projects/{projectID}", server.requireDeveloperAuth(http.HandlerFunc(server.getProject)))
	routes.PublicBeta("PATCH /v1/projects/{projectID}", server.requireDeveloperAuth(http.HandlerFunc(server.updateProject)))
	routes.PublicBeta("DELETE /v1/projects/{projectID}", server.requireDeveloperAuth(http.HandlerFunc(server.archiveProject)))
	routes.Internal("GET /v1/projects/{projectID}/resource-lifecycle-policy", server.requireAuth(http.HandlerFunc(server.getProjectResourceLifecyclePolicy)))
	routes.Internal("PUT /v1/projects/{projectID}/resource-lifecycle-policy", server.requireAuth(http.HandlerFunc(server.putProjectResourceLifecyclePolicy)))
	routes.PublicBeta("GET /v1/projects/{projectID}/provider-capabilities", server.requireDeveloperAuth(http.HandlerFunc(server.projectProviderCapabilities)))
	routes.PublicBeta("GET /v1/projects/{projectID}/sessions", server.requireDeveloperAuth(http.HandlerFunc(server.listProjectSessions)))
	routes.PublicBeta("POST /v1/projects/{projectID}/sessions", server.requireDeveloperAuth(http.HandlerFunc(server.createSession)))

	routes.PublicBeta("GET /v1/sessions/{sessionID}", server.requireDeveloperAuth(http.HandlerFunc(server.getAgentSession)))
	routes.PublicBeta("GET /v1/sessions/{sessionID}/usage", server.requireDeveloperAuth(http.HandlerFunc(server.getSessionUsage)))
	routes.PublicBeta("POST /v1/sessions/{sessionID}/model-switch", server.requireDeveloperAuth(http.HandlerFunc(server.switchSessionModel)))
	routes.PublicBeta("GET /v1/sessions/{sessionID}/provider-capabilities", server.requireDeveloperAuth(http.HandlerFunc(server.sessionProviderCapabilities)))
	routes.PublicBeta("GET /v1/sessions/{sessionID}/events", server.requireDeveloperAuth(http.HandlerFunc(server.listSessionEvents)))
	routes.PublicBeta("GET /v1/sessions/{sessionID}/events/stream", server.requireDeveloperAuth(http.HandlerFunc(server.streamSessionEvents)))
	routes.PublicBeta("GET /v1/sessions/{sessionID}/interactions", server.requireDeveloperAuth(http.HandlerFunc(server.listPendingSessionInteractions)))
	routes.Internal("GET /v1/sessions/{sessionID}/memory-references", server.requireAuth(http.HandlerFunc(server.listEffectiveMemoryReferences)))
	routes.PublicBeta("POST /v1/sessions/{sessionID}/turns", server.requireDeveloperAuth(http.HandlerFunc(server.createTurn)))
	routes.PublicBeta("POST /v1/sessions/{sessionID}/turns/active/steer", server.requireDeveloperAuth(http.HandlerFunc(server.steerActiveTurn)))
	routes.PublicBeta("POST /v1/sessions/{sessionID}/turns/active/interrupt", server.requireDeveloperAuth(http.HandlerFunc(server.interruptActiveTurn)))
	routes.PublicBeta("POST /v1/sessions/{sessionID}/turns/active/resume", server.requireDeveloperAuth(http.HandlerFunc(server.resumeActiveTurn)))
	routes.PublicBeta("POST /v1/sessions/{sessionID}/compact", server.requireDeveloperAuth(http.HandlerFunc(server.compactSession)))
	routes.PublicBeta("POST /v1/sessions/{sessionID}/reviews", server.requireDeveloperAuth(http.HandlerFunc(server.startSessionReview)))
	routes.PublicBeta("POST /v1/sessions/{sessionID}/rollback", server.requireDeveloperAuth(http.HandlerFunc(server.rollbackSession)))
	routes.PublicBeta("POST /v1/sessions/{sessionID}/fork", server.requireDeveloperAuth(http.HandlerFunc(server.forkSession)))
	routes.PublicBeta("POST /v1/sessions/{sessionID}/suspend", server.requireDeveloperAuth(http.HandlerFunc(server.suspendSession)))
	routes.PublicBeta("POST /v1/sessions/{sessionID}/resume", server.requireDeveloperAuth(http.HandlerFunc(server.resumeSession)))
	routes.PublicBeta("POST /v1/sessions/{sessionID}/archive", server.requireDeveloperAuth(http.HandlerFunc(server.archiveSession)))
	routes.PublicBeta("POST /v1/executions/{executionID}/cancel", server.requireDeveloperAuth(http.HandlerFunc(server.cancelExecution)))
	routes.PublicBeta("POST /v1/executions/{executionID}/resume", server.requireDeveloperAuth(http.HandlerFunc(server.resumeActiveTurnExecution)))
	routes.PublicBeta("GET /v1/executions/{executionID}/interactions", server.requireDeveloperAuth(http.HandlerFunc(server.listExecutionInteractions)))
	routes.Internal("GET /v1/executions/{executionID}/runtime-isolation", server.requireAuth(http.HandlerFunc(server.listExecutionRuntimeIsolationDecisions)))
	routes.PublicBeta("POST /v1/executions/{executionID}/approvals/{requestID}/resolve", server.requireDeveloperAuth(http.HandlerFunc(server.resolveExecutionApproval)))
	routes.PublicBeta("POST /v1/executions/{executionID}/user-input/{requestID}/resolve", server.requireDeveloperAuth(http.HandlerFunc(server.resolveExecutionUserInput)))
	routes.PublicBeta("GET /v1/sessions/{sessionID}/artifacts", server.requireDeveloperAuth(http.HandlerFunc(server.listArtifacts)))
	routes.PublicBeta("POST /v1/sessions/{sessionID}/artifacts", server.requireDeveloperAuth(http.HandlerFunc(server.createArtifact)))
	routes.PublicBeta("GET /v1/artifacts/{artifactID}", server.requireDeveloperAuth(http.HandlerFunc(server.getArtifact)))
	routes.PublicBeta("POST /v1/artifacts/{artifactID}/complete", server.requireDeveloperAuth(http.HandlerFunc(server.completeArtifact)))
	routes.PublicBeta("POST /v1/artifacts/{artifactID}/download", server.requireDeveloperAuth(http.HandlerFunc(server.downloadArtifact)))
	routes.PublicBeta("DELETE /v1/artifacts/{artifactID}", server.requireDeveloperAuth(http.HandlerFunc(server.deleteArtifact)))
	routes.InternalFunc("PUT /v1/artifact-content/{artifactID}", server.uploadArtifactContent)
	routes.InternalFunc("GET /v1/artifact-content/{artifactID}", server.downloadArtifactContent)

	server.apiRoutes = routes.manifest()
	server.handler = server.withRequestContext(server.observeRequests(server.recoverPanics(server.securityHeaders(routes))))
	return server, nil
}

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "profile": s.config.Platform.Profile,
	})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	type dependency struct {
		Status          string `json:"status"`
		Kind            string `json:"kind,omitempty"`
		LatencyMS       int64  `json:"latencyMs"`
		ExpectedVersion int64  `json:"expectedVersion,omitempty"`
		AppliedVersion  int64  `json:"appliedVersion,omitempty"`
	}
	checks := map[string]dependency{}
	databaseStarted := time.Now()
	sqlDB, err := s.db.DB()
	if err != nil {
		checks["database"] = dependency{Status: "unavailable", Kind: string(s.config.Platform.MetadataStore), LatencyMS: time.Since(databaseStarted).Milliseconds()}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready", "checks": checks, "requestId": requestID(r)})
		return
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		checks["database"] = dependency{Status: "unavailable", Kind: string(s.config.Platform.MetadataStore), LatencyMS: time.Since(databaseStarted).Milliseconds()}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready", "checks": checks, "requestId": requestID(r)})
		return
	}
	checks["database"] = dependency{Status: "ready", Kind: string(s.config.Platform.MetadataStore), LatencyMS: time.Since(databaseStarted).Milliseconds()}
	writeStarted := time.Now()
	if err := s.schema.CheckWrite(ctx); err != nil {
		checks["databaseWrite"] = dependency{Status: "unavailable", Kind: string(s.config.Platform.MetadataStore), LatencyMS: time.Since(writeStarted).Milliseconds()}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready", "checks": checks, "requestId": requestID(r)})
		return
	}
	checks["databaseWrite"] = dependency{Status: "ready", Kind: string(s.config.Platform.MetadataStore), LatencyMS: time.Since(writeStarted).Milliseconds()}
	schemaStarted := time.Now()
	schemaStatus, err := s.schema.Check(ctx)
	if err != nil {
		checks["schema"] = dependency{
			Status: "unavailable", Kind: string(schemaStatus.Kind), LatencyMS: time.Since(schemaStarted).Milliseconds(),
			ExpectedVersion: schemaStatus.ExpectedVersion, AppliedVersion: schemaStatus.AppliedVersion,
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready", "checks": checks, "requestId": requestID(r)})
		return
	}
	checks["schema"] = dependency{
		Status: "ready", Kind: string(schemaStatus.Kind), LatencyMS: time.Since(schemaStarted).Milliseconds(),
		ExpectedVersion: schemaStatus.ExpectedVersion, AppliedVersion: schemaStatus.AppliedVersion,
	}
	artifactStarted := time.Now()
	if err := s.artifacts.CheckStore(ctx); err != nil {
		checks["artifactStore"] = dependency{Status: "unavailable", Kind: string(s.config.Platform.ArtifactStore), LatencyMS: time.Since(artifactStarted).Milliseconds()}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready", "checks": checks, "requestId": requestID(r)})
		return
	}
	checks["artifactStore"] = dependency{Status: "ready", Kind: string(s.config.Platform.ArtifactStore), LatencyMS: time.Since(artifactStarted).Milliseconds()}
	checks["queue"] = dependency{Status: "ready", Kind: string(s.config.Platform.QueueDriver)}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "checks": checks})
}

func (s *Server) prometheusMetrics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	payload, err := s.metrics.Gather(ctx)
	if err != nil {
		s.logger.Error("control-plane metrics collection failed", "requestId", requestID(r), "traceId", traceID(r), "error", err)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("# HELP synara_metrics_collection_success Whether authoritative database metrics were collected successfully.\n# TYPE synara_metrics_collection_success gauge\nsynara_metrics_collection_success 0\n"))
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}

func (s *Server) devLogin(w http.ResponseWriter, r *http.Request) {
	if !s.config.DevBootstrapEnabled {
		s.writeError(w, r, problem.New(404, "not_found", "Route not found."))
		return
	}
	var input identity.DevLoginInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	issued, err := s.identity.DevLogin(r.Context(), input, clientIP(r), r.UserAgent(), requestID(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.setSessionCookie(w, issued.Token)
	writeJSON(w, http.StatusOK, issued.State)
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	state, err := s.identity.GetSessionState(r.Context(), mustPrincipal(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if err := s.identity.Revoke(r.Context(), mustPrincipal(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setActiveTenant(w http.ResponseWriter, r *http.Request) {
	var input struct {
		TenantID uuid.UUID `json:"tenantId"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	principal, err := s.identity.SetActiveTenant(r.Context(), mustPrincipal(r), input.TenantID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	state, err := s.identity.GetSessionState(r.Context(), principal)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) listTenants(w http.ResponseWriter, r *http.Request) {
	items, err := s.tenancy.ListTenants(r.Context(), mustPrincipal(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) listTenantDeletionRequests(w http.ResponseWriter, r *http.Request) {
	items, err := s.tenancy.ListDeletingTenants(r.Context(), mustPrincipal(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) createTenant(w http.ResponseWriter, r *http.Request) {
	var input tenancy.CreateTenantInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.tenancy.CreateSelfServiceTenant(r.Context(), mustPrincipal(r), input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) getTenant(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	item, err := s.tenancy.GetTenant(r.Context(), mustPrincipal(r), tenantID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) updateTenant(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	var input tenancy.UpdateTenantInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.tenancy.UpdateTenant(r.Context(), mustPrincipal(r), tenantID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) transitionTenant(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	var input tenancy.TransitionTenantInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.tenancy.TransitionTenant(r.Context(), mustPrincipal(r), tenantID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) restoreTenant(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	var input tenancy.RestoreTenantInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.tenancy.RestoreTenant(r.Context(), mustPrincipal(r), tenantID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) requestTenantDeletion(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	var input tenancy.DeleteTenantInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.tenancy.RequestTenantDeletion(r.Context(), mustPrincipal(r), tenantID, input, requestID(r), clientIP(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteTenant(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	if err := s.tenancy.DeleteTenant(r.Context(), mustPrincipal(r), tenantID, requestID(r), clientIP(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listTenantMembers(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	items, err := s.tenancy.ListTenantMembers(r.Context(), mustPrincipal(r), tenantID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) inviteTenantMember(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	var input tenancy.InviteTenantMemberInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.tenancy.InviteTenantMember(r.Context(), mustPrincipal(r), tenantID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) acceptInvitation(w http.ResponseWriter, r *http.Request) {
	item, err := s.tenancy.AcceptInvitation(r.Context(), mustPrincipal(r), r.PathValue("token"), requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) updateTenantMember(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	userID, ok := s.pathUUID(w, r, "userID")
	if !ok {
		return
	}
	var input tenancy.UpdateTenantMemberInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.tenancy.UpdateTenantMember(r.Context(), mustPrincipal(r), tenantID, userID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) removeTenantMember(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	userID, ok := s.pathUUID(w, r, "userID")
	if !ok {
		return
	}
	if err := s.tenancy.RemoveTenantMember(r.Context(), mustPrincipal(r), tenantID, userID, requestID(r), clientIP(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) revokeTenantUserSessions(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	userID, ok := s.pathUUID(w, r, "userID")
	if !ok {
		return
	}
	revoked, err := s.identity.RevokeTenantUserSessions(
		r.Context(), mustPrincipal(r), tenantID, userID, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revokedCount": revoked})
}

func (s *Server) listOrganizations(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	items, err := s.tenancy.ListOrganizations(r.Context(), mustPrincipal(r), tenantID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) createOrganization(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	var input tenancy.CreateOrganizationInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.tenancy.CreateOrganization(r.Context(), mustPrincipal(r), tenantID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) getOrganization(w http.ResponseWriter, r *http.Request) {
	tenantID, organizationID, ok := s.organizationPath(w, r)
	if !ok {
		return
	}
	item, err := s.tenancy.GetOrganization(r.Context(), mustPrincipal(r), tenantID, organizationID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) updateOrganization(w http.ResponseWriter, r *http.Request) {
	tenantID, organizationID, ok := s.organizationPath(w, r)
	if !ok {
		return
	}
	var input tenancy.UpdateOrganizationInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.tenancy.UpdateOrganization(r.Context(), mustPrincipal(r), tenantID, organizationID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) archiveOrganization(w http.ResponseWriter, r *http.Request) {
	tenantID, organizationID, ok := s.organizationPath(w, r)
	if !ok {
		return
	}
	if err := s.tenancy.ArchiveOrganization(r.Context(), mustPrincipal(r), tenantID, organizationID, requestID(r), clientIP(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listOrganizationMembers(w http.ResponseWriter, r *http.Request) {
	tenantID, organizationID, ok := s.organizationPath(w, r)
	if !ok {
		return
	}
	items, err := s.tenancy.ListOrganizationMembers(r.Context(), mustPrincipal(r), tenantID, organizationID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) putOrganizationMember(w http.ResponseWriter, r *http.Request) {
	tenantID, organizationID, ok := s.organizationPath(w, r)
	if !ok {
		return
	}
	var input tenancy.PutOrganizationMemberInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.tenancy.PutOrganizationMember(r.Context(), mustPrincipal(r), tenantID, organizationID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) updateOrganizationMember(w http.ResponseWriter, r *http.Request) {
	tenantID, organizationID, ok := s.organizationPath(w, r)
	if !ok {
		return
	}
	userID, ok := s.pathUUID(w, r, "userID")
	if !ok {
		return
	}
	var input tenancy.UpdateOrganizationMemberInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.tenancy.UpdateOrganizationMember(r.Context(), mustPrincipal(r), tenantID, organizationID, userID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) removeOrganizationMember(w http.ResponseWriter, r *http.Request) {
	tenantID, organizationID, ok := s.organizationPath(w, r)
	if !ok {
		return
	}
	userID, ok := s.pathUUID(w, r, "userID")
	if !ok {
		return
	}
	if err := s.tenancy.RemoveOrganizationMember(r.Context(), mustPrincipal(r), tenantID, organizationID, userID, requestID(r), clientIP(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	tenantID, organizationID, ok := s.organizationPath(w, r)
	if !ok {
		return
	}
	limit, err := queryInt(r, "limit", 50)
	if err != nil || limit < 1 || limit > 200 {
		s.writeError(w, r, problem.New(400, "invalid_project_limit", "Project list limit must be between 1 and 200."))
		return
	}
	page, err := s.projects.ListPage(r.Context(), mustPrincipal(r), tenantID, organizationID, projects.ProjectListQuery{
		Limit: limit, Cursor: r.URL.Query().Get("cursor"),
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	tenantID, organizationID, ok := s.organizationPath(w, r)
	if !ok {
		return
	}
	var input projects.CreateProjectInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, replayed, err := s.projects.CreateWithIdempotency(
		r.Context(), mustPrincipal(r), tenantID, organizationID, input,
		r.Header.Get("Idempotency-Key"), requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	setIdempotencyReplayHeader(w, replayed)
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	projectID, ok := s.pathUUID(w, r, "projectID")
	if !ok {
		return
	}
	tenantID, err := sessions.ActiveTenant(mustPrincipal(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.projects.Get(r.Context(), mustPrincipal(r), tenantID, projectID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) updateProject(w http.ResponseWriter, r *http.Request) {
	projectID, ok := s.pathUUID(w, r, "projectID")
	if !ok {
		return
	}
	tenantID, err := sessions.ActiveTenant(mustPrincipal(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var input projects.UpdateProjectInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, replayed, err := s.projects.UpdateWithIdempotency(
		r.Context(), mustPrincipal(r), tenantID, projectID, input,
		r.Header.Get("Idempotency-Key"), requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	setIdempotencyReplayHeader(w, replayed)
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) archiveProject(w http.ResponseWriter, r *http.Request) {
	projectID, ok := s.pathUUID(w, r, "projectID")
	if !ok {
		return
	}
	tenantID, err := sessions.ActiveTenant(mustPrincipal(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	_, replayed, err := s.projects.ArchiveWithIdempotency(
		r.Context(), mustPrincipal(r), tenantID, projectID,
		r.Header.Get("Idempotency-Key"), requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	setIdempotencyReplayHeader(w, replayed)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listProjectSessions(w http.ResponseWriter, r *http.Request) {
	projectID, ok := s.pathUUID(w, r, "projectID")
	if !ok {
		return
	}
	limit, err := queryInt(r, "limit", 50)
	if err != nil || limit < 1 || limit > 200 {
		s.writeError(w, r, problem.New(400, "invalid_session_limit", "Session list limit must be between 1 and 200."))
		return
	}
	page, err := s.sessions.ListByProjectPage(r.Context(), mustPrincipal(r), projectID, sessions.SessionListQuery{
		Limit: limit, Cursor: r.URL.Query().Get("cursor"),
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	projectID, ok := s.pathUUID(w, r, "projectID")
	if !ok {
		return
	}
	var input sessions.CreateSessionInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, replayed, err := s.sessions.CreateWithIdempotency(
		r.Context(), mustPrincipal(r), projectID, input,
		r.Header.Get("Idempotency-Key"), requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	setIdempotencyReplayHeader(w, replayed)
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) getAgentSession(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := s.pathUUID(w, r, "sessionID")
	if !ok {
		return
	}
	tenantID, err := sessions.ActiveTenant(mustPrincipal(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.sessions.Get(r.Context(), mustPrincipal(r), tenantID, sessionID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) createTurn(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := s.pathUUID(w, r, "sessionID")
	if !ok {
		return
	}
	var input sessions.CreateTurnInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, replayed, err := s.sessions.CreateTurnWithIdempotency(
		r.Context(), mustPrincipal(r), sessionID, input,
		r.Header.Get("Idempotency-Key"), requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	setIdempotencyReplayHeader(w, replayed)
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) listSessionEvents(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := s.pathUUID(w, r, "sessionID")
	if !ok {
		return
	}
	afterSequence, err := queryInt64(r, "afterSequence", 0)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	limit, err := queryInt(r, "limit", 50)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if limit < 1 || limit > 200 {
		s.writeError(w, r, problem.New(400, "invalid_event_limit", "limit must be between 1 and 200."))
		return
	}
	page, err := s.sessions.ListEvents(r.Context(), mustPrincipal(r), sessionID, afterSequence, limit)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) archiveSession(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := s.pathUUID(w, r, "sessionID")
	if !ok {
		return
	}
	item, replayed, err := s.sessions.ArchiveWithIdempotency(
		r.Context(), mustPrincipal(r), sessionID, r.Header.Get("Idempotency-Key"), requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	setIdempotencyReplayHeader(w, replayed)
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) suspendSession(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := s.pathUUID(w, r, "sessionID")
	if !ok {
		return
	}
	item, replayed, err := s.sessions.Suspend(
		r.Context(), mustPrincipal(r), sessionID, r.Header.Get("Idempotency-Key"), requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	setIdempotencyReplayHeader(w, replayed)
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) resumeSession(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := s.pathUUID(w, r, "sessionID")
	if !ok {
		return
	}
	item, replayed, err := s.sessions.Resume(
		r.Context(), mustPrincipal(r), sessionID, r.Header.Get("Idempotency-Key"), requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	setIdempotencyReplayHeader(w, replayed)
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) cancelExecution(w http.ResponseWriter, r *http.Request) {
	executionID, ok := s.pathUUID(w, r, "executionID")
	if !ok {
		return
	}
	result, err := s.executions.Cancel(
		r.Context(), mustPrincipal(r), executionID, r.Header.Get("Idempotency-Key"), requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	setIdempotencyReplayHeader(w, result.Replayed)
	writeJSON(w, result.StatusCode, projectDeveloperExecution(result.Value))
}

func (s *Server) resumeActiveTurnExecution(w http.ResponseWriter, r *http.Request) {
	executionID, ok := s.pathUUID(w, r, "executionID")
	if !ok {
		return
	}
	result, err := s.executions.ResumeActiveTurn(
		r.Context(), mustPrincipal(r), executionID,
		r.Header.Get("Idempotency-Key"), requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	setIdempotencyReplayHeader(w, result.Replayed)
	writeJSON(w, result.StatusCode, projectDeveloperExecution(result.Value))
}

func (s *Server) interruptActiveTurn(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := s.pathUUID(w, r, "sessionID")
	if !ok {
		return
	}
	result, err := s.executions.RequestInterrupt(
		r.Context(), mustPrincipal(r), sessionID, r.Header.Get("Idempotency-Key"), requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	setIdempotencyReplayHeader(w, result.Replayed)
	writeJSON(w, result.StatusCode, projectDeveloperControlCommand(result.Value))
}

func (s *Server) resumeActiveTurn(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := s.pathUUID(w, r, "sessionID")
	if !ok {
		return
	}
	result, err := s.executions.ResumeActiveTurnForSession(
		r.Context(), mustPrincipal(r), sessionID,
		r.Header.Get("Idempotency-Key"), requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	setIdempotencyReplayHeader(w, result.Replayed)
	writeJSON(w, result.StatusCode, projectDeveloperExecution(result.Value))
}

func (s *Server) steerActiveTurn(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := s.pathUUID(w, r, "sessionID")
	if !ok {
		return
	}
	var input executions.SteerActiveTurnInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	result, err := s.executions.RequestSteer(
		r.Context(), mustPrincipal(r), sessionID, input,
		r.Header.Get("Idempotency-Key"), requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	setIdempotencyReplayHeader(w, result.Replayed)
	writeJSON(w, result.StatusCode, projectDeveloperControlCommand(result.Value))
}

func (s *Server) listExecutionInteractions(w http.ResponseWriter, r *http.Request) {
	executionID, ok := s.pathUUID(w, r, "executionID")
	if !ok {
		return
	}
	if _, machine := authorization.MachinePrincipalFromContext(r.Context()); machine {
		limit, err := queryInt(r, "limit", 50)
		if err != nil || limit < 1 || limit > 200 {
			s.writeError(w, r, problem.New(400, "invalid_interaction_limit", "Interaction list limit must be between 1 and 200."))
			return
		}
		page, err := s.executions.ListInteractionsPage(r.Context(), mustPrincipal(r), executionID, executions.InteractionListQuery{
			Limit: limit, Cursor: r.URL.Query().Get("cursor"),
		})
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": projectDeveloperInteractions(page.Items), "nextCursor": page.NextCursor})
		return
	}
	items, err := s.executions.ListInteractions(r.Context(), mustPrincipal(r), executionID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) listExecutionRuntimeIsolationDecisions(w http.ResponseWriter, r *http.Request) {
	executionID, ok := s.pathUUID(w, r, "executionID")
	if !ok {
		return
	}
	items, err := s.executions.ListRuntimeIsolationDecisions(r.Context(), mustPrincipal(r), executionID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) listPendingSessionInteractions(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := s.pathUUID(w, r, "sessionID")
	if !ok {
		return
	}
	if _, machine := authorization.MachinePrincipalFromContext(r.Context()); machine {
		limit, err := queryInt(r, "limit", 50)
		if err != nil || limit < 1 || limit > 200 {
			s.writeError(w, r, problem.New(400, "invalid_interaction_limit", "Interaction list limit must be between 1 and 200."))
			return
		}
		page, err := s.executions.ListPendingInteractionsPage(r.Context(), mustPrincipal(r), sessionID, executions.InteractionListQuery{
			Limit: limit, Cursor: r.URL.Query().Get("cursor"),
		})
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, page)
		return
	}
	snapshot, err := s.executions.ListPendingInteractions(r.Context(), mustPrincipal(r), sessionID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *Server) resolveExecutionApproval(w http.ResponseWriter, r *http.Request) {
	executionID, ok := s.pathUUID(w, r, "executionID")
	if !ok {
		return
	}
	var input executions.ResolveApprovalInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	result, err := s.executions.ResolveApproval(
		r.Context(), mustPrincipal(r), executionID, r.PathValue("requestID"), input,
		r.Header.Get("Idempotency-Key"), requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	setIdempotencyReplayHeader(w, result.Replayed)
	if _, machine := authorization.MachinePrincipalFromContext(r.Context()); machine {
		writeJSON(w, result.StatusCode, projectDeveloperInteraction(result.Value))
		return
	}
	writeJSON(w, result.StatusCode, result.Value)
}

func (s *Server) resolveExecutionUserInput(w http.ResponseWriter, r *http.Request) {
	executionID, ok := s.pathUUID(w, r, "executionID")
	if !ok {
		return
	}
	var input executions.ResolveUserInputInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	result, err := s.executions.ResolveUserInput(
		r.Context(), mustPrincipal(r), executionID, r.PathValue("requestID"), input,
		r.Header.Get("Idempotency-Key"), requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	setIdempotencyReplayHeader(w, result.Replayed)
	if _, machine := authorization.MachinePrincipalFromContext(r.Context()); machine {
		writeJSON(w, result.StatusCode, projectDeveloperInteraction(result.Value))
		return
	}
	writeJSON(w, result.StatusCode, result.Value)
}

func setIdempotencyReplayHeader(w http.ResponseWriter, replayed bool) {
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, cookieErr := r.Cookie(s.config.CookieName)
		authorizationHeader := strings.TrimSpace(r.Header.Get("Authorization"))
		if cookieErr == nil && authorizationHeader != "" {
			s.writeError(w, r, problem.New(400, "ambiguous_authentication", "Use either a Web cookie or a Desktop Bearer credential, not both."))
			return
		}
		var principal identity.Principal
		var err error
		if authorizationHeader != "" {
			bearer, ok := desktopBearerToken(authorizationHeader)
			if !ok {
				s.writeError(w, r, problem.New(401, "desktop_authentication_required", "A valid Desktop Bearer credential is required."))
				return
			}
			principal, err = s.identity.AuthenticateDesktopRequest(r.Context(), bearer, requestID(r), clientIP(r))
		} else {
			if cookieErr != nil {
				s.writeError(w, r, problem.New(401, "authentication_required", "Authentication is required."))
				return
			}
			principal, err = s.identity.Authenticate(r.Context(), cookie.Value)
		}
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		if principal.ActiveTenantID != nil {
			requestLogScopeFor(r).tenantID = *principal.ActiveTenantID
		}
		if principal.SupportAccessGrantID != nil {
			if err := s.supportAccess.RecordAccess(
				r.Context(), principal, r.Method, normalizedLogRoute(r), requestID(r), clientIP(r),
			); err != nil {
				s.writeError(w, r, err)
				return
			}
			allowedControlAction :=
				(r.Method == http.MethodPut && r.Pattern == "PUT /v1/auth/active-tenant") ||
					(r.Method == http.MethodPost && r.Pattern == "POST /v1/auth/logout")
			if r.Method != http.MethodGet && r.Method != http.MethodHead && !allowedControlAction {
				s.writeError(w, r, problem.New(403, "support_access_read_only", "Support Access is read-only."))
				return
			}
		}
		_, patternPath, hasPatternPath := strings.Cut(r.Pattern, " ")
		explicitTenantRoute := hasPatternPath &&
			(patternPath == "/v1/tenants/{tenantID}" || strings.HasPrefix(patternPath, "/v1/tenants/{tenantID}/"))
		deletionRecoveryRoute := r.Pattern == "POST /v1/tenants/{tenantID}/restore"
		if explicitTenantRoute && !deletionRecoveryRoute {
			pathTenantID, parseErr := uuid.Parse(r.PathValue("tenantID"))
			if parseErr == nil {
				if err := identity.RequireActiveTenant(principal, pathTenantID); err != nil {
					s.writeError(w, r, err)
					return
				}
			}
		}
		ctx := context.WithValue(r.Context(), principalContextKey{}, principal)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) requireDeveloperAuth(next http.Handler) http.Handler {
	userAuthentication := s.requireAuth(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizationHeader := strings.TrimSpace(r.Header.Get("Authorization"))
		bearer, bearerOK := desktopBearerToken(authorizationHeader)
		if authorizationHeader == "" || !bearerOK || !strings.HasPrefix(bearer, serviceaccounts.TokenPrefix) {
			userAuthentication.ServeHTTP(w, r)
			return
		}
		if _, cookieErr := r.Cookie(s.config.CookieName); cookieErr == nil {
			s.writeError(w, r, problem.New(400, "ambiguous_authentication", "Use either a Web cookie or a Service Account Bearer credential, not both."))
			return
		}
		if s.serviceAccounts == nil {
			s.writeError(w, r, problem.New(503, "developer_api_authentication_unavailable", "Developer API authentication is unavailable."))
			return
		}
		machine, err := s.serviceAccounts.Authenticate(r.Context(), bearer)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		if !machine.Allows("api.access") {
			s.writeError(w, r, problem.New(403, "service_account_scope_forbidden", "Service Account is not allowed to access the developer API."))
			return
		}
		if _, patternPath, ok := strings.Cut(r.Pattern, " "); ok &&
			(patternPath == "/v1/tenants/{tenantID}" || strings.HasPrefix(patternPath, "/v1/tenants/{tenantID}/")) {
			if pathTenantID, parseErr := uuid.Parse(r.PathValue("tenantID")); parseErr == nil && pathTenantID != machine.TenantID {
				s.writeError(w, r, problem.New(403, "service_account_tenant_forbidden", "Service Account cannot access this tenant."))
				return
			}
		}
		activeTenantID := machine.TenantID
		serviceAccountID := machine.ID
		principal := identity.Principal{
			UserID: machine.CreatedBy, ActiveTenantID: &activeTenantID, Audience: "service-account",
			DisplayName: machine.Name, ServiceAccountID: &serviceAccountID,
		}
		scope := requestLogScopeFor(r)
		scope.tenantID = machine.TenantID
		if machine.OrganizationID != nil {
			scope.organizationID = *machine.OrganizationID
		}
		ctx := authorization.WithMachinePrincipal(r.Context(), authorization.MachinePrincipal{
			ActorID: machine.ID, TenantID: machine.TenantID,
			OrganizationID: machine.OrganizationID, Role: machine.Role,
		})
		ctx = context.WithValue(ctx, principalContextKey{}, principal)
		admission, err := s.developerAPIUsage.Admit(
			ctx, machine.TenantID, machine.ID, machine.OrganizationID, r.Pattern, machine.RateLimitPerMinute,
		)
		if err != nil {
			s.writeError(w, r, problem.Wrap(
				http.StatusServiceUnavailable,
				"developer_api_admission_unavailable",
				"Developer API admission is temporarily unavailable.",
				err,
			))
			return
		}
		setDeveloperAPIRateLimitHeaders(w.Header(), admission)
		if !admission.Allowed {
			w.Header().Set("Retry-After", strconv.Itoa(developerAPIRetryAfterSeconds(admission.ResetAfter)))
			s.writeError(w, r, problem.New(
				http.StatusTooManyRequests,
				"developer_api_rate_limited",
				"Developer API rate limit exceeded for this Service Account.",
			))
			return
		}
		recorder := &developerAPIResponseWriter{ResponseWriter: w}
		startedAt := time.Now()
		next.ServeHTTP(recorder, r.WithContext(ctx))
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		if err := s.developerAPIUsage.RecordOutcome(
			context.WithoutCancel(ctx), machine.TenantID, machine.ID,
			admission.ResetAt.Add(-time.Minute), r.Pattern, status, time.Since(startedAt),
		); err != nil {
			s.logger.Warn(
				"developer API usage outcome could not be recorded",
				"tenantId", machine.TenantID,
				"serviceAccountId", machine.ID,
				"routePattern", r.Pattern,
				"error", err,
			)
		}
	})
}

func desktopBearerToken(value string) (string, bool) {
	parts := strings.Fields(value)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
		return "", false
	}
	return parts[1], true
}

func (s *Server) requireServiceAccount(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization := strings.TrimSpace(r.Header.Get("Authorization"))
		if !strings.HasPrefix(strings.ToLower(authorization), "bearer ") {
			s.writeError(w, r, problem.New(401, "service_account_authentication_required", "Service Account authentication is required."))
			return
		}
		principal, err := s.serviceAccounts.Authenticate(r.Context(), strings.TrimSpace(authorization[len("Bearer "):]))
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		scope := requestLogScopeFor(r)
		scope.tenantID = principal.TenantID
		if principal.OrganizationID != nil {
			scope.organizationID = *principal.OrganizationID
		}
		ctx := context.WithValue(r.Context(), serviceAccountContextKey{}, principal)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) withRequestContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if id == "" || len(id) > 160 {
			id = uuid.NewString()
		}
		if incoming := validHex(strings.TrimSpace(r.Header.Get("X-Trace-ID")), 16); incoming != "" {
			r.Header.Set("Traceparent", "00-"+incoming+"-"+randomHex(8)+"-01")
		}
		extracted := controltracing.ExtractHTTP(r.Context(), r.Header)
		ctx, span := otel.Tracer("synara/control-plane").Start(
			extracted, "control-plane.http", oteltrace.WithSpanKind(oteltrace.SpanKindServer),
		)
		defer span.End()
		spanContext := span.SpanContext()
		traceID := incomingTraceID(r)
		spanID := ""
		traceFlags := "01"
		if spanContext.IsValid() {
			traceID = spanContext.TraceID().String()
			spanID = spanContext.SpanID().String()
			if !spanContext.IsSampled() {
				traceFlags = "00"
			}
		}
		if traceID == "" {
			traceID = randomHex(16)
		}
		if spanID == "" {
			spanID = randomHex(8)
		}
		w.Header().Set("X-Request-ID", id)
		w.Header().Set("X-Trace-ID", traceID)
		w.Header().Set("Traceparent", "00-"+traceID+"-"+spanID+"-"+traceFlags)
		ctx = context.WithValue(ctx, requestIDContextKey{}, id)
		ctx = context.WithValue(ctx, traceIDContextKey{}, traceID)
		ctx = context.WithValue(ctx, clientIPContextKey{}, s.resolveClientIP(r))
		ctx = context.WithValue(ctx, requestLogScopeContextKey{}, &requestLogScope{})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type responseStatusRecorder struct {
	http.ResponseWriter
	status      int
	problemCode string
}

func (w *responseStatusRecorder) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseStatusRecorder) Write(payload []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(payload)
}

func (w *responseStatusRecorder) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *responseStatusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *responseStatusRecorder) recordProblem(code string) { w.problemCode = code }

func (s *Server) observeRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &responseStatusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		duration := time.Since(started)
		s.metrics.ObserveHTTP(r.Method, r.Pattern, status, duration, recorder.problemCode)
		span := oteltrace.SpanFromContext(r.Context())
		span.SetName(normalizedLogRoute(r))
		span.SetAttributes(
			attribute.String("http.request.method", r.Method),
			attribute.String("http.route", normalizedLogRoute(r)),
			attribute.Int("http.response.status_code", status),
		)
		if recorder.problemCode != "" {
			span.SetAttributes(attribute.String("synara.error.code", recorder.problemCode))
		}
		if status >= http.StatusInternalServerError {
			span.SetStatus(codes.Error, http.StatusText(status))
		}
		attributes := []any{
			"requestId", requestID(r), "traceId", traceID(r), "method", r.Method,
			"route", normalizedLogRoute(r), "status", status, "durationMs", duration.Milliseconds(),
		}
		attributes = append(attributes, requestScopeLogAttributes(r, recorder.problemCode)...)
		s.logger.Info("control-plane request completed", attributes...)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.logger.Error("control-plane request panic", "requestId", requestID(r), "traceId", traceID(r), "panic", recovered)
				s.writeError(w, r, problem.New(500, "internal_error", "The control plane encountered an unexpected error."))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func decodeJSON(r *http.Request, target any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return problem.New(415, "unsupported_media_type", "Content-Type must be application/json.")
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxJSONBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return problem.Wrap(400, "invalid_json", "Request body is not valid JSON.", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return problem.New(400, "invalid_json", "Request body must contain one JSON value.")
	}
	captureDecodedRequestScope(r, target)
	return nil
}

func (s *Server) pathUUID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		s.writeError(w, r, problem.New(400, "invalid_id", "Path identifier is invalid."))
		return uuid.Nil, false
	}
	return id, true
}

func (s *Server) organizationPath(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return uuid.Nil, uuid.Nil, false
	}
	organizationID, ok := s.pathUUID(w, r, "organizationID")
	if !ok {
		return uuid.Nil, uuid.Nil, false
	}
	return tenantID, organizationID, true
}

func queryInt64(r *http.Request, name string, fallback int64) (int64, error) {
	value := strings.TrimSpace(r.URL.Query().Get(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, problem.New(400, "invalid_query_parameter", name+" must be an integer.")
	}
	return parsed, nil
}

func queryInt(r *http.Request, name string, fallback int) (int, error) {
	value, err := queryInt64(r, name, int64(fallback))
	if err != nil {
		return 0, err
	}
	if value > int64(^uint(0)>>1) || value < -int64(^uint(0)>>1)-1 {
		return 0, problem.New(400, "invalid_query_parameter", name+" is out of range.")
	}
	return int(value), nil
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var apiError *problem.Error
	if podlifecycle.IsLogicalIdentityLockUnavailable(err) {
		apiError = problem.New(
			http.StatusServiceUnavailable,
			"worker_logical_identity_lock_unavailable",
			"The Worker logical identity is changing; retry the request.",
		)
	} else if !errors.As(err, &apiError) {
		apiError = problem.Wrap(500, "internal_error", "The control plane encountered an unexpected error.", err)
	}
	if recorder, ok := w.(interface{ recordProblem(string) }); ok {
		recorder.recordProblem(apiError.Code)
	}
	if apiError.Status >= 500 {
		attributes := []any{"requestId", requestID(r), "traceId", traceID(r)}
		attributes = append(attributes, requestScopeLogAttributes(r, apiError.Code)...)
		attributes = append(attributes, "error", apiError)
		s.logger.Error("control-plane request failed", attributes...)
	}
	writeJSON(w, apiError.Status, map[string]any{
		"error": map[string]any{
			"code": apiError.Code, "message": apiError.Message,
			"requestId": requestID(r), "details": apiError.Details,
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func mustPrincipal(r *http.Request) identity.Principal {
	return r.Context().Value(principalContextKey{}).(identity.Principal)
}

func mustServiceAccount(r *http.Request) serviceaccounts.Principal {
	return r.Context().Value(serviceAccountContextKey{}).(serviceaccounts.Principal)
}

func requestID(r *http.Request) string {
	value, _ := r.Context().Value(requestIDContextKey{}).(string)
	return value
}

func traceID(r *http.Request) string {
	value, _ := r.Context().Value(traceIDContextKey{}).(string)
	return value
}

func requestLogScopeFor(r *http.Request) *requestLogScope {
	if r != nil {
		if scope, ok := r.Context().Value(requestLogScopeContextKey{}).(*requestLogScope); ok && scope != nil {
			return scope
		}
	}
	return &requestLogScope{}
}

func captureDecodedRequestScope(r *http.Request, target any) {
	scope := requestLogScopeFor(r)
	value := reflect.ValueOf(target)
	for value.IsValid() && value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return
		}
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return
	}
	if tenantID, ok := decodedUUIDField(value, "TenantID"); ok {
		scope.tenantID = tenantID
	}
	if organizationID, ok := decodedUUIDField(value, "OrganizationID"); ok {
		scope.organizationID = organizationID
	}
	if generation := value.FieldByName("Generation"); generation.IsValid() && generation.CanInt() && generation.Int() > 0 {
		scope.generation = generation.Int()
	}
}

func decodedUUIDField(value reflect.Value, name string) (uuid.UUID, bool) {
	field := value.FieldByName(name)
	for field.IsValid() && field.Kind() == reflect.Pointer {
		if field.IsNil() {
			return uuid.Nil, false
		}
		field = field.Elem()
	}
	if !field.IsValid() || !field.CanInterface() {
		return uuid.Nil, false
	}
	id, ok := field.Interface().(uuid.UUID)
	return id, ok && id != uuid.Nil
}

func requestScopeLogAttributes(r *http.Request, errorCode string) []any {
	scope := requestLogScopeFor(r)
	attributes := make([]any, 0, 14)
	appendID := func(key, pathName string, fallback uuid.UUID) {
		id, err := uuid.Parse(r.PathValue(pathName))
		if err != nil || id == uuid.Nil {
			id = fallback
		}
		if id != uuid.Nil {
			attributes = append(attributes, key, id.String())
		}
	}
	appendID("tenantId", "tenantID", scope.tenantID)
	appendID("organizationId", "organizationID", scope.organizationID)
	appendID("sessionId", "sessionID", uuid.Nil)
	appendID("executionId", "executionID", uuid.Nil)
	if scope.workerID != uuid.Nil {
		attributes = append(attributes, "workerId", scope.workerID.String())
	}
	if scope.generation > 0 {
		attributes = append(attributes, "generation", scope.generation)
	}
	if errorCode = strings.TrimSpace(errorCode); errorCode != "" {
		attributes = append(attributes, "errorCode", errorCode)
	}
	return attributes
}

func incomingTraceID(r *http.Request) string {
	if value := validHex(strings.TrimSpace(r.Header.Get("X-Trace-ID")), 16); value != "" {
		return value
	}
	parts := strings.Split(strings.TrimSpace(r.Header.Get("Traceparent")), "-")
	if len(parts) == 4 && parts[0] == "00" {
		return validHex(parts[1], 16)
	}
	return ""
}

func validHex(value string, bytes int) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != bytes*2 || strings.Trim(value, "0") == "" {
		return ""
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != bytes {
		return ""
	}
	return value
}

func randomHex(bytes int) string {
	buffer := make([]byte, bytes)
	if _, err := rand.Read(buffer); err != nil {
		return strings.ReplaceAll(uuid.NewString(), "-", "")[:bytes*2]
	}
	return hex.EncodeToString(buffer)
}

func normalizedLogRoute(r *http.Request) string {
	pattern := strings.TrimSpace(r.Pattern)
	if pattern == "" {
		return "unmatched"
	}
	if prefix := r.Method + " "; strings.HasPrefix(pattern, prefix) {
		pattern = strings.TrimSpace(strings.TrimPrefix(pattern, prefix))
	}
	return pattern
}

func clientIP(r *http.Request) string {
	if value, ok := r.Context().Value(clientIPContextKey{}).(string); ok && value != "" {
		return value
	}
	address, ok := directRemoteIP(r.RemoteAddr)
	if ok {
		return address.String()
	}
	return strings.TrimSpace(r.RemoteAddr)
}

func (s *Server) resolveClientIP(r *http.Request) string {
	remote, ok := directRemoteIP(r.RemoteAddr)
	if !ok {
		return strings.TrimSpace(r.RemoteAddr)
	}
	candidate := remote
	forwarded := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for index := len(forwarded) - 1; index >= 0 && s.isTrustedProxy(candidate); index-- {
		address, err := netip.ParseAddr(strings.TrimSpace(forwarded[index]))
		if err != nil {
			return remote.String()
		}
		candidate = address.Unmap()
	}
	return candidate.String()
}

func (s *Server) isTrustedProxy(address netip.Addr) bool {
	for _, prefix := range s.config.TrustedProxyCIDRs {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func directRemoteIP(value string) (netip.Addr, bool) {
	if addressPort, err := netip.ParseAddrPort(strings.TrimSpace(value)); err == nil {
		return addressPort.Addr().Unmap(), true
	}
	address, err := netip.ParseAddr(strings.Trim(strings.TrimSpace(value), "[]"))
	if err != nil {
		return netip.Addr{}, false
	}
	return address.Unmap(), true
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: s.config.CookieName, Value: token, Domain: s.config.CookieDomain,
		Path: s.config.CookiePath, HttpOnly: true, Secure: s.config.CookieSecure,
		SameSite: sessionCookieSameSite(s.config.CookieSameSite),
		MaxAge:   int(s.config.SessionTTL.Seconds()), Expires: time.Now().UTC().Add(s.config.SessionTTL),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: s.config.CookieName, Value: "", Domain: s.config.CookieDomain,
		Path: s.config.CookiePath, HttpOnly: true, Secure: s.config.CookieSecure,
		SameSite: sessionCookieSameSite(s.config.CookieSameSite),
		MaxAge:   -1, Expires: time.Unix(1, 0).UTC(),
	})
}

func sessionCookieSameSite(value string) http.SameSite {
	switch value {
	case "strict":
		return http.SameSiteStrictMode
	case "none":
		return http.SameSiteNoneMode
	default:
		return http.SameSiteLaxMode
	}
}
