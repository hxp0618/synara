package executions

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/credentialscope"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	ProviderCredentialAccessStatusActive                = "active"
	ProviderCredentialAccessStatusRefreshWindowClosed   = "refresh-window-closed"
	ProviderCredentialAccessStatusCredentialUnavailable = "credential-unavailable"
	ProviderCredentialAccessStatusExpired               = "expired"
)

type ProviderCredentialGrantAccessResolution struct {
	Grant      persistence.ExecutionProviderCredentialGrant
	Credential persistence.ProviderCredential
	Access     ProviderCredentialAccess
}

type providerCredentialAccessProjection struct {
	Grant      persistence.ExecutionProviderCredentialGrant
	Credential *persistence.ProviderCredential
	Access     ProviderCredentialAccess
	Updates    map[string]any
}

type providerCredentialAccessSessionSnapshot struct {
	ID                         uuid.UUID  `gorm:"column:id"`
	TenantID                   uuid.UUID  `gorm:"column:tenant_id"`
	OrganizationID             uuid.UUID  `gorm:"column:organization_id"`
	CreatedBy                  uuid.UUID  `gorm:"column:created_by"`
	Model                      *string    `gorm:"column:model"`
	ResourceState              string     `gorm:"column:resource_state"`
	MeaningfulActivitySequence int64      `gorm:"column:meaningful_activity_sequence"`
	MeaningfulActivityAt       time.Time  `gorm:"column:meaningful_activity_at"`
	WaitingKeepAliveSeconds    int        `gorm:"column:waiting_keep_alive_seconds"`
	SuspendAfterIdleSeconds    int        `gorm:"column:suspend_after_idle_seconds"`
	AbsoluteExpiresAt          *time.Time `gorm:"column:absolute_expires_at"`
}

type providerCredentialAccessDurable struct {
	Attached          bool
	GrantID           uuid.UUID
	Serial            int64
	ActivitySequence  int64
	ActivityAt        time.Time
	IssuedAt          time.Time
	RenewedAt         time.Time
	ExpiresAt         time.Time
	RefreshDeadlineAt time.Time
	HardExpiresAt     *time.Time
}

func (s *Service) ResolveProviderCredentialGrantAccess(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
	executionID, grantID uuid.UUID,
	input LeaseInput,
) (ProviderCredentialGrantAccessResolution, error) {
	lease, execution, err := s.lockLease(ctx, tx, worker, executionID, input, true)
	if err != nil {
		return ProviderCredentialGrantAccessResolution{}, err
	}
	if err := requireExecutionTenantActive(ctx, tx, execution.TenantID); err != nil {
		return ProviderCredentialGrantAccessResolution{}, err
	}
	now := s.now()
	if err := s.requireExecutionSessionWithinAbsoluteLifetime(ctx, tx, execution, now); err != nil {
		return ProviderCredentialGrantAccessResolution{}, err
	}
	projection, err := s.projectProviderCredentialAccessLocked(
		ctx, tx, execution, lease, grantID, now, true,
	)
	if err != nil {
		return ProviderCredentialGrantAccessResolution{}, err
	}
	if err := applyProviderCredentialAccessUpdates(ctx, tx, lease, projection.Updates); err != nil {
		return ProviderCredentialGrantAccessResolution{}, err
	}
	if projection.Access.Status != ProviderCredentialAccessStatusActive || projection.Credential == nil {
		return ProviderCredentialGrantAccessResolution{}, providerCredentialAccessInactiveError(projection.Access)
	}
	return ProviderCredentialGrantAccessResolution{
		Grant: projection.Grant, Credential: *projection.Credential, Access: projection.Access,
	}, nil
}

func (s *Service) renewProviderCredentialAccessLocked(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	lease persistence.WorkerLease,
	now time.Time,
) (*ProviderCredentialAccess, map[string]any, error) {
	if lease.ProviderCredentialGrantID == nil {
		return nil, nil, nil
	}
	projection, err := s.projectProviderCredentialAccessLocked(
		ctx, tx, execution, lease, *lease.ProviderCredentialGrantID, now, false,
	)
	if err != nil {
		return nil, nil, err
	}
	return &projection.Access, projection.Updates, nil
}

