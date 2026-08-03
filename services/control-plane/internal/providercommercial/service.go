package providercommercial

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/governanceauthority"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

var authorizationKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,119}$`)
var regionPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,63}$`)
var documentSHA256Pattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var requiredApprovalRoles = []string{"legal", "privacy", "security", "product"}

type Approval struct {
	ID                uuid.UUID `json:"id"`
	Role              string    `json:"role"`
	Decision          string    `json:"decision"`
	ApproverUserID    uuid.UUID `json:"approverUserId"`
	Reason            string    `json:"reason"`
	EvidenceReference string    `json:"evidenceReference"`
	EvidenceSHA256    *string   `json:"evidenceSha256"`
	CreatedAt         time.Time `json:"createdAt"`
}

type Authorization struct {
	ID                          uuid.UUID  `json:"id"`
	AuthorizationKey            string     `json:"authorizationKey"`
	Provider                    string     `json:"provider"`
	ProviderProduct             string     `json:"providerProduct"`
	AccountType                 string     `json:"accountType"`
	ContractingEntity           string     `json:"contractingEntity"`
	CredentialMode              string     `json:"credentialMode"`
	AllowedCredentialScopes     []string   `json:"allowedCredentialScopes"`
	AllowedRegions              []string   `json:"allowedRegions"`
	DataUsePolicy               string     `json:"dataUsePolicy"`
	RetentionPolicy             string     `json:"retentionPolicy"`
	TermsEffectiveAt            time.Time  `json:"termsEffectiveAt"`
	TermsReference              string     `json:"termsReference"`
	TermsSHA256                 *string    `json:"termsSha256"`
	AgreementReference          string     `json:"agreementReference"`
	AgreementSHA256             *string    `json:"agreementSha256"`
	DPAReference                string     `json:"dpaReference"`
	DPASHA256                   *string    `json:"dpaSha256"`
	ProhibitedUseSummary        string     `json:"prohibitedUseSummary"`
	TerminationRunbookReference string     `json:"terminationRunbookReference"`
	TerminationRunbookSHA256    *string    `json:"terminationRunbookSha256"`
	ReviewExpiresAt             time.Time  `json:"reviewExpiresAt"`
	State                       string     `json:"state"`
	Version                     int64      `json:"version"`
	CreatedBy                   uuid.UUID  `json:"createdBy"`
	Approvals                   []Approval `json:"approvals"`
	ActivatedAt                 *time.Time `json:"activatedAt"`
	RejectedAt                  *time.Time `json:"rejectedAt"`
	RevokedAt                   *time.Time `json:"revokedAt"`
	CreatedAt                   time.Time  `json:"createdAt"`
	UpdatedAt                   time.Time  `json:"updatedAt"`
}

type CreateInput struct {
	AuthorizationKey            string    `json:"authorizationKey"`
	Provider                    string    `json:"provider"`
	ProviderProduct             string    `json:"providerProduct"`
	AccountType                 string    `json:"accountType"`
	ContractingEntity           string    `json:"contractingEntity"`
	CredentialMode              string    `json:"credentialMode"`
	AllowedCredentialScopes     []string  `json:"allowedCredentialScopes"`
	AllowedRegions              []string  `json:"allowedRegions"`
	DataUsePolicy               string    `json:"dataUsePolicy"`
	RetentionPolicy             string    `json:"retentionPolicy"`
	TermsEffectiveAt            time.Time `json:"termsEffectiveAt"`
	TermsReference              string    `json:"termsReference"`
	TermsSHA256                 string    `json:"termsSha256"`
	AgreementReference          string    `json:"agreementReference"`
	AgreementSHA256             string    `json:"agreementSha256"`
	DPAReference                string    `json:"dpaReference"`
	DPASHA256                   string    `json:"dpaSha256"`
	ProhibitedUseSummary        string    `json:"prohibitedUseSummary"`
	TerminationRunbookReference string    `json:"terminationRunbookReference"`
	TerminationRunbookSHA256    string    `json:"terminationRunbookSha256"`
	ReviewExpiresAt             time.Time `json:"reviewExpiresAt"`
}

