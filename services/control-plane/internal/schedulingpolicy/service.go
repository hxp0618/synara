package schedulingpolicy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/providercatalog"
)

const (
	ModeAny   = "any"
	ModeAllow = "allow"

	DimensionTarget        = "target"
	DimensionRegion        = "region"
	DimensionCluster       = "cluster"
	DimensionProvider      = "provider"
	DimensionCapacityClass = "capacity_class"

	UnrestrictedDigest = "48646d468c45b8a2257c2080fce3ef0697f7ec181ca7e085eb49a194a92a90b2"
)

var (
	ErrVersionConflict          = errors.New("execution scheduling policy version conflict")
	ErrPolicyCorrupt            = errors.New("execution scheduling policy is corrupt")
	ErrPolicyStale              = errors.New("execution scheduling policy is stale")
	ErrOrganizationWidensTenant = errors.New("organization execution scheduling policy widens tenant authority")
	dimensions                  = []string{DimensionTarget, DimensionRegion, DimensionCluster, DimensionProvider, DimensionCapacityClass}
)

type Rule struct {
	Mode   string   `json:"mode"`
	Values []string `json:"values"`
}

type Document struct {
	DenyAll       bool `json:"denyAll"`
	Target        Rule `json:"target"`
	Region        Rule `json:"region"`
	Cluster       Rule `json:"cluster"`
	Provider      Rule `json:"provider"`
	CapacityClass Rule `json:"capacityClass"`
}

type ScopeSnapshot struct {
	ScopeKind  string     `json:"scopeKind"`
	ScopeID    uuid.UUID  `json:"scopeId"`
	Version    int64      `json:"version"`
	Digest     string     `json:"digest"`
	Document   Document   `json:"document"`
	HeadID     *uuid.UUID `json:"-"`
	RevisionID *uuid.UUID `json:"-"`
}

type Snapshot struct {
	Tenant       ScopeSnapshot  `json:"tenant"`
	Organization *ScopeSnapshot `json:"organization,omitempty"`
	Effective    Document       `json:"effective"`
}

type Target struct {
	ID       uuid.UUID
	Region   string
	Cluster  string
	Provider string
}

type UpdateInput struct {
	ExpectedVersion int64
	Document        Document
	ActorID         uuid.UUID
	RequestID       string
	IPAddress       string
}

type Service struct {
	db  *gorm.DB
	now func() time.Time
}

func NewService(db *gorm.DB) *Service {
	return &Service{db: db, now: func() time.Time { return time.Now().UTC() }}
}

func UnrestrictedDocument() Document {
	return Document{Target: anyRule(), Region: anyRule(), Cluster: anyRule(), Provider: anyRule(), CapacityClass: anyRule()}
}

func anyRule() Rule { return Rule{Mode: ModeAny, Values: []string{}} }

func (s *Service) Resolve(ctx context.Context, tx *gorm.DB, tenantID uuid.UUID, organizationID *uuid.UUID) (Snapshot, error) {
	if tx == nil {
		tx = s.db
	}
	tenant, err := s.loadScope(ctx, tx, tenantID, "tenant", tenantID, false)
	if err != nil {
		return Snapshot{}, err
	}
	result := Snapshot{Tenant: tenant, Effective: cloneDocument(tenant.Document)}
	if organizationID != nil {
		if err := validateOrganization(ctx, tx, tenantID, *organizationID, false); err != nil {
			return Snapshot{}, err
		}
		organization, err := s.loadScope(ctx, tx, tenantID, "organization", *organizationID, false)
		if err != nil {
			return Snapshot{}, err
		}
		result.Organization = &organization
		result.Effective = intersect(tenant.Document, organization.Document)
	}
	return result, nil
}

func (s *Service) GetTenant(ctx context.Context, tenantID uuid.UUID) (Snapshot, error) {
	return s.Resolve(ctx, s.db, tenantID, nil)
}

func (s *Service) GetOrganization(ctx context.Context, tenantID, organizationID uuid.UUID) (Snapshot, error) {
	return s.Resolve(ctx, s.db, tenantID, &organizationID)
}

func (s *Service) UpdateTenant(ctx context.Context, tenantID uuid.UUID, input UpdateInput) (Snapshot, error) {
	if err := s.update(ctx, tenantID, "tenant", tenantID, input); err != nil {
		return Snapshot{}, err
	}
	return s.GetTenant(ctx, tenantID)
}