func (s *Service) projectProviderCredentialAccessLocked(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	lease persistence.WorkerLease,
	grantID uuid.UUID,
	now time.Time,
	allowInitialAttach bool,
) (providerCredentialAccessProjection, error) {
	grant, err := loadExecutionProviderCredentialGrantByID(ctx, tx, execution, grantID)
	if err != nil {
		return providerCredentialAccessProjection{}, err
	}
	if execution.ProviderCredentialIDSnapshot == nil ||
		*execution.ProviderCredentialIDSnapshot != grant.CredentialID {
		return providerCredentialAccessProjection{}, problem.New(
			409,
			"provider_credential_grant_fenced",
			"Provider Credential Grant no longer matches the Execution generation snapshot.",
		)
	}
	if execution.ProviderCredentialVersionSnapshot == nil ||
		*execution.ProviderCredentialVersionSnapshot != grant.CredentialVersion {
		return providerCredentialAccessProjection{}, problem.New(
			409,
			"provider_credential_grant_version_fenced",
			"Provider Credential version no longer matches the Execution generation snapshot.",
		)
	}
	session, err := loadProviderCredentialAccessSessionSnapshot(ctx, tx, execution)
	if err != nil {
		return providerCredentialAccessProjection{}, err
	}
	if lease.ProviderCredentialGrantID != nil && *lease.ProviderCredentialGrantID != grant.ID {
		return providerCredentialAccessProjection{}, problem.New(
			409,
			"provider_credential_grant_fenced",
			"Provider Credential access state no longer matches the immutable Grant.",
		)
	}
	credential, unavailable, err := resolveExplicitProviderCredentialForGrant(
		ctx, tx, execution, session, grant, now,
	)
	if err != nil {
		return providerCredentialAccessProjection{}, err
	}
	return s.computeProviderCredentialAccessProjection(
		now, grant, session, lease, credential, unavailable, allowInitialAttach,
	), nil
}

func (s *Service) computeProviderCredentialAccessProjection(
	now time.Time,
	grant persistence.ExecutionProviderCredentialGrant,
	session providerCredentialAccessSessionSnapshot,
	lease persistence.WorkerLease,
	credential *persistence.ProviderCredential,
	credentialUnavailable bool,
	allowInitialAttach bool,
) providerCredentialAccessProjection {
	activitySequence := session.MeaningfulActivitySequence
	activityAt := session.MeaningfulActivityAt.UTC()
	if activityAt.IsZero() {
		activityAt = now
	}
	current := currentProviderCredentialAccessDurable(lease)
	if current.Attached {
		if activityAt.Before(current.ActivityAt) {
			activityAt = current.ActivityAt
		}
		if current.ActivitySequence == activitySequence {
			activityAt = current.ActivityAt
		}
	}
	renewalNow := now
	if current.Attached && current.RenewedAt.After(renewalNow) {
		renewalNow = current.RenewedAt
	}
	refreshDeadlineAt, refreshWindowOpen := providerCredentialRefreshWindow(session, activityAt, now)
	responseHardExpiresAt := providerCredentialHardExpiry(session.AbsoluteExpiresAt, current.HardExpiresAt, credential)
	hardCapTerminal := responseHardExpiresAt != nil && !responseHardExpiresAt.After(now)

	next := current
	if current.Attached && !hardCapTerminal {
		next.ActivitySequence = activitySequence
		next.ActivityAt = activityAt
		next.RefreshDeadlineAt = refreshDeadlineAt
		next.HardExpiresAt = providerCredentialDurableHardExpiry(
			current.ExpiresAt, current.HardExpiresAt, responseHardExpiresAt,
		)
	}
	if !current.Attached && credential != nil && refreshWindowOpen && allowInitialAttach && !hardCapTerminal {
		durableHardExpiresAt := providerCredentialDurableHardExpiry(
			time.Time{}, nil, responseHardExpiresAt,
		)
		next = providerCredentialAccessDurable{
			Attached:          true,
			GrantID:           grant.ID,
			Serial:            1,
			ActivitySequence:  activitySequence,
			ActivityAt:        activityAt,
			IssuedAt:          now,
			RenewedAt:         renewalNow,
			ExpiresAt:         providerCredentialDesiredExpiry(renewalNow, s.providerCredentialAccessTTL, durableHardExpiresAt),
			RefreshDeadlineAt: refreshDeadlineAt,
			HardExpiresAt:     durableHardExpiresAt,
		}
	}
	if next.Attached && credential != nil && refreshWindowOpen && !hardCapTerminal {
		desiredExpiry := providerCredentialDesiredExpiry(renewalNow, s.providerCredentialAccessTTL, next.HardExpiresAt)
		if current.Attached && desiredExpiry.Before(next.ExpiresAt) {
			desiredExpiry = next.ExpiresAt
		}
		next.ActivitySequence = activitySequence
		next.ActivityAt = activityAt
		next.RenewedAt = renewalNow
		next.ExpiresAt = desiredExpiry
		next.RefreshDeadlineAt = refreshDeadlineAt
	}

	next, updates := providerCredentialAccessDurableUpdates(current, next)
	effectiveExpiresAt := now
	if next.Attached {
		effectiveExpiresAt = next.ExpiresAt
		if responseHardExpiresAt != nil && responseHardExpiresAt.Before(effectiveExpiresAt) {
			effectiveExpiresAt = responseHardExpiresAt.UTC()
		}
	}
	access := ProviderCredentialAccess{
		GrantID:           grant.ID,
		Serial:            next.Serial,
		ActivitySequence:  activitySequence,
		ActivityAt:        activityAt,
		RefreshDeadlineAt: refreshDeadlineAt,
		HardExpiresAt:     responseHardExpiresAt,
		ExpiresAt:         effectiveExpiresAt,
		IssuedAt:          now,
		RenewedAt:         now,
	}
	if next.Attached {
		access.Serial = next.Serial
		access.ActivitySequence = next.ActivitySequence
		access.ActivityAt = next.ActivityAt
		access.IssuedAt = next.IssuedAt
		access.RenewedAt = next.RenewedAt
		access.ExpiresAt = effectiveExpiresAt
		access.RefreshDeadlineAt = next.RefreshDeadlineAt
		access.HardExpiresAt = responseHardExpiresAt
	}
	switch {
	case credentialUnavailable:
		access.Status = ProviderCredentialAccessStatusCredentialUnavailable
	case next.Attached && effectiveExpiresAt.After(now) && refreshWindowOpen:
		access.Status = ProviderCredentialAccessStatusActive
		access.RenewAfterAt = providerCredentialRenewAfter(renewalNow, effectiveExpiresAt, s.leaseTTL)
	case next.Attached && effectiveExpiresAt.After(now):
		access.Status = ProviderCredentialAccessStatusRefreshWindowClosed
		access.RenewAfterAt = providerCredentialRenewAfter(renewalNow, effectiveExpiresAt, s.leaseTTL)
	default:
		access.Status = ProviderCredentialAccessStatusExpired
	}
	return providerCredentialAccessProjection{
		Grant: grant, Credential: credential, Access: access, Updates: updates,
	}
}