type ApprovalInput struct {
	Role              string `json:"role"`
	Decision          string `json:"decision"`
	Reason            string `json:"reason"`
	EvidenceReference string `json:"evidenceReference"`
	EvidenceSHA256    string `json:"evidenceSha256"`
}

type TransitionInput struct {
	ExpectedVersion int64  `json:"expectedVersion"`
	TargetState     string `json:"targetState"`
	Reason          string `json:"reason"`
}

type HostedExecutionInput struct {
	TenantID          uuid.UUID
	Provider          string
	TargetKind        string
	PlacementRegion   string
	CredentialID      *uuid.UUID
	CredentialVersion *int
}

type Service struct {
	db               *gorm.DB
	operatorTenantID uuid.UUID
	now              func() time.Time
	authority        *governanceauthority.Service
}

func NewService(db *gorm.DB, operatorTenantID uuid.UUID) *Service {
	return &Service{db: db, operatorTenantID: operatorTenantID, now: func() time.Time { return time.Now().UTC() }, authority: governanceauthority.NewService(db, operatorTenantID)}
}

func (s *Service) List(ctx context.Context) ([]Authorization, error) {
	var models []persistence.ProviderCommercialAuthorization
	if err := s.db.WithContext(ctx).Where("operator_tenant_id = ?", s.operatorTenantID).
		Order("created_at DESC, id DESC").Limit(100).Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "provider_commercial_authorizations_load_failed", "Provider commercial authorizations could not be loaded.", err)
	}
	items := make([]Authorization, 0, len(models))
	for _, model := range models {
		item, err := s.view(ctx, model)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (s *Service) Create(ctx context.Context, actorID uuid.UUID, input CreateInput, requestID, ipAddress string) (Authorization, error) {
	model, err := s.normalizeCreate(actorID, input)
	if err != nil {
		return Authorization{}, err
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&model).Error; errors.Is(err, gorm.ErrDuplicatedKey) {
			return problem.New(409, "provider_commercial_authorization_exists", "This Provider commercial authorization already exists.")
		} else if err != nil {
			return problem.Wrap(500, "provider_commercial_authorization_create_failed", "Provider commercial authorization could not be created.", err)
		}
		return s.recordAudit(ctx, tx, actorID, "provider.commercial_authorization_created", model.ID, requestID, ipAddress, map[string]any{
			"authorizationKey": model.AuthorizationKey, "provider": publicProvider(model.Provider),
			"credentialMode": model.CredentialMode, "allowedRegions": model.AllowedRegions,
			"documentDigests": map[string]*string{
				"terms": model.TermsSHA256, "agreement": model.AgreementSHA256,
				"dpa": model.DPASHA256, "terminationRunbook": model.TerminationRunbookSHA256,
			},
		})
	})
	if err != nil {
		return Authorization{}, err
	}
	return s.get(ctx, model.ID)
}

