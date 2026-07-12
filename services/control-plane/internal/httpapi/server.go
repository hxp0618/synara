package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/artifacts"
	"github.com/synara-ai/synara/services/control-plane/internal/config"
	"github.com/synara-ai/synara/services/control-plane/internal/credentials"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/enterpriseidentity"
	"github.com/synara-ai/synara/services/control-plane/internal/eventstream"
	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/observability"
	"github.com/synara-ai/synara/services/control-plane/internal/outbox"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/projects"
	"github.com/synara-ai/synara/services/control-plane/internal/quotas"
	"github.com/synara-ai/synara/services/control-plane/internal/retention"
	"github.com/synara-ai/synara/services/control-plane/internal/scim"
	"github.com/synara-ai/synara/services/control-plane/internal/serviceaccounts"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
	"github.com/synara-ai/synara/services/control-plane/internal/tenancy"
)

const maxJSONBodyBytes = 1 << 20

type principalContextKey struct{}
type requestIDContextKey struct{}
type traceIDContextKey struct{}
type clientIPContextKey struct{}
type workerContextKey struct{}
type serviceAccountContextKey struct{}

type Server struct {
	config             config.Config
	db                 *gorm.DB
	identity           *identity.Service
	tenancy            *tenancy.Service
	projects           *projects.Service
	sessions           *sessions.Service
	executions         *executions.Service
	targets            *executiontargets.Service
	sshTargets         *executiontargets.SSHProvisioner
	artifacts          *artifacts.Service
	quotas             *quotas.Service
	credentials        *credentials.Service
	retention          *retention.Service
	metrics            *observability.Registry
	outbox             *outbox.Service
	enterpriseIdentity *enterpriseidentity.Service
	serviceAccounts    *serviceaccounts.Service
	scim               *scim.Service
	logger             *slog.Logger
	schema             schemaReadiness
	eventStreams       *eventstream.Service
	sessionEventPoll   time.Duration
	sessionEventBeat   time.Duration
	sessionEventWrite  time.Duration
	handler            http.Handler
}