func currentProviderCredentialAccessDurable(lease persistence.WorkerLease) providerCredentialAccessDurable {
	if lease.ProviderCredentialGrantID == nil ||
		lease.ProviderCredentialAccessSerial == nil ||
		lease.ProviderCredentialActivitySequence == nil ||
		lease.ProviderCredentialActivityAt == nil ||
		lease.ProviderCredentialAccessIssuedAt == nil ||
		lease.ProviderCredentialAccessRenewedAt == nil ||
		lease.ProviderCredentialAccessExpiresAt == nil ||
		lease.ProviderCredentialRefreshDeadlineAt == nil {
		return providerCredentialAccessDurable{}
	}
	return providerCredentialAccessDurable{
		Attached:          true,
		GrantID:           *lease.ProviderCredentialGrantID,
		Serial:            *lease.ProviderCredentialAccessSerial,
		ActivitySequence:  *lease.ProviderCredentialActivitySequence,
		ActivityAt:        lease.ProviderCredentialActivityAt.UTC(),
		IssuedAt:          lease.ProviderCredentialAccessIssuedAt.UTC(),
		RenewedAt:         lease.ProviderCredentialAccessRenewedAt.UTC(),
		ExpiresAt:         lease.ProviderCredentialAccessExpiresAt.UTC(),
		RefreshDeadlineAt: lease.ProviderCredentialRefreshDeadlineAt.UTC(),
		HardExpiresAt:     copyTimePtr(lease.ProviderCredentialHardExpiresAt),
	}
}