func (s *Service) RecordApproval(ctx context.Context, actorID, authorizationID uuid.UUID, input ApprovalInput, requestID, ipAddress string) (Authorization, error) {
	input.Role = strings.ToLower(strings.TrimSpace(input.Role))
	input.Decision = strings.ToLower(strings.TrimSpace(input.Decision))
	input.Reason = strings.TrimSpace(input.Reason)
	input.EvidenceReference = strings.TrimSpace(input.EvidenceReference)
	input.EvidenceSHA256 = strings.TrimSpace(input.EvidenceSHA256)
	if !slices.Contains(requiredApprovalRoles, input.Role) || !slices.Contains([]string{"approved", "rejected"}, input.Decision) ||
		len(input.Reason) < 10 || len(input.Reason) > 2000 || !validHTTPSReference(input.EvidenceReference) || !validDocumentSHA256(input.EvidenceSHA256) {
		return Authorization{}, problem.New(400, "provider_commercial_approval_invalid", "Provider commercial approval requires a valid role, decision, reason, HTTPS evidence reference and non-zero SHA-256.")
	}
	approval := persistence.ProviderCommercialAuthorizationApproval{
		ID: uuid.New(), AuthorizationID: authorizationID, OperatorTenantID: s.operatorTenantID,
		ApprovalRole: input.Role, Decision: input.Decision, ApproverUserID: actorID,
		Reason: input.Reason, EvidenceReference: input.EvidenceReference, EvidenceSHA256: &input.EvidenceSHA256, CreatedAt: s.now(),
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		authorization, err := s.lock(ctx, tx, authorizationID)
		if err != nil {
			return err
		}
		if authorization.State != "ready_for_review" || authorization.CreatedBy == actorID || !authorization.ReviewExpiresAt.After(s.now()) {
			return problem.New(409, "provider_commercial_authorization_not_reviewable", "Provider commercial authorization is not reviewable by this operator.")
		}
		if err := s.authority.RequireWithDB(ctx, tx, actorID, governanceauthority.ProviderCommercialPrefix+input.Role); err != nil {
			return err
		}
		if err := tx.Create(&approval).Error; errors.Is(err, gorm.ErrDuplicatedKey) {
			return problem.New(409, "provider_commercial_approval_conflict", "This role or operator already recorded a decision.")
		} else if err != nil {
			return problem.Wrap(500, "provider_commercial_approval_create_failed", "Provider commercial approval could not be recorded.", err)
		}
		if err := s.recordAudit(ctx, tx, actorID, "provider.commercial_approval_recorded", authorization.ID, requestID, ipAddress, map[string]any{
			"authorizationKey": authorization.AuthorizationKey, "role": approval.ApprovalRole,
			"decision": approval.Decision, "evidenceSha256": approval.EvidenceSHA256,
		}); err != nil {
			return err
		}
		if approval.Decision != "rejected" {
			return nil
		}
		now := s.now()
		result := tx.Model(&persistence.ProviderCommercialAuthorization{}).
			Where("id = ? AND version = ? AND state = ?", authorization.ID, authorization.Version, "ready_for_review").
			Updates(map[string]any{"state": "rejected", "version": authorization.Version + 1, "rejected_at": now, "updated_at": now})
		if result.Error != nil {
			return problem.Wrap(500, "provider_commercial_authorization_reject_failed", "Provider commercial authorization could not be rejected.", result.Error)
		}
		if result.RowsAffected != 1 {
			return problem.New(409, "provider_commercial_authorization_version_conflict", "Provider commercial authorization changed before rejection.")
		}
		return s.recordAudit(ctx, tx, actorID, "provider.commercial_authorization_rejected", authorization.ID, requestID, ipAddress, map[string]any{"authorizationKey": authorization.AuthorizationKey, "role": approval.ApprovalRole})
	})
	if err != nil {
		return Authorization{}, err
	}
	return s.get(ctx, authorizationID)
}