func (s *Service) UpdateOrganization(ctx context.Context, tenantID, organizationID uuid.UUID, input UpdateInput) (Snapshot, error) {
	if err := s.update(ctx, tenantID, "organization", organizationID, input); err != nil {
		return Snapshot{}, err
	}
	return s.GetOrganization(ctx, tenantID, organizationID)
}

func (s *Service) LockEffectiveForCommit(ctx context.Context, tx *gorm.DB, tenantID uuid.UUID, organizationID *uuid.UUID, expected Snapshot) error {
	if tx == nil {
		return errors.New("LockEffectiveForCommit requires a transaction")
	}
	if !validExpectedIdentity(tenantID, organizationID, expected) {
		return ErrPolicyStale
	}
	var tenant persistence.Tenant
	if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("id = ?", tenantID).Take(&tenant).Error; err != nil {
		return fmt.Errorf("lock tenant scheduling authority: %w", err)
	}
	if organizationID != nil {
		if err := validateOrganization(ctx, tx, tenantID, *organizationID, true); err != nil {
			return err
		}
	}
	actualTenant, err := s.loadScope(ctx, tx, tenantID, "tenant", tenantID, true)
	if err != nil {
		return err
	}
	actual := Snapshot{Tenant: actualTenant, Effective: actualTenant.Document}
	if organizationID != nil {
		org, err := s.loadScope(ctx, tx, tenantID, "organization", *organizationID, true)
		if err != nil {
			return err
		}
		actual.Organization = &org
		actual.Effective = intersect(actualTenant.Document, org.Document)
	}
	if !sameSnapshot(expected, actual) {
		return ErrPolicyStale
	}
	return nil
}

func (s *Service) update(ctx context.Context, tenantID uuid.UUID, scopeKind string, scopeID uuid.UUID, input UpdateInput) error {
	if input.ExpectedVersion < 0 || input.ActorID == uuid.Nil {
		return errors.New("invalid scheduling policy update identity or version")
	}
	normalized, digest, err := Normalize(input.Document)
	if err != nil {
		return err
	}
	return persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var tenant persistence.Tenant
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("id = ?", tenantID).Take(&tenant).Error; err != nil {
			return err
		}
		if scopeKind == "organization" {
			if err := validateOrganization(ctx, tx, tenantID, scopeID, true); err != nil {
				return err
			}
			parent, err := s.loadScope(ctx, tx, tenantID, "tenant", tenantID, true)
			if err != nil {
				return err
			}
			if !narrows(parent.Document, normalized) {
				return ErrOrganizationWidensTenant
			}
		}
		now := s.now()
		var head persistence.ExecutionSchedulingPolicyHead
		err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("tenant_id = ? AND scope_kind = ? AND scope_id = ?", tenantID, scopeKind, scopeID).Take(&head).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if input.ExpectedVersion != 0 {
				return ErrVersionConflict
			}
			head = persistence.ExecutionSchedulingPolicyHead{ID: uuid.New(), TenantID: tenantID, ScopeKind: scopeKind, ScopeID: scopeID, Version: 0, UpdatedBy: input.ActorID, CreatedAt: now, UpdatedAt: now}
			if scopeKind == "organization" {
				head.OrganizationID = &scopeID
			}
			if err := tx.WithContext(ctx).Create(&head).Error; err != nil {
				return fmt.Errorf("create scheduling policy head: %w", err)
			}
		} else if err != nil {
			return err
		} else if head.Version != input.ExpectedVersion {
			return ErrVersionConflict
		}

		revision := persistence.ExecutionSchedulingPolicyRevision{ID: uuid.New(), TenantID: tenantID, PolicyHeadID: head.ID, RevisionNumber: head.Version + 1, SHA256: digest, DenyAll: normalized.DenyAll, CreatedBy: input.ActorID, CreatedAt: now}
		if err := tx.WithContext(ctx).Create(&revision).Error; err != nil {
			return fmt.Errorf("create scheduling policy revision: %w", err)
		}
		for _, dimension := range dimensions {
			rule := ruleFor(normalized, dimension)
			if err := tx.WithContext(ctx).Create(&persistence.ExecutionSchedulingPolicyRule{TenantID: tenantID, RevisionID: revision.ID, Dimension: dimension, Mode: rule.Mode}).Error; err != nil {
				return err
			}
			for _, value := range rule.Values {
				if err := tx.WithContext(ctx).Create(&persistence.ExecutionSchedulingPolicyRuleValue{TenantID: tenantID, RevisionID: revision.ID, Dimension: dimension, Value: value}).Error; err != nil {
					return err
				}
			}
		}
		result := tx.WithContext(ctx).Model(&persistence.ExecutionSchedulingPolicyHead{}).
			Where("tenant_id = ? AND id = ? AND version = ?", tenantID, head.ID, input.ExpectedVersion).
			Updates(map[string]any{"current_revision_id": revision.ID, "version": revision.RevisionNumber, "updated_by": input.ActorID, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrVersionConflict
		}
		action := "execution_scheduling_policy.tenant_updated"
		var organizationID *uuid.UUID
		if scopeKind == "organization" {
			action = "execution_scheduling_policy.organization_updated"
			organizationID = &scopeID
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID:       tenantID,
			ActorType:      "user",
			ActorID:        &input.ActorID,
			Action:         action,
			ResourceType:   "execution_scheduling_policy",
			ResourceID:     &scopeID,
			OrganizationID: organizationID,
			RequestID:      input.RequestID,
			IPAddress:      input.IPAddress,
			Metadata: map[string]any{
				"scopeKind": scopeKind,
				"version":   revision.RevisionNumber,
				"digest":    revision.SHA256,
				"denyAll":   revision.DenyAll,
			},
		})
	})
}