func providerCredentialAccessDurableUpdates(
	current providerCredentialAccessDurable,
	next providerCredentialAccessDurable,
) (providerCredentialAccessDurable, map[string]any) {
	if !next.Attached {
		return next, nil
	}
	if !current.Attached {
		return next, map[string]any{
			"provider_credential_grant_id":            next.GrantID,
			"provider_credential_access_serial":       next.Serial,
			"provider_credential_activity_sequence":   next.ActivitySequence,
			"provider_credential_activity_at":         next.ActivityAt,
			"provider_credential_access_issued_at":    next.IssuedAt,
			"provider_credential_access_renewed_at":   next.RenewedAt,
			"provider_credential_access_expires_at":   next.ExpiresAt,
			"provider_credential_refresh_deadline_at": next.RefreshDeadlineAt,
			"provider_credential_hard_expires_at":     next.HardExpiresAt,
		}
	}
	payloadChanged := current.ActivitySequence != next.ActivitySequence ||
		!current.ActivityAt.Equal(next.ActivityAt) ||
		!current.RenewedAt.Equal(next.RenewedAt) ||
		!current.ExpiresAt.Equal(next.ExpiresAt) ||
		!current.RefreshDeadlineAt.Equal(next.RefreshDeadlineAt) ||
		!equalOptionalTimes(current.HardExpiresAt, next.HardExpiresAt)
	if !payloadChanged {
		return next, nil
	}
	next.Serial = current.Serial + 1
	return next, map[string]any{
		"provider_credential_access_serial":       next.Serial,
		"provider_credential_activity_sequence":   next.ActivitySequence,
		"provider_credential_activity_at":         next.ActivityAt,
		"provider_credential_access_renewed_at":   next.RenewedAt,
		"provider_credential_access_expires_at":   next.ExpiresAt,
		"provider_credential_refresh_deadline_at": next.RefreshDeadlineAt,
		"provider_credential_hard_expires_at":     next.HardExpiresAt,
	}
}

func loadExecutionProviderCredentialGrantByID(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	grantID uuid.UUID,
) (persistence.ExecutionProviderCredentialGrant, error) {
	var grant persistence.ExecutionProviderCredentialGrant
	err := tx.WithContext(ctx).
		Where(
			"tenant_id = ? AND execution_id = ? AND generation = ? AND id = ?",
			execution.TenantID, execution.ID, execution.Generation, grantID,
		).
		Take(&grant).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.ExecutionProviderCredentialGrant{}, problem.New(
			404,
			"provider_credential_grant_not_found",
			"Provider Credential Grant not found.",
		)
	}
	if err != nil {
		return persistence.ExecutionProviderCredentialGrant{}, problem.Wrap(
			500,
			"provider_credential_grant_load_failed",
			"Provider Credential Grant could not be loaded.",
			err,
		)
	}
	return grant, nil
}

func loadProviderCredentialAccessSessionSnapshot(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
) (providerCredentialAccessSessionSnapshot, error) {
	var session providerCredentialAccessSessionSnapshot
	err := tx.WithContext(ctx).
		Table("agent_sessions").
		Select(stringsJoin([]string{
			"id",
			"tenant_id",
			"organization_id",
			"created_by",
			"model",
			"resource_state",
			"meaningful_activity_sequence",
			"meaningful_activity_at",
			"waiting_keep_alive_seconds",
			"suspend_after_idle_seconds",
			"absolute_expires_at",
		})).
		Where("tenant_id = ? AND id = ?", execution.TenantID, execution.SessionID).
		Take(&session).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return providerCredentialAccessSessionSnapshot{}, problem.New(404, "session_not_found", "Agent Session not found.")
	}
	if err != nil {
		return providerCredentialAccessSessionSnapshot{}, problem.Wrap(500, "session_load_failed", "Agent Session could not be loaded.", err)
	}
	return session, nil
}

func resolveExplicitProviderCredentialForGrant(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	session providerCredentialAccessSessionSnapshot,
	grant persistence.ExecutionProviderCredentialGrant,
	now time.Time,
) (*persistence.ProviderCredential, bool, error) {
	if execution.Provider == nil {
		return nil, false, problem.New(
			500,
			"execution_provider_snapshot_invalid",
			"Execution Provider snapshot is invalid.",
		)
	}
	selection, err := credentialscope.Resolve(ctx, tx, credentialscope.Request{
		TenantID: execution.TenantID, OrganizationID: session.OrganizationID,
		SessionOwnerUserID: session.CreatedBy, Provider: *execution.Provider, Model: session.Model,
		ExplicitCredentialID: &grant.CredentialID, Now: now,
	})
	if err != nil {
		var apiError *problem.Error
		if errors.As(err, &apiError) &&
			(apiError.Code == "credential_not_found" ||
				apiError.Code == "credential_unavailable" ||
				apiError.Code == "platform_credential_not_entitled") {
			return nil, true, nil
		}
		return nil, false, err
	}
	if selection == nil {
		return nil, true, nil
	}
	credential := selection.Credential
	if credential.Version != grant.CredentialVersion {
		return nil, true, nil
	}
	if credential.RevokedAt != nil || (credential.ExpiresAt != nil && !credential.ExpiresAt.After(now)) {
		return nil, true, nil
	}
	return &credential, false, nil
}