func (s *Service) Transition(ctx context.Context, actorID, authorizationID uuid.UUID, input TransitionInput, requestID, ipAddress string) (Authorization, error) {
	input.TargetState = strings.ToLower(strings.TrimSpace(input.TargetState))
	input.Reason = strings.TrimSpace(input.Reason)
	if input.ExpectedVersion <= 0 || !slices.Contains([]string{"ready_for_review", "active", "revoked"}, input.TargetState) || len(input.Reason) < 10 || len(input.Reason) > 1000 {
		return Authorization{}, problem.New(400, "provider_commercial_transition_invalid", "Provider commercial transition requires a version, valid target and bounded reason.")
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		authorization, err := s.lock(ctx, tx, authorizationID)
		if err != nil {
			return err
		}
		valid := authorization.State == "draft" && input.TargetState == "ready_for_review" ||
			authorization.State == "ready_for_review" && input.TargetState == "active" ||
			authorization.State == "active" && input.TargetState == "revoked"
		if authorization.Version != input.ExpectedVersion || !valid {
			return problem.New(409, "provider_commercial_authorization_version_conflict", "Provider commercial authorization state or version changed.")
		}
		if input.TargetState == "active" {
			if !authorization.ReviewExpiresAt.After(s.now()) {
				return problem.New(409, "provider_commercial_authorization_expired", "Provider commercial review expired before activation.")
			}
			var approvals int64
			if err := tx.Model(&persistence.ProviderCommercialAuthorizationApproval{}).
				Where("authorization_id = ? AND decision = ? AND approval_role IN ? AND evidence_sha256 IS NOT NULL", authorization.ID, "approved", requiredApprovalRoles).
				Count(&approvals).Error; err != nil {
				return problem.Wrap(500, "provider_commercial_approvals_load_failed", "Provider commercial approvals could not be verified.", err)
			}
			if approvals != 4 {
				return problem.New(409, "provider_commercial_approvals_incomplete", "Legal, Privacy, Security and Product approvals are required.")
			}
		}
		now := s.now()
		updates := map[string]any{"state": input.TargetState, "version": authorization.Version + 1, "updated_at": now}
		if input.TargetState == "active" {
			updates["activated_at"] = now
		} else if input.TargetState == "revoked" {
			updates["revoked_at"] = now
		}
		result := tx.Model(&persistence.ProviderCommercialAuthorization{}).
			Where("id = ? AND operator_tenant_id = ? AND version = ? AND state = ?", authorization.ID, s.operatorTenantID, authorization.Version, authorization.State).
			Updates(updates)
		if result.Error != nil {
			return problem.Wrap(500, "provider_commercial_authorization_transition_failed", "Provider commercial authorization could not be transitioned.", result.Error)
		}
		if result.RowsAffected != 1 {
			return problem.New(409, "provider_commercial_authorization_version_conflict", "Provider commercial authorization changed before transition.")
		}
		return s.recordAudit(ctx, tx, actorID, "provider.commercial_authorization_transitioned", authorization.ID, requestID, ipAddress, map[string]any{
			"authorizationKey": authorization.AuthorizationKey, "from": authorization.State, "to": input.TargetState, "reason": input.Reason,
		})
	})
	if err != nil {
		return Authorization{}, err
	}
	return s.get(ctx, authorizationID)
}

func RequireHostedExecution(ctx context.Context, tx *gorm.DB, operatorTenantID uuid.UUID, input HostedExecutionInput, now time.Time) error {
	if !platform.IsRemoteTarget(platform.ExecutionTargetKind(input.TargetKind)) || strings.TrimSpace(input.Provider) == "" {
		return nil
	}
	provider, ok := storageProvider(input.Provider)
	region := strings.TrimSpace(strings.ToLower(input.PlacementRegion))
	if input.TenantID != uuid.Nil && region == "" {
		var tenant persistence.Tenant
		if err := tx.WithContext(ctx).Select("region").Where("id = ?", input.TenantID).Take(&tenant).Error; err == nil {
			region = strings.TrimSpace(strings.ToLower(tenant.Region))
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.Wrap(500, "provider_commercial_region_load_failed", "Hosted Provider Region could not be verified.", err)
		}
	}
	if !ok || input.TenantID == uuid.Nil || input.CredentialID == nil || input.CredentialVersion == nil || *input.CredentialVersion <= 0 || !regionPattern.MatchString(region) {
		return problem.New(409, "provider_commercial_authorization_required", "Hosted Provider execution requires an active commercial authorization, approved Region and commercial credential.")
	}
	var credential persistence.ProviderCredential
	if err := tx.WithContext(ctx).
		Where("id = ? AND tenant_id = ? AND version = ? AND purpose = ? AND revoked_at IS NULL", *input.CredentialID, input.TenantID, *input.CredentialVersion, "provider").
		Take(&credential).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return problem.New(409, "provider_commercial_credential_required", "Hosted Provider execution requires the exact active commercial credential snapshot.")
	} else if err != nil {
		return problem.Wrap(500, "provider_commercial_credential_load_failed", "Hosted Provider credential could not be verified.", err)
	}
	credentialProvider, providerOK := storageProvider(credential.Provider)
	if !providerOK || credentialProvider != provider || credential.ExpiresAt != nil && !credential.ExpiresAt.After(now) {
		return problem.New(409, "provider_commercial_credential_required", "Hosted Provider execution requires the exact active commercial credential snapshot.")
	}
	credentialMode := "customer_byok"
	if credential.Scope == "platform" {
		credentialMode = "platform_managed"
	}
	var authorizations []persistence.ProviderCommercialAuthorization
	if err := tx.WithContext(ctx).
		Where(`operator_tenant_id = ? AND provider = ? AND state = ? AND review_expires_at > ? AND credential_mode = ?
			AND terms_sha256 IS NOT NULL AND agreement_sha256 IS NOT NULL AND dpa_sha256 IS NOT NULL AND termination_runbook_sha256 IS NOT NULL
			AND 4 = (SELECT count(*) FROM provider_commercial_authorization_approvals AS approval
			  WHERE approval.authorization_id = provider_commercial_authorizations.id AND approval.decision = 'approved'
			    AND approval.approval_role IN ('legal', 'privacy', 'security', 'product') AND approval.evidence_sha256 IS NOT NULL)`, operatorTenantID, provider, "active", now, credentialMode).
		Find(&authorizations).Error; err != nil {
		return problem.Wrap(500, "provider_commercial_authorization_load_failed", "Provider commercial authorization could not be verified.", err)
	}
	for _, authorization := range authorizations {
		if slices.Contains(authorization.AllowedCredentialScopes, credential.Scope) && slices.Contains(authorization.AllowedRegions, region) {
			return nil
		}
	}
	return problem.New(409, "provider_commercial_authorization_required", "Hosted Provider execution requires an active commercial authorization matching Provider, Region and credential scope.")
}