type schemaReadiness interface {
	Check(context.Context) (database.SchemaStatus, error)
	CheckWrite(context.Context) error
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
) (*Server, error) {
	eventStreams, err := eventstream.New(db, eventstream.Config{
		InstanceID: uuid.NewString(), LeaseTTL: cfg.SSELeaseTTL,
		MaxConnectionsPerUser:   cfg.SSEMaxConnectionsPerUser,
		MaxConnectionsPerTenant: cfg.SSEMaxConnectionsPerTenant,
	})
	if err != nil {
		return nil, err
	}
	server := &Server{
		config: cfg, db: db, identity: identityService, tenancy: tenancyService,
		projects: projectService, sessions: sessionService, executions: executionService,
		targets: executionTargetService, sshTargets: sshProvisioner,
		artifacts: artifactService, quotas: quotaService,
		credentials: credentialService, retention: retentionService, metrics: metrics, outbox: outboxService,
		enterpriseIdentity: enterpriseIdentityService, serviceAccounts: serviceAccountService,
		scim: scimService, schema: schemaChecker, logger: logger, eventStreams: eventStreams,
		sessionEventPoll: cfg.SSEPollInterval, sessionEventBeat: cfg.SSEHeartbeatInterval,
		sessionEventWrite: cfg.SSEWriteTimeout,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", server.health)
	mux.HandleFunc("GET /ready", server.ready)
	mux.HandleFunc("GET /metrics", server.prometheusMetrics)
	mux.HandleFunc("GET /v1/platform/profile", server.getPlatformProfile)
	mux.HandleFunc("POST /v1/auth/dev-login", server.devLogin)
	mux.HandleFunc("GET /v1/auth/sso/connections", server.listPublicIdentityConnections)
	mux.HandleFunc("GET /v1/auth/sso/{connectionID}/start", server.startSSO)
	mux.HandleFunc("GET /v1/auth/sso/{connectionID}/metadata", server.samlMetadata)
	mux.HandleFunc("GET /v1/auth/sso/{connectionID}/callback", server.completeSSO)
	mux.HandleFunc("POST /v1/auth/sso/{connectionID}/callback", server.completeSSO)
	mux.HandleFunc("POST /v1/workers/register", server.registerWorker)
	mux.Handle("POST /v1/workers/heartbeat", server.requireWorker(http.HandlerFunc(server.workerHeartbeat)))
	mux.Handle("POST /v1/workers/executions/claim", server.requireWorker(http.HandlerFunc(server.claimExecution)))
	mux.Handle("POST /v1/workers/executions/{executionID}/renew", server.requireWorker(http.HandlerFunc(server.renewExecutionLease)))
	mux.Handle("POST /v1/workers/executions/{executionID}/start", server.requireWorker(http.HandlerFunc(server.startExecution)))
	mux.Handle("POST /v1/workers/executions/{executionID}/complete", server.requireWorker(http.HandlerFunc(server.completeExecution)))
	mux.Handle("POST /v1/workers/executions/{executionID}/fail", server.requireWorker(http.HandlerFunc(server.failExecution)))
	mux.Handle("POST /v1/workers/executions/{executionID}/release", server.requireWorker(http.HandlerFunc(server.releaseExecution)))
	mux.Handle("POST /v1/workers/executions/{executionID}/events", server.requireWorker(http.HandlerFunc(server.appendRuntimeEvent)))
	mux.Handle("POST /v1/workers/executions/{executionID}/interaction-resolutions/pull", server.requireWorker(http.HandlerFunc(server.pullInteractionResolutions)))
	mux.Handle("POST /v1/workers/executions/{executionID}/interaction-resolutions/{interactionID}/delivered", server.requireWorker(http.HandlerFunc(server.markInteractionResolutionDelivered)))
	mux.Handle("POST /v1/workers/executions/{executionID}/interaction-resolutions/{interactionID}/acknowledged", server.requireWorker(http.HandlerFunc(server.acknowledgeInteractionResolution)))
	mux.Handle("POST /v1/workers/executions/{executionID}/control-commands/pull", server.requireWorker(http.HandlerFunc(server.pullControlCommands)))
	mux.Handle("POST /v1/workers/executions/{executionID}/control-commands/{controlCommandID}/delivered", server.requireWorker(http.HandlerFunc(server.markControlCommandDelivered)))
	mux.Handle("POST /v1/workers/executions/{executionID}/control-commands/{controlCommandID}/acknowledged", server.requireWorker(http.HandlerFunc(server.acknowledgeControlCommand)))
	mux.Handle("POST /v1/workers/executions/{executionID}/artifacts", server.requireWorker(http.HandlerFunc(server.createWorkerArtifact)))
	mux.Handle("POST /v1/workers/executions/{executionID}/artifacts/{artifactID}/complete", server.requireWorker(http.HandlerFunc(server.completeWorkerArtifact)))
	mux.Handle("POST /v1/workers/executions/{executionID}/credentials/{credentialID}/resolve", server.requireWorker(http.HandlerFunc(server.resolveExecutionCredential)))
	mux.Handle("GET /v1/auth/session", server.requireAuth(http.HandlerFunc(server.getSession)))
	mux.Handle("POST /v1/auth/logout", server.requireAuth(http.HandlerFunc(server.logout)))
	mux.Handle("PUT /v1/auth/active-tenant", server.requireAuth(http.HandlerFunc(server.setActiveTenant)))
	mux.Handle("POST /v1/invitations/{token}/accept", server.requireAuth(http.HandlerFunc(server.acceptInvitation)))

	mux.Handle("GET /v1/tenants", server.requireAuth(http.HandlerFunc(server.listTenants)))
	mux.Handle("POST /v1/tenants", server.requireAuth(http.HandlerFunc(server.createTenant)))
	mux.Handle("GET /v1/tenants/{tenantID}", server.requireAuth(http.HandlerFunc(server.getTenant)))
	mux.Handle("PATCH /v1/tenants/{tenantID}", server.requireAuth(http.HandlerFunc(server.updateTenant)))
	mux.Handle("DELETE /v1/tenants/{tenantID}", server.requireAuth(http.HandlerFunc(server.deleteTenant)))
	mux.Handle("GET /v1/tenants/{tenantID}/members", server.requireAuth(http.HandlerFunc(server.listTenantMembers)))
	mux.Handle("POST /v1/tenants/{tenantID}/invitations", server.requireAuth(http.HandlerFunc(server.inviteTenantMember)))
	mux.Handle("PATCH /v1/tenants/{tenantID}/members/{userID}", server.requireAuth(http.HandlerFunc(server.updateTenantMember)))
	mux.Handle("DELETE /v1/tenants/{tenantID}/members/{userID}", server.requireAuth(http.HandlerFunc(server.removeTenantMember)))
	mux.Handle("POST /v1/tenants/{tenantID}/members/{userID}/revoke-sessions", server.requireAuth(http.HandlerFunc(server.revokeTenantUserSessions)))
	mux.Handle("GET /v1/tenants/{tenantID}/audit-logs", server.requireAuth(http.HandlerFunc(server.listAuditLogs)))
	mux.Handle("GET /v1/tenants/{tenantID}/audit-logs/export", server.requireAuth(http.HandlerFunc(server.exportAuditLogs)))
	mux.Handle("GET /v1/tenants/{tenantID}/outbox-messages", server.requireAuth(http.HandlerFunc(server.listOutboxMessages)))
	mux.Handle("POST /v1/tenants/{tenantID}/outbox-messages/{messageID}/replay", server.requireAuth(http.HandlerFunc(server.replayOutboxMessage)))
	mux.Handle("GET /v1/tenants/{tenantID}/execution-targets", server.requireAuth(http.HandlerFunc(server.listExecutionTargets)))
	mux.Handle("POST /v1/tenants/{tenantID}/execution-targets", server.requireAuth(http.HandlerFunc(server.createExecutionTarget)))
	mux.Handle("GET /v1/tenants/{tenantID}/execution-targets/{executionTargetID}", server.requireAuth(http.HandlerFunc(server.getExecutionTarget)))
	mux.Handle("POST /v1/tenants/{tenantID}/execution-targets/{executionTargetID}/ssh/install", server.requireAuth(http.HandlerFunc(server.installSSHExecutionTarget)))
	mux.Handle("POST /v1/tenants/{tenantID}/execution-targets/{executionTargetID}/ssh/upgrade", server.requireAuth(http.HandlerFunc(server.upgradeSSHExecutionTarget)))
	mux.Handle("POST /v1/tenants/{tenantID}/execution-targets/{executionTargetID}/ssh/revoke", server.requireAuth(http.HandlerFunc(server.revokeSSHExecutionTarget)))
	mux.Handle("GET /v1/tenants/{tenantID}/quota", server.requireAuth(http.HandlerFunc(server.getTenantQuota)))
	mux.Handle("PUT /v1/tenants/{tenantID}/quota", server.requireAuth(http.HandlerFunc(server.putTenantQuota)))
	mux.Handle("GET /v1/tenants/{tenantID}/retention-policy", server.requireAuth(http.HandlerFunc(server.getRetentionPolicy)))
	mux.Handle("PUT /v1/tenants/{tenantID}/retention-policy", server.requireAuth(http.HandlerFunc(server.putRetentionPolicy)))
	mux.Handle("GET /v1/tenants/{tenantID}/credentials", server.requireAuth(http.HandlerFunc(server.listCredentials)))
	mux.Handle("POST /v1/tenants/{tenantID}/credentials", server.requireAuth(http.HandlerFunc(server.createCredential)))
	mux.Handle("POST /v1/tenants/{tenantID}/credentials/{credentialID}/rotate", server.requireAuth(http.HandlerFunc(server.rotateCredential)))
	mux.Handle("POST /v1/tenants/{tenantID}/credentials/{credentialID}/revoke", server.requireAuth(http.HandlerFunc(server.revokeCredential)))
	mux.Handle("GET /v1/tenants/{tenantID}/identity-connections", server.requireAuth(http.HandlerFunc(server.listIdentityConnections)))
	mux.Handle("POST /v1/tenants/{tenantID}/identity-connections", server.requireAuth(http.HandlerFunc(server.createIdentityConnection)))
	mux.Handle("POST /v1/tenants/{tenantID}/identity-connections/{connectionID}/disable", server.requireAuth(http.HandlerFunc(server.disableIdentityConnection)))
	mux.Handle("GET /v1/tenants/{tenantID}/identity-connections/{connectionID}/group-mappings", server.requireAuth(http.HandlerFunc(server.listIdentityGroupMappings)))
	mux.Handle("PUT /v1/tenants/{tenantID}/identity-connections/{connectionID}/group-mappings", server.requireAuth(http.HandlerFunc(server.replaceIdentityGroupMappings)))
	mux.Handle("GET /v1/tenants/{tenantID}/service-accounts", server.requireAuth(http.HandlerFunc(server.listServiceAccounts)))
	mux.Handle("POST /v1/tenants/{tenantID}/service-accounts", server.requireAuth(http.HandlerFunc(server.createServiceAccount)))
	mux.Handle("POST /v1/tenants/{tenantID}/service-accounts/{serviceAccountID}/rotate-token", server.requireAuth(http.HandlerFunc(server.rotateServiceAccountToken)))
	mux.Handle("POST /v1/tenants/{tenantID}/service-accounts/{serviceAccountID}/revoke", server.requireAuth(http.HandlerFunc(server.revokeServiceAccount)))

	mux.Handle("GET /scim/v2/ServiceProviderConfig", server.requireServiceAccount(http.HandlerFunc(server.scimServiceProviderConfig)))
	mux.Handle("GET /scim/v2/ResourceTypes", server.requireServiceAccount(http.HandlerFunc(server.scimResourceTypes)))
	mux.Handle("GET /scim/v2/Schemas", server.requireServiceAccount(http.HandlerFunc(server.scimSchemas)))
	mux.Handle("GET /scim/v2/Users", server.requireServiceAccount(http.HandlerFunc(server.scimListUsers)))
	mux.Handle("POST /scim/v2/Users", server.requireServiceAccount(http.HandlerFunc(server.scimCreateUser)))
	mux.Handle("GET /scim/v2/Users/{userID}", server.requireServiceAccount(http.HandlerFunc(server.scimGetUser)))
	mux.Handle("PUT /scim/v2/Users/{userID}", server.requireServiceAccount(http.HandlerFunc(server.scimReplaceUser)))
	mux.Handle("PATCH /scim/v2/Users/{userID}", server.requireServiceAccount(http.HandlerFunc(server.scimPatchUser)))
	mux.Handle("DELETE /scim/v2/Users/{userID}", server.requireServiceAccount(http.HandlerFunc(server.scimDeleteUser)))
	mux.Handle("GET /scim/v2/Groups", server.requireServiceAccount(http.HandlerFunc(server.scimListGroups)))
	mux.Handle("POST /scim/v2/Groups", server.requireServiceAccount(http.HandlerFunc(server.scimCreateGroup)))
	mux.Handle("GET /scim/v2/Groups/{groupID}", server.requireServiceAccount(http.HandlerFunc(server.scimGetGroup)))
	mux.Handle("PUT /scim/v2/Groups/{groupID}", server.requireServiceAccount(http.HandlerFunc(server.scimReplaceGroup)))
	mux.Handle("PATCH /scim/v2/Groups/{groupID}", server.requireServiceAccount(http.HandlerFunc(server.scimPatchGroup)))
	mux.Handle("DELETE /scim/v2/Groups/{groupID}", server.requireServiceAccount(http.HandlerFunc(server.scimDeleteGroup)))

	mux.Handle("GET /v1/tenants/{tenantID}/organizations", server.requireAuth(http.HandlerFunc(server.listOrganizations)))
	mux.Handle("POST /v1/tenants/{tenantID}/organizations", server.requireAuth(http.HandlerFunc(server.createOrganization)))
	mux.Handle("GET /v1/tenants/{tenantID}/organizations/{organizationID}", server.requireAuth(http.HandlerFunc(server.getOrganization)))
	mux.Handle("PATCH /v1/tenants/{tenantID}/organizations/{organizationID}", server.requireAuth(http.HandlerFunc(server.updateOrganization)))
	mux.Handle("DELETE /v1/tenants/{tenantID}/organizations/{organizationID}", server.requireAuth(http.HandlerFunc(server.archiveOrganization)))
	mux.Handle("GET /v1/tenants/{tenantID}/organizations/{organizationID}/members", server.requireAuth(http.HandlerFunc(server.listOrganizationMembers)))
	mux.Handle("POST /v1/tenants/{tenantID}/organizations/{organizationID}/members", server.requireAuth(http.HandlerFunc(server.putOrganizationMember)))
	mux.Handle("PATCH /v1/tenants/{tenantID}/organizations/{organizationID}/members/{userID}", server.requireAuth(http.HandlerFunc(server.updateOrganizationMember)))
	mux.Handle("DELETE /v1/tenants/{tenantID}/organizations/{organizationID}/members/{userID}", server.requireAuth(http.HandlerFunc(server.removeOrganizationMember)))
	mux.Handle("GET /v1/tenants/{tenantID}/organizations/{organizationID}/projects", server.requireAuth(http.HandlerFunc(server.listProjects)))
	mux.Handle("POST /v1/tenants/{tenantID}/organizations/{organizationID}/projects", server.requireAuth(http.HandlerFunc(server.createProject)))

	mux.Handle("GET /v1/projects/{projectID}", server.requireAuth(http.HandlerFunc(server.getProject)))
	mux.Handle("PATCH /v1/projects/{projectID}", server.requireAuth(http.HandlerFunc(server.updateProject)))
	mux.Handle("DELETE /v1/projects/{projectID}", server.requireAuth(http.HandlerFunc(server.archiveProject)))
	mux.Handle("GET /v1/projects/{projectID}/sessions", server.requireAuth(http.HandlerFunc(server.listProjectSessions)))
	mux.Handle("POST /v1/projects/{projectID}/sessions", server.requireAuth(http.HandlerFunc(server.createSession)))

	mux.Handle("GET /v1/sessions/{sessionID}", server.requireAuth(http.HandlerFunc(server.getAgentSession)))
	mux.Handle("GET /v1/sessions/{sessionID}/events", server.requireAuth(http.HandlerFunc(server.listSessionEvents)))
	mux.Handle("GET /v1/sessions/{sessionID}/events/stream", server.requireAuth(http.HandlerFunc(server.streamSessionEvents)))
	mux.Handle("POST /v1/sessions/{sessionID}/turns", server.requireAuth(http.HandlerFunc(server.createTurn)))
	mux.Handle("POST /v1/sessions/{sessionID}/turns/active/steer", server.requireAuth(http.HandlerFunc(server.steerActiveTurn)))
	mux.Handle("POST /v1/sessions/{sessionID}/turns/active/interrupt", server.requireAuth(http.HandlerFunc(server.interruptActiveTurn)))
	mux.Handle("POST /v1/sessions/{sessionID}/suspend", server.requireAuth(http.HandlerFunc(server.suspendSession)))
	mux.Handle("POST /v1/sessions/{sessionID}/resume", server.requireAuth(http.HandlerFunc(server.resumeSession)))
	mux.Handle("POST /v1/sessions/{sessionID}/archive", server.requireAuth(http.HandlerFunc(server.archiveSession)))
	mux.Handle("POST /v1/executions/{executionID}/cancel", server.requireAuth(http.HandlerFunc(server.cancelExecution)))
	mux.Handle("GET /v1/executions/{executionID}/interactions", server.requireAuth(http.HandlerFunc(server.listExecutionInteractions)))
	mux.Handle("POST /v1/executions/{executionID}/approvals/{requestID}/resolve", server.requireAuth(http.HandlerFunc(server.resolveExecutionApproval)))
	mux.Handle("POST /v1/executions/{executionID}/user-input/{requestID}/resolve", server.requireAuth(http.HandlerFunc(server.resolveExecutionUserInput)))
	mux.Handle("GET /v1/sessions/{sessionID}/artifacts", server.requireAuth(http.HandlerFunc(server.listArtifacts)))
	mux.Handle("POST /v1/sessions/{sessionID}/artifacts", server.requireAuth(http.HandlerFunc(server.createArtifact)))
	mux.Handle("GET /v1/artifacts/{artifactID}", server.requireAuth(http.HandlerFunc(server.getArtifact)))
	mux.Handle("POST /v1/artifacts/{artifactID}/complete", server.requireAuth(http.HandlerFunc(server.completeArtifact)))
	mux.Handle("POST /v1/artifacts/{artifactID}/download", server.requireAuth(http.HandlerFunc(server.downloadArtifact)))
	mux.Handle("DELETE /v1/artifacts/{artifactID}", server.requireAuth(http.HandlerFunc(server.deleteArtifact)))
	mux.HandleFunc("PUT /v1/artifact-content/{artifactID}", server.uploadArtifactContent)
	mux.HandleFunc("GET /v1/artifact-content/{artifactID}", server.downloadArtifactContent)

	server.handler = server.withRequestContext(server.observeRequests(server.recoverPanics(server.securityHeaders(mux))))
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

func (s *Server) createTenant(w http.ResponseWriter, r *http.Request) {
	var input tenancy.CreateTenantInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.tenancy.CreateTenant(r.Context(), mustPrincipal(r), input, requestID(r), clientIP(r))
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
	items, err := s.projects.List(r.Context(), mustPrincipal(r), tenantID, organizationID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
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
	item, err := s.projects.Update(r.Context(), mustPrincipal(r), tenantID, projectID, input, requestID(r), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
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
	if err := s.projects.Archive(r.Context(), mustPrincipal(r), tenantID, projectID, requestID(r), clientIP(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listProjectSessions(w http.ResponseWriter, r *http.Request) {
	projectID, ok := s.pathUUID(w, r, "projectID")
	if !ok {
		return
	}
	items, err := s.sessions.ListByProject(r.Context(), mustPrincipal(r), projectID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
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
	limit, err := queryInt(r, "limit", 100)
	if err != nil {
		s.writeError(w, r, err)
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
	writeJSON(w, result.StatusCode, result.Value)
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
	writeJSON(w, result.StatusCode, result.Value)
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
	writeJSON(w, result.StatusCode, result.Value)
}

func (s *Server) listExecutionInteractions(w http.ResponseWriter, r *http.Request) {
	executionID, ok := s.pathUUID(w, r, "executionID")
	if !ok {
		return
	}
	items, err := s.executions.ListInteractions(r.Context(), mustPrincipal(r), executionID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
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
	writeJSON(w, result.StatusCode, result.Value)
}

func setIdempotencyReplayHeader(w http.ResponseWriter, replayed bool) {
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(s.config.CookieName)
		if err != nil {
			s.writeError(w, r, problem.New(401, "authentication_required", "Authentication is required."))
			return
		}
		principal, err := s.identity.Authenticate(r.Context(), cookie.Value)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		ctx := context.WithValue(r.Context(), principalContextKey{}, principal)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
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
		trace := incomingTraceID(r)
		if trace == "" {
			trace = randomHex(16)
		}
		span := randomHex(8)
		w.Header().Set("X-Request-ID", id)
		w.Header().Set("X-Trace-ID", trace)
		w.Header().Set("Traceparent", "00-"+trace+"-"+span+"-01")
		ctx := context.WithValue(r.Context(), requestIDContextKey{}, id)
		ctx = context.WithValue(ctx, traceIDContextKey{}, trace)
		ctx = context.WithValue(ctx, clientIPContextKey{}, s.resolveClientIP(r))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type responseStatusRecorder struct {
	http.ResponseWriter
	status int
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
		s.metrics.ObserveHTTP(r.Method, r.Pattern, status, duration)
		s.logger.Info("control-plane request completed",
			"requestId", requestID(r), "traceId", traceID(r), "method", r.Method,
			"route", normalizedLogRoute(r), "status", status, "durationMs", duration.Milliseconds(),
		)
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
	if !errors.As(err, &apiError) {
		apiError = problem.Wrap(500, "internal_error", "The control plane encountered an unexpected error.", err)
	}
	if apiError.Status >= 500 {
		s.logger.Error("control-plane request failed", "requestId", requestID(r), "traceId", traceID(r), "code", apiError.Code, "error", apiError)
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