func (s *Service) loadScope(ctx context.Context, db *gorm.DB, tenantID uuid.UUID, kind string, scopeID uuid.UUID, lock bool) (ScopeSnapshot, error) {
	query := db.WithContext(ctx)
	if lock {
		query = persistence.WithLocking(query, "UPDATE", "")
	}
	var head persistence.ExecutionSchedulingPolicyHead
	err := query.Where("tenant_id = ? AND scope_kind = ? AND scope_id = ?", tenantID, kind, scopeID).Take(&head).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ScopeSnapshot{ScopeKind: kind, ScopeID: scopeID, Version: 0, Digest: UnrestrictedDigest, Document: UnrestrictedDocument()}, nil
	}
	if err != nil {
		return ScopeSnapshot{}, err
	}
	if head.Version <= 0 || head.CurrentRevisionID == nil {
		return ScopeSnapshot{}, ErrPolicyCorrupt
	}
	query = db.WithContext(ctx)
	if lock {
		query = persistence.WithLocking(query, "SHARE", "")
	}
	var revision persistence.ExecutionSchedulingPolicyRevision
	if err := query.Where("tenant_id = ? AND id = ? AND policy_head_id = ? AND revision_number = ?", tenantID, *head.CurrentRevisionID, head.ID, head.Version).Take(&revision).Error; err != nil {
		return ScopeSnapshot{}, fmt.Errorf("%w: revision: %v", ErrPolicyCorrupt, err)
	}
	document, err := loadDocument(ctx, db, tenantID, revision)
	if err != nil {
		return ScopeSnapshot{}, err
	}
	_, digest, err := Normalize(document)
	if err != nil || digest != revision.SHA256 {
		return ScopeSnapshot{}, fmt.Errorf("%w: digest mismatch", ErrPolicyCorrupt)
	}
	return ScopeSnapshot{ScopeKind: kind, ScopeID: scopeID, Version: head.Version, Digest: digest, Document: document, HeadID: &head.ID, RevisionID: &revision.ID}, nil
}

func loadDocument(ctx context.Context, db *gorm.DB, tenantID uuid.UUID, revision persistence.ExecutionSchedulingPolicyRevision) (Document, error) {
	var rules []persistence.ExecutionSchedulingPolicyRule
	if err := db.WithContext(ctx).Where("tenant_id = ? AND revision_id = ?", tenantID, revision.ID).Find(&rules).Error; err != nil {
		return Document{}, err
	}
	if len(rules) != len(dimensions) {
		return Document{}, fmt.Errorf("%w: expected five rules", ErrPolicyCorrupt)
	}
	var values []persistence.ExecutionSchedulingPolicyRuleValue
	if err := db.WithContext(ctx).Where("tenant_id = ? AND revision_id = ?", tenantID, revision.ID).Find(&values).Error; err != nil {
		return Document{}, err
	}
	byDimension := map[string][]string{}
	for _, value := range values {
		if !knownDimension(value.Dimension) {
			return Document{}, fmt.Errorf("%w: unknown value dimension", ErrPolicyCorrupt)
		}
		byDimension[value.Dimension] = append(byDimension[value.Dimension], value.Value)
	}
	document := UnrestrictedDocument()
	document.DenyAll = revision.DenyAll
	seen := map[string]bool{}
	for _, row := range rules {
		if !knownDimension(row.Dimension) {
			return Document{}, fmt.Errorf("%w: unknown rule dimension", ErrPolicyCorrupt)
		}
		if seen[row.Dimension] {
			return Document{}, fmt.Errorf("%w: duplicate rule", ErrPolicyCorrupt)
		}
		seen[row.Dimension] = true
		setRule(&document, row.Dimension, Rule{Mode: row.Mode, Values: byDimension[row.Dimension]})
	}
	return document, nil
}