func (s *Service) normalizeCreate(actorID uuid.UUID, input CreateInput) (persistence.ProviderCommercialAuthorization, error) {
	input.AuthorizationKey = strings.ToLower(strings.TrimSpace(input.AuthorizationKey))
	provider, providerOK := storageProvider(input.Provider)
	input.ProviderProduct = strings.TrimSpace(input.ProviderProduct)
	input.AccountType = strings.TrimSpace(input.AccountType)
	input.ContractingEntity = strings.TrimSpace(input.ContractingEntity)
	input.CredentialMode = strings.ToLower(strings.TrimSpace(input.CredentialMode))
	input.DataUsePolicy = strings.ToLower(strings.TrimSpace(input.DataUsePolicy))
	input.RetentionPolicy = strings.TrimSpace(input.RetentionPolicy)
	input.ProhibitedUseSummary = strings.TrimSpace(input.ProhibitedUseSummary)
	input.AllowedCredentialScopes = normalizeSet(input.AllowedCredentialScopes)
	input.AllowedRegions = normalizeSet(input.AllowedRegions)
	refs := []*string{&input.TermsReference, &input.AgreementReference, &input.DPAReference, &input.TerminationRunbookReference}
	for _, ref := range refs {
		*ref = strings.TrimSpace(*ref)
	}
	digests := []*string{&input.TermsSHA256, &input.AgreementSHA256, &input.DPASHA256, &input.TerminationRunbookSHA256}
	for _, digest := range digests {
		*digest = strings.TrimSpace(*digest)
	}
	now := s.now()
	if s.operatorTenantID == uuid.Nil || actorID == uuid.Nil || !authorizationKeyPattern.MatchString(input.AuthorizationKey) || !providerOK ||
		len(input.ProviderProduct) < 2 || len(input.ProviderProduct) > 200 || len(input.AccountType) < 2 || len(input.AccountType) > 200 ||
		len(input.ContractingEntity) < 2 || len(input.ContractingEntity) > 300 || !slices.Contains([]string{"customer_byok", "platform_managed"}, input.CredentialMode) ||
		!validScopes(input.AllowedCredentialScopes) || !validRegions(input.AllowedRegions) ||
		!slices.Contains([]string{"no_training", "tenant_explicit_opt_in"}, input.DataUsePolicy) || len(input.RetentionPolicy) < 3 || len(input.RetentionPolicy) > 1000 ||
		input.TermsEffectiveAt.IsZero() || !input.ReviewExpiresAt.After(now) || input.ReviewExpiresAt.After(now.Add(180*24*time.Hour)) ||
		len(input.ProhibitedUseSummary) < 20 || len(input.ProhibitedUseSummary) > 4000 {
		return persistence.ProviderCommercialAuthorization{}, problem.New(400, "provider_commercial_authorization_invalid", "Provider commercial authorization identity, scope, policy or review window is invalid.")
	}
	for _, ref := range refs {
		if !validHTTPSReference(*ref) {
			return persistence.ProviderCommercialAuthorization{}, problem.New(400, "provider_commercial_reference_invalid", "Provider commercial references must be credential-free HTTPS URLs without query or fragment.")
		}
	}
	for _, digest := range digests {
		if !validDocumentSHA256(*digest) {
			return persistence.ProviderCommercialAuthorization{}, problem.New(400, "provider_commercial_digest_invalid", "Provider commercial document digests must be non-zero sha256:<64 lowercase hex> values.")
		}
	}
	return persistence.ProviderCommercialAuthorization{
		ID: uuid.New(), OperatorTenantID: s.operatorTenantID, AuthorizationKey: input.AuthorizationKey, Provider: provider,
		ProviderProduct: input.ProviderProduct, AccountType: input.AccountType, ContractingEntity: input.ContractingEntity,
		CredentialMode: input.CredentialMode, AllowedCredentialScopes: input.AllowedCredentialScopes, AllowedRegions: input.AllowedRegions,
		DataUsePolicy: input.DataUsePolicy, RetentionPolicy: input.RetentionPolicy, TermsEffectiveAt: input.TermsEffectiveAt.UTC(),
		TermsReference: input.TermsReference, TermsSHA256: &input.TermsSHA256,
		AgreementReference: input.AgreementReference, AgreementSHA256: &input.AgreementSHA256,
		DPAReference: input.DPAReference, DPASHA256: &input.DPASHA256,
		ProhibitedUseSummary: input.ProhibitedUseSummary, TerminationRunbookReference: input.TerminationRunbookReference,
		TerminationRunbookSHA256: &input.TerminationRunbookSHA256,
		ReviewExpiresAt:          input.ReviewExpiresAt.UTC(), State: "draft", Version: 1, CreatedBy: actorID, CreatedAt: now, UpdatedAt: now,
	}, nil
}