func applyProviderCredentialAccessUpdates(
	ctx context.Context,
	tx *gorm.DB,
	lease persistence.WorkerLease,
	updates map[string]any,
) error {
	if len(updates) == 0 {
		return nil
	}
	result := tx.WithContext(ctx).
		Model(&persistence.WorkerLease{}).
		Where(
			"tenant_id = ? AND execution_id = ? AND worker_id = ? AND generation = ?",
			lease.TenantID, lease.ExecutionID, lease.WorkerID, lease.Generation,
		).
		Updates(updates)
	return expectOne(
		result,
		409,
		"provider_credential_access_update_failed",
		"Provider Credential access state could not be persisted.",
	)
}

func providerCredentialAccessInactiveError(access ProviderCredentialAccess) error {
	code := "provider_credential_access_expired"
	message := "Provider Credential access has expired."
	switch access.Status {
	case ProviderCredentialAccessStatusRefreshWindowClosed:
		code = "provider_credential_access_refresh_window_closed"
		message = "Provider Credential access is outside its refresh window."
	case ProviderCredentialAccessStatusCredentialUnavailable:
		code = "provider_credential_access_unavailable"
		message = "Provider Credential is unavailable for this Execution generation."
	}
	apiError := problem.New(409, code, message)
	apiError.Details = map[string]any{"access": access}
	return apiError
}

func providerCredentialRefreshWindow(
	session providerCredentialAccessSessionSnapshot,
	activityAt, now time.Time,
) (time.Time, bool) {
	var seconds int
	switch session.ResourceState {
	case "waiting", "checkpointing":
		seconds = session.WaitingKeepAliveSeconds
	case "provisioning", "active", "restoring":
		seconds = session.SuspendAfterIdleSeconds
	default:
		return activityAt, false
	}
	if seconds <= 0 {
		return activityAt, false
	}
	deadline := activityAt.Add(time.Duration(seconds) * time.Second)
	return deadline, deadline.After(now)
}

func providerCredentialHardExpiry(
	sessionAbsolute *time.Time,
	currentHardExpiresAt *time.Time,
	credential *persistence.ProviderCredential,
) *time.Time {
	hard := copyTimePtr(currentHardExpiresAt)
	if sessionAbsolute != nil {
		value := sessionAbsolute.UTC()
		if hard == nil || value.Before(*hard) {
			hard = &value
		}
	}
	if credential != nil && credential.ExpiresAt != nil {
		value := credential.ExpiresAt.UTC()
		if hard == nil || value.Before(*hard) {
			hard = &value
		}
	}
	return hard
}

func providerCredentialDurableHardExpiry(
	currentExpiresAt time.Time,
	currentHardExpiresAt *time.Time,
	responseHardExpiresAt *time.Time,
) *time.Time {
	durable := copyTimePtr(currentHardExpiresAt)
	if responseHardExpiresAt == nil {
		return durable
	}
	if currentExpiresAt.IsZero() {
		return copyTimePtr(responseHardExpiresAt)
	}
	if responseHardExpiresAt.Before(currentExpiresAt) {
		return durable
	}
	if durable == nil || responseHardExpiresAt.Before(*durable) {
		return copyTimePtr(responseHardExpiresAt)
	}
	return durable
}

func providerCredentialDesiredExpiry(
	now time.Time,
	ttl time.Duration,
	hardExpiresAt *time.Time,
) time.Time {
	expiresAt := now.Add(ttl)
	if hardExpiresAt != nil && hardExpiresAt.Before(expiresAt) {
		expiresAt = hardExpiresAt.UTC()
	}
	return expiresAt
}

func providerCredentialRenewAfter(
	now, expiresAt time.Time,
	leaseTTL time.Duration,
) *time.Time {
	candidate := now.Add(leaseTTL)
	limit := expiresAt.Add(-time.Nanosecond)
	if !limit.After(now) {
		return nil
	}
	if candidate.After(limit) {
		candidate = limit
	}
	if !candidate.After(now) {
		return nil
	}
	return &candidate
}

func equalOptionalTimes(left, right *time.Time) bool {
	switch {
	case left == nil && right == nil:
		return true
	case left == nil || right == nil:
		return false
	default:
		return left.Equal(*right)
	}
}

func copyTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := value.UTC()
	return &copy
}

func stringsJoin(values []string) string {
	if len(values) == 0 {
		return ""
	}
	joined := values[0]
	for _, value := range values[1:] {
		joined += ", " + value
	}
	return joined
}