func Normalize(document Document) (Document, string, error) {
	normalized := Document{DenyAll: document.DenyAll}
	for _, dimension := range dimensions {
		rule, err := normalizeRule(dimension, ruleFor(document, dimension))
		if err != nil {
			return Document{}, "", err
		}
		setRule(&normalized, dimension, rule)
	}
	payload := struct {
		DenyAll bool            `json:"denyAll"`
		Rules   []canonicalRule `json:"rules"`
	}{DenyAll: normalized.DenyAll}
	for _, dimension := range dimensions {
		rule := ruleFor(normalized, dimension)
		payload.Rules = append(payload.Rules, canonicalRule{dimension, rule.Mode, rule.Values})
	}
	encoded, _ := json.Marshal(payload)
	digest := sha256.Sum256(encoded)
	return normalized, hex.EncodeToString(digest[:]), nil
}

type canonicalRule struct {
	Dimension string   `json:"dimension"`
	Mode      string   `json:"mode"`
	Values    []string `json:"values"`
}

func normalizeRule(dimension string, rule Rule) (Rule, error) {
	if rule.Mode != ModeAny && rule.Mode != ModeAllow {
		return Rule{}, fmt.Errorf("invalid %s policy mode", dimension)
	}
	if rule.Mode == ModeAny && len(rule.Values) != 0 {
		return Rule{}, fmt.Errorf("any %s policy cannot contain values", dimension)
	}
	result := Rule{Mode: rule.Mode, Values: make([]string, 0, len(rule.Values))}
	seen := map[string]bool{}
	for _, raw := range rule.Values {
		value := strings.TrimSpace(raw)
		switch dimension {
		case DimensionTarget:
			parsed, err := uuid.Parse(value)
			if err != nil || raw != parsed.String() {
				return Rule{}, errors.New("target policy values must be canonical UUIDs")
			}
			value = parsed.String()
		case DimensionProvider:
			canonical, ok := providercatalog.CanonicalName(value)
			if !ok {
				return Rule{}, fmt.Errorf("unknown provider %q", raw)
			}
			value = canonical
		case DimensionCapacityClass:
			value = strings.ToLower(value)
			if value != "standard" && value != "interactive" {
				return Rule{}, fmt.Errorf("unknown capacity class %q", raw)
			}
		case DimensionRegion:
			if len(value) < 1 || len(value) > 120 || value != raw {
				return Rule{}, errors.New("region values must be trimmed and 1-120 bytes")
			}
		case DimensionCluster:
			if len(value) < 1 || len(value) > 200 || value != raw {
				return Rule{}, errors.New("cluster values must be trimmed and 1-200 bytes")
			}
		}
		if seen[value] {
			continue
		}
		seen[value] = true
		result.Values = append(result.Values, value)
	}
	sort.Strings(result.Values)
	return result, nil
}

func AllowsTarget(document Document, target Target) bool {
	if document.DenyAll {
		return false
	}
	provider := target.Provider
	if document.Provider.Mode == ModeAllow {
		var valid bool
		provider, valid = providercatalog.CanonicalName(target.Provider)
		if !valid {
			return false
		}
	}
	return allows(document.Target, target.ID.String()) && allows(document.Region, target.Region) && allows(document.Cluster, target.Cluster) && allows(document.Provider, provider)
}

func AllowsPlacement(document Document, capacityClass string) bool {
	if document.DenyAll {
		return false
	}
	if document.CapacityClass.Mode == ModeAny {
		return true
	}
	canonical := strings.ToLower(strings.TrimSpace(capacityClass))
	if canonical != "standard" && canonical != "interactive" {
		return false
	}
	return allows(document.CapacityClass, canonical)
}

func allows(rule Rule, value string) bool {
	if rule.Mode == ModeAny {
		return true
	}
	i := sort.SearchStrings(rule.Values, value)
	return i < len(rule.Values) && rule.Values[i] == value
}