func (s *Service) lock(ctx context.Context, tx *gorm.DB, id uuid.UUID) (persistence.ProviderCommercialAuthorization, error) {
	var model persistence.ProviderCommercialAuthorization
	if err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND operator_tenant_id = ?", id, s.operatorTenantID).First(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return model, problem.New(404, "provider_commercial_authorization_not_found", "Provider commercial authorization was not found.")
	} else if err != nil {
		return model, problem.Wrap(500, "provider_commercial_authorization_load_failed", "Provider commercial authorization could not be loaded.", err)
	}
	return model, nil
}

func (s *Service) get(ctx context.Context, id uuid.UUID) (Authorization, error) {
	var model persistence.ProviderCommercialAuthorization
	if err := s.db.WithContext(ctx).Where("id = ? AND operator_tenant_id = ?", id, s.operatorTenantID).First(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return Authorization{}, problem.New(404, "provider_commercial_authorization_not_found", "Provider commercial authorization was not found.")
	} else if err != nil {
		return Authorization{}, problem.Wrap(500, "provider_commercial_authorization_load_failed", "Provider commercial authorization could not be loaded.", err)
	}
	return s.view(ctx, model)
}

func (s *Service) view(ctx context.Context, model persistence.ProviderCommercialAuthorization) (Authorization, error) {
	var approvalModels []persistence.ProviderCommercialAuthorizationApproval
	if err := s.db.WithContext(ctx).Where("authorization_id = ?", model.ID).Order("created_at, id").Find(&approvalModels).Error; err != nil {
		return Authorization{}, problem.Wrap(500, "provider_commercial_approvals_load_failed", "Provider commercial approvals could not be loaded.", err)
	}
	approvals := make([]Approval, 0, len(approvalModels))
	for _, approval := range approvalModels {
		approvals = append(approvals, Approval{ID: approval.ID, Role: approval.ApprovalRole, Decision: approval.Decision, ApproverUserID: approval.ApproverUserID, Reason: approval.Reason, EvidenceReference: approval.EvidenceReference, EvidenceSHA256: approval.EvidenceSHA256, CreatedAt: approval.CreatedAt})
	}
	return Authorization{
		ID: model.ID, AuthorizationKey: model.AuthorizationKey, Provider: publicProvider(model.Provider),
		ProviderProduct: model.ProviderProduct, AccountType: model.AccountType, ContractingEntity: model.ContractingEntity,
		CredentialMode: model.CredentialMode, AllowedCredentialScopes: model.AllowedCredentialScopes, AllowedRegions: model.AllowedRegions,
		DataUsePolicy: model.DataUsePolicy, RetentionPolicy: model.RetentionPolicy, TermsEffectiveAt: model.TermsEffectiveAt,
		TermsReference: model.TermsReference, TermsSHA256: model.TermsSHA256,
		AgreementReference: model.AgreementReference, AgreementSHA256: model.AgreementSHA256,
		DPAReference: model.DPAReference, DPASHA256: model.DPASHA256,
		ProhibitedUseSummary: model.ProhibitedUseSummary, TerminationRunbookReference: model.TerminationRunbookReference,
		TerminationRunbookSHA256: model.TerminationRunbookSHA256,
		ReviewExpiresAt:          model.ReviewExpiresAt, State: model.State, Version: model.Version, CreatedBy: model.CreatedBy,
		Approvals: approvals, ActivatedAt: model.ActivatedAt, RejectedAt: model.RejectedAt, RevokedAt: model.RevokedAt,
		CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt,
	}, nil
}

func (s *Service) recordAudit(ctx context.Context, tx *gorm.DB, actorID uuid.UUID, action string, resourceID uuid.UUID, requestID, ipAddress string, metadata map[string]any) error {
	return audit.Record(ctx, tx, audit.Entry{TenantID: s.operatorTenantID, ActorType: "user", ActorID: &actorID, Action: action, ResourceType: "provider_commercial_authorization", ResourceID: &resourceID, RequestID: requestID, IPAddress: ipAddress, Metadata: metadata})
}

func storageProvider(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "codex":
		return "codex", true
	case "claudeagent", "claude_agent":
		return "claude_agent", true
	default:
		return "", false
	}
}

func publicProvider(value string) string {
	if value == "claude_agent" {
		return "claudeAgent"
	}
	return value
}

func normalizeSet(values []string) []string {
	set := map[string]struct{}{}
	for _, value := range values {
		normalized := strings.ToLower(strings.TrimSpace(value))
		if normalized != "" {
			set[normalized] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	slices.Sort(result)
	return result
}

func validScopes(values []string) bool {
	return len(values) >= 1 && len(values) <= 4 && !slices.ContainsFunc(values, func(value string) bool {
		return !slices.Contains([]string{"user", "organization", "tenant", "platform"}, value)
	})
}

func validRegions(values []string) bool {
	return len(values) >= 1 && len(values) <= 32 && !slices.ContainsFunc(values, func(value string) bool { return !regionPattern.MatchString(value) })
}

func validHTTPSReference(raw string) bool {
	if len(raw) < 8 || len(raw) > 2048 || strings.ContainsAny(raw, "\r\n\x00") {
		return false
	}
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == ""
}

func validDocumentSHA256(value string) bool {
	return documentSHA256Pattern.MatchString(value) && value != "sha256:"+strings.Repeat("0", 64)
}