func validateOrganization(ctx context.Context, db *gorm.DB, tenantID, organizationID uuid.UUID, lock bool) error {
	query := db.WithContext(ctx)
	if lock {
		query = persistence.WithLocking(query, "UPDATE", "")
	}
	var org persistence.Organization
	if err := query.Where("tenant_id = ? AND id = ?", tenantID, organizationID).Take(&org).Error; err != nil {
		return fmt.Errorf("organization scheduling scope is unavailable: %w", err)
	}
	return nil
}

func narrows(parent, child Document) bool {
	if parent.DenyAll {
		return true
	}
	for _, dimension := range dimensions {
		p, c := ruleFor(parent, dimension), ruleFor(child, dimension)
		if p.Mode == ModeAllow && c.Mode == ModeAllow {
			for _, value := range c.Values {
				if !allows(p, value) {
					return false
				}
			}
		}
	}
	return true
}

func intersect(parent, child Document) Document {
	result := Document{DenyAll: parent.DenyAll || child.DenyAll}
	for _, dimension := range dimensions {
		p, c := ruleFor(parent, dimension), ruleFor(child, dimension)
		var rule Rule
		switch {
		case p.Mode == ModeAny:
			rule = c
		case c.Mode == ModeAny:
			rule = p
		default:
			rule = Rule{Mode: ModeAllow, Values: []string{}}
			for _, value := range p.Values {
				if allows(c, value) {
					rule.Values = append(rule.Values, value)
				}
			}
		}
		setRule(&result, dimension, Rule{Mode: rule.Mode, Values: append([]string(nil), rule.Values...)})
	}
	return result
}

func sameSnapshot(a, b Snapshot) bool {
	if !sameScope(a.Tenant, b.Tenant) || (a.Organization == nil) != (b.Organization == nil) {
		return false
	}
	if a.Organization != nil && !sameScope(*a.Organization, *b.Organization) {
		return false
	}
	_, ad, ae := Normalize(a.Effective)
	_, bd, be := Normalize(b.Effective)
	return ae == nil && be == nil && ad == bd
}

func sameScope(a, b ScopeSnapshot) bool {
	if a.ScopeKind != b.ScopeKind || a.ScopeID != b.ScopeID || a.Version != b.Version || a.Digest != b.Digest {
		return false
	}
	if (a.HeadID == nil) != (b.HeadID == nil) || (a.RevisionID == nil) != (b.RevisionID == nil) {
		return false
	}
	if a.HeadID != nil && *a.HeadID != *b.HeadID {
		return false
	}
	return a.RevisionID == nil || *a.RevisionID == *b.RevisionID
}

func validExpectedIdentity(tenantID uuid.UUID, organizationID *uuid.UUID, expected Snapshot) bool {
	if expected.Tenant.ScopeKind != "tenant" || expected.Tenant.ScopeID != tenantID || !validScopeShape(expected.Tenant) {
		return false
	}
	if organizationID == nil {
		return expected.Organization == nil
	}
	return expected.Organization != nil && expected.Organization.ScopeKind == "organization" &&
		expected.Organization.ScopeID == *organizationID && validScopeShape(*expected.Organization)
}

func validScopeShape(scope ScopeSnapshot) bool {
	if scope.Version == 0 {
		return scope.Digest == UnrestrictedDigest && scope.HeadID == nil && scope.RevisionID == nil
	}
	return scope.Version > 0 && scope.HeadID != nil && scope.RevisionID != nil
}

func ruleFor(document Document, dimension string) Rule {
	switch dimension {
	case DimensionTarget:
		return document.Target
	case DimensionRegion:
		return document.Region
	case DimensionCluster:
		return document.Cluster
	case DimensionProvider:
		return document.Provider
	default:
		return document.CapacityClass
	}
}
func setRule(document *Document, dimension string, rule Rule) {
	switch dimension {
	case DimensionTarget:
		document.Target = rule
	case DimensionRegion:
		document.Region = rule
	case DimensionCluster:
		document.Cluster = rule
	case DimensionProvider:
		document.Provider = rule
	case DimensionCapacityClass:
		document.CapacityClass = rule
	}
}
func cloneDocument(document Document) Document {
	result := document
	for _, d := range dimensions {
		r := ruleFor(document, d)
		r.Values = append([]string(nil), r.Values...)
		setRule(&result, d, r)
	}
	return result
}
func knownDimension(value string) bool {
	for _, dimension := range dimensions {
		if value == dimension {
			return true
		}
	}
	return false
}
