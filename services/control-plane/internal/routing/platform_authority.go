package routing

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	PlatformAuthoritySchemaVersionV1 = 1

	PlatformAuthorityOwnershipShared      = "platform-shared"
	PlatformAuthorityOwnershipTenantOwned = "tenant-owned"

	platformAuthoritySignatureDomain = "synara.platform-routing-authority.v1\n"
	platformAuthorityMaximumValidity = 5 * time.Minute
	platformAuthorityClockSkew       = 30 * time.Second
	platformAuthorityMaximumItems    = 32
)

type PlatformAuthorityPublisherKey struct {
	KeyID     string
	PublicKey ed25519.PublicKey
}

type PlatformAuthorityDRRouteScope struct {
	SourceDRDomain string
	DRDomain       string
}

type PlatformAuthorityTargetScope struct {
	ExecutionTargetID   uuid.UUID
	Ownership           string
	TenantID            *uuid.UUID
	PublishHealth       bool
	PublishReservations bool
	DRRoutes            []PlatformAuthorityDRRouteScope
}

type PlatformAuthorityPublisherConfig struct {
	PublisherIdentity string
	Keys              []PlatformAuthorityPublisherKey
	Targets           []PlatformAuthorityTargetScope
}

type PlatformAuthorityHealth struct {
	Status                 string                           `json:"status"`
	CapacityStatus         string                           `json:"capacityStatus"`
	AvailableCapacityUnits *int                             `json:"availableCapacityUnits,omitempty"`
	AllocatedCapacityUnits int                              `json:"allocatedCapacityUnits"`
	ReservationAuthority   *ReservationAuthorityObservation `json:"reservationAuthority,omitempty"`
	Reason                 *string                          `json:"reason,omitempty"`
	TTLSeconds             int                              `json:"ttlSeconds"`
}

type PlatformAuthorityDRReadiness struct {
	SourceDRDomain      string  `json:"sourceDrDomain"`
	DRDomain            string  `json:"drDomain"`
	ReplicatedThroughAt string  `json:"replicatedThroughAt"`
	ArtifactsReady      bool    `json:"artifactsReady"`
	CheckpointsReady    bool    `json:"checkpointsReady"`
	MemoryReady         bool    `json:"memoryReady"`
	Reason              *string `json:"reason,omitempty"`
	TTLSeconds          int     `json:"ttlSeconds"`
}

// PlatformAuthorityPublication is a self-contained Ed25519-signed bundle.
// All timestamps must be canonical UTC RFC3339Nano strings. Signature covers
// every field except Signature using the fixed v1 field order below.
type PlatformAuthorityPublication struct {
	SchemaVersion     int                            `json:"schemaVersion"`
	PublisherIdentity string                         `json:"publisherIdentity"`
	KeyID             string                         `json:"keyId"`
	Nonce             string                         `json:"nonce"`
	IssuedAt          string                         `json:"issuedAt"`
	ExpiresAt         string                         `json:"expiresAt"`
	ExecutionTargetID string                         `json:"executionTargetId"`
	ObservedAt        string                         `json:"observedAt"`
	Health            *PlatformAuthorityHealth       `json:"health,omitempty"`
	DRReadiness       []PlatformAuthorityDRReadiness `json:"drReadiness,omitempty"`
	Signature         string                         `json:"signature"`
}

type platformAuthoritySignedPayload struct {
	SchemaVersion     int                            `json:"schemaVersion"`
	PublisherIdentity string                         `json:"publisherIdentity"`
	KeyID             string                         `json:"keyId"`
	Nonce             string                         `json:"nonce"`
	IssuedAt          string                         `json:"issuedAt"`
	ExpiresAt         string                         `json:"expiresAt"`
	ExecutionTargetID string                         `json:"executionTargetId"`
	ObservedAt        string                         `json:"observedAt"`
	Health            *PlatformAuthorityHealth       `json:"health,omitempty"`
	DRReadiness       []PlatformAuthorityDRReadiness `json:"drReadiness,omitempty"`
}

type PlatformAuthorityPublicationResult struct {
	ExecutionTargetID uuid.UUID               `json:"executionTargetId"`
	TenantID          *uuid.UUID              `json:"tenantId,omitempty"`
	PublisherIdentity string                  `json:"publisherIdentity"`
	Nonce             uuid.UUID               `json:"nonce"`
	ObservedAt        time.Time               `json:"observedAt"`
	Health            *TargetHealthView       `json:"health,omitempty"`
	DRReadiness       []TargetDRReadinessView `json:"drReadiness,omitempty"`
	Replayed          bool                    `json:"replayed"`
}

type platformAuthorityKey struct {
	publicKey       ed25519.PublicKey
	publicKeySHA256 string
}

type platformAuthorityTargetScope struct {
	tenantID            *uuid.UUID
	publishHealth       bool
	publishReservations bool
	drRoutes            map[string]string
}

type platformAuthorityPublisher struct {
	keys    map[string]platformAuthorityKey
	targets map[uuid.UUID]platformAuthorityTargetScope
}

type PlatformAuthorityService struct {
	db            *gorm.DB
	publishers    map[string]platformAuthorityPublisher
	healthTargets map[uuid.UUID]struct{}
	drSources     map[uuid.UUID]map[string]struct{}
	now           func() time.Time
}

func NewPlatformAuthorityService(
	db *gorm.DB,
	configs []PlatformAuthorityPublisherConfig,
) (*PlatformAuthorityService, error) {
	if db == nil {
		return nil, errors.New("Platform routing authority database is required")
	}
	normalized, err := NormalizePlatformAuthorityPublisherConfigs(configs)
	if err != nil {
		return nil, err
	}
	service := &PlatformAuthorityService{
		db:            db,
		publishers:    make(map[string]platformAuthorityPublisher, len(normalized)),
		healthTargets: make(map[uuid.UUID]struct{}),
		drSources:     make(map[uuid.UUID]map[string]struct{}),
		now:           func() time.Time { return time.Now().UTC() },
	}
	for _, publisherConfig := range normalized {
		publisher := platformAuthorityPublisher{
			keys:    make(map[string]platformAuthorityKey, len(publisherConfig.Keys)),
			targets: make(map[uuid.UUID]platformAuthorityTargetScope, len(publisherConfig.Targets)),
		}
		for _, key := range publisherConfig.Keys {
			digest := sha256.Sum256(key.PublicKey)
			publisher.keys[key.KeyID] = platformAuthorityKey{
				publicKey:       append(ed25519.PublicKey(nil), key.PublicKey...),
				publicKeySHA256: hex.EncodeToString(digest[:]),
			}
		}
		for _, target := range publisherConfig.Targets {
			routes := make(map[string]string, len(target.DRRoutes))
			for _, route := range target.DRRoutes {
				routes[route.SourceDRDomain] = route.DRDomain
			}
			publisher.targets[target.ExecutionTargetID] = platformAuthorityTargetScope{
				tenantID: copyUUIDPointer(target.TenantID), publishHealth: target.PublishHealth,
				publishReservations: target.PublishReservations, drRoutes: routes,
			}
			if target.PublishHealth {
				service.healthTargets[target.ExecutionTargetID] = struct{}{}
			}
			if len(routes) > 0 {
				if service.drSources[target.ExecutionTargetID] == nil {
					service.drSources[target.ExecutionTargetID] = make(map[string]struct{}, len(routes))
				}
				for sourceDomain := range routes {
					service.drSources[target.ExecutionTargetID][sourceDomain] = struct{}{}
				}
			}
		}
		service.publishers[publisherConfig.PublisherIdentity] = publisher
	}
	return service, nil
}

func (s *PlatformAuthorityService) Configured() bool {
	return s != nil && len(s.publishers) > 0
}

func (s *PlatformAuthorityService) HasConfiguredHealthAuthority(targetID uuid.UUID) bool {
	if s == nil {
		return false
	}
	_, found := s.healthTargets[targetID]
	return found
}

func (s *PlatformAuthorityService) HasConfiguredDRAuthority(targetID uuid.UUID, sourceDRDomain string) bool {
	if s == nil {
		return false
	}
	sources := s.drSources[targetID]
	_, found := sources[strings.TrimSpace(sourceDRDomain)]
	return found
}

func NormalizePlatformAuthorityPublisherConfigs(
	configs []PlatformAuthorityPublisherConfig,
) ([]PlatformAuthorityPublisherConfig, error) {
	if len(configs) > 64 {
		return nil, errors.New("Platform routing authority supports at most 64 publishers")
	}
	result := make([]PlatformAuthorityPublisherConfig, 0, len(configs))
	identities := make(map[string]struct{}, len(configs))
	healthAuthorities := make(map[uuid.UUID]string)
	drAuthorities := make(map[string]string)
	for publisherIndex, rawPublisher := range configs {
		identity, err := normalizePlatformAuthorityIdentifier(rawPublisher.PublisherIdentity, 160)
		if err != nil {
			return nil, fmt.Errorf("publishers[%d].publisherIdentity: %w", publisherIndex, err)
		}
		if _, duplicate := identities[identity]; duplicate {
			return nil, fmt.Errorf("publishers[%d].publisherIdentity %q is duplicated", publisherIndex, identity)
		}
		identities[identity] = struct{}{}
		if len(rawPublisher.Keys) == 0 || len(rawPublisher.Keys) > 8 {
			return nil, fmt.Errorf("publishers[%d].keys must contain between 1 and 8 keys", publisherIndex)
		}
		if len(rawPublisher.Targets) == 0 || len(rawPublisher.Targets) > 128 {
			return nil, fmt.Errorf("publishers[%d].targets must contain between 1 and 128 targets", publisherIndex)
		}
		publisher := PlatformAuthorityPublisherConfig{PublisherIdentity: identity}
		keyIDs := make(map[string]struct{}, len(rawPublisher.Keys))
		for keyIndex, rawKey := range rawPublisher.Keys {
			keyID, keyErr := normalizePlatformAuthorityIdentifier(rawKey.KeyID, 160)
			if keyErr != nil {
				return nil, fmt.Errorf("publishers[%d].keys[%d].keyId: %w", publisherIndex, keyIndex, keyErr)
			}
			if _, duplicate := keyIDs[keyID]; duplicate {
				return nil, fmt.Errorf("publishers[%d].keys[%d].keyId %q is duplicated", publisherIndex, keyIndex, keyID)
			}
			if len(rawKey.PublicKey) != ed25519.PublicKeySize {
				return nil, fmt.Errorf("publishers[%d].keys[%d].ed25519PublicKey is invalid", publisherIndex, keyIndex)
			}
			keyIDs[keyID] = struct{}{}
			publisher.Keys = append(publisher.Keys, PlatformAuthorityPublisherKey{
				KeyID: keyID, PublicKey: append(ed25519.PublicKey(nil), rawKey.PublicKey...),
			})
		}
		targetIDs := make(map[uuid.UUID]struct{}, len(rawPublisher.Targets))
		for targetIndex, rawTarget := range rawPublisher.Targets {
			if rawTarget.ExecutionTargetID == uuid.Nil {
				return nil, fmt.Errorf("publishers[%d].targets[%d].executionTargetId is required", publisherIndex, targetIndex)
			}
			if _, duplicate := targetIDs[rawTarget.ExecutionTargetID]; duplicate {
				return nil, fmt.Errorf("publishers[%d].targets[%d].executionTargetId is duplicated", publisherIndex, targetIndex)
			}
			targetIDs[rawTarget.ExecutionTargetID] = struct{}{}
			target := PlatformAuthorityTargetScope{
				ExecutionTargetID:   rawTarget.ExecutionTargetID,
				Ownership:           strings.TrimSpace(rawTarget.Ownership),
				PublishHealth:       rawTarget.PublishHealth,
				PublishReservations: rawTarget.PublishReservations,
			}
			if target.PublishReservations && !target.PublishHealth {
				return nil, fmt.Errorf("publishers[%d].targets[%d].publishReservations requires publishHealth", publisherIndex, targetIndex)
			}
			switch target.Ownership {
			case PlatformAuthorityOwnershipShared:
				if rawTarget.TenantID != nil {
					return nil, fmt.Errorf("publishers[%d].targets[%d].tenantId must be absent for platform-shared ownership", publisherIndex, targetIndex)
				}
			case PlatformAuthorityOwnershipTenantOwned:
				if rawTarget.TenantID == nil || *rawTarget.TenantID == uuid.Nil {
					return nil, fmt.Errorf("publishers[%d].targets[%d].tenantId is required for tenant-owned ownership", publisherIndex, targetIndex)
				}
				target.TenantID = copyUUIDPointer(rawTarget.TenantID)
			default:
				return nil, fmt.Errorf("publishers[%d].targets[%d].ownership is invalid", publisherIndex, targetIndex)
			}
			if len(rawTarget.DRRoutes) > platformAuthorityMaximumItems {
				return nil, fmt.Errorf("publishers[%d].targets[%d].drRoutes supports at most %d routes", publisherIndex, targetIndex, platformAuthorityMaximumItems)
			}
			sourceDomains := make(map[string]struct{}, len(rawTarget.DRRoutes))
			for routeIndex, rawRoute := range rawTarget.DRRoutes {
				sourceDomain, routeErr := normalizePlatformAuthorityDomain(rawRoute.SourceDRDomain)
				if routeErr != nil {
					return nil, fmt.Errorf("publishers[%d].targets[%d].drRoutes[%d].sourceDrDomain: %w", publisherIndex, targetIndex, routeIndex, routeErr)
				}
				destinationDomain, routeErr := normalizePlatformAuthorityDomain(rawRoute.DRDomain)
				if routeErr != nil {
					return nil, fmt.Errorf("publishers[%d].targets[%d].drRoutes[%d].drDomain: %w", publisherIndex, targetIndex, routeIndex, routeErr)
				}
				if sourceDomain == destinationDomain {
					return nil, fmt.Errorf("publishers[%d].targets[%d].drRoutes[%d] must cross DR domains", publisherIndex, targetIndex, routeIndex)
				}
				if _, duplicate := sourceDomains[sourceDomain]; duplicate {
					return nil, fmt.Errorf("publishers[%d].targets[%d].drRoutes sourceDrDomain %q is duplicated", publisherIndex, targetIndex, sourceDomain)
				}
				sourceDomains[sourceDomain] = struct{}{}
				authorityKey := rawTarget.ExecutionTargetID.String() + "\x00" + sourceDomain
				if previous, duplicate := drAuthorities[authorityKey]; duplicate {
					return nil, fmt.Errorf("DR authority for Target %s and sourceDrDomain %q overlaps publishers %q and %q", rawTarget.ExecutionTargetID, sourceDomain, previous, identity)
				}
				drAuthorities[authorityKey] = identity
				target.DRRoutes = append(target.DRRoutes, PlatformAuthorityDRRouteScope{
					SourceDRDomain: sourceDomain, DRDomain: destinationDomain,
				})
			}
			if !target.PublishHealth && len(target.DRRoutes) == 0 {
				return nil, fmt.Errorf("publishers[%d].targets[%d] must authorize health and/or a DR route", publisherIndex, targetIndex)
			}
			if target.PublishHealth {
				if previous, duplicate := healthAuthorities[target.ExecutionTargetID]; duplicate {
					return nil, fmt.Errorf("health authority for Target %s overlaps publishers %q and %q", target.ExecutionTargetID, previous, identity)
				}
				healthAuthorities[target.ExecutionTargetID] = identity
			}
			sort.Slice(target.DRRoutes, func(i, j int) bool {
				return target.DRRoutes[i].SourceDRDomain < target.DRRoutes[j].SourceDRDomain
			})
			publisher.Targets = append(publisher.Targets, target)
		}
		result = append(result, publisher)
	}
	return result, nil
}

func SignPlatformAuthorityPublication(
	privateKey ed25519.PrivateKey,
	publication PlatformAuthorityPublication,
) (PlatformAuthorityPublication, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return PlatformAuthorityPublication{}, errors.New("invalid Platform routing authority private key")
	}
	payload, err := platformAuthoritySigningPayload(publication)
	if err != nil {
		return PlatformAuthorityPublication{}, err
	}
	publication.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
	return publication, nil
}

func (s *PlatformAuthorityService) Publish(
	ctx context.Context,
	pathTargetID uuid.UUID,
	publication PlatformAuthorityPublication,
) (PlatformAuthorityPublicationResult, error) {
	if !s.Configured() {
		return PlatformAuthorityPublicationResult{}, problem.New(
			503,
			"platform_routing_authority_not_configured",
			"Platform routing authority publishers are not configured.",
		)
	}
	if publication.SchemaVersion != PlatformAuthoritySchemaVersionV1 ||
		publication.PublisherIdentity != strings.TrimSpace(publication.PublisherIdentity) ||
		publication.KeyID != strings.TrimSpace(publication.KeyID) {
		return PlatformAuthorityPublicationResult{}, platformAuthorityAuthenticationFailed()
	}
	publisher, found := s.publishers[publication.PublisherIdentity]
	if !found {
		return PlatformAuthorityPublicationResult{}, platformAuthorityAuthenticationFailed()
	}
	key, found := publisher.keys[publication.KeyID]
	if !found {
		return PlatformAuthorityPublicationResult{}, platformAuthorityAuthenticationFailed()
	}
	payload, err := platformAuthoritySigningPayload(publication)
	if err != nil {
		return PlatformAuthorityPublicationResult{}, platformAuthorityAuthenticationFailed()
	}
	signature, err := decodePlatformAuthoritySignature(publication.Signature)
	if err != nil || !ed25519.Verify(key.publicKey, payload, signature) {
		return PlatformAuthorityPublicationResult{}, platformAuthorityAuthenticationFailed()
	}
	targetID, err := parseCanonicalPlatformAuthorityUUID(publication.ExecutionTargetID, "executionTargetId")
	if err != nil || targetID != pathTargetID || pathTargetID == uuid.Nil {
		return PlatformAuthorityPublicationResult{}, problem.New(
			400,
			"invalid_platform_routing_authority_target",
			"The signed executionTargetId must exactly match the request path.",
		)
	}
	targetScope, found := publisher.targets[targetID]
	if !found {
		return PlatformAuthorityPublicationResult{}, platformAuthorityScopeDenied()
	}
	nonce, err := parseCanonicalPlatformAuthorityUUID(publication.Nonce, "nonce")
	if err != nil {
		return PlatformAuthorityPublicationResult{}, err
	}
	issuedAt, err := parseCanonicalPlatformAuthorityTime(publication.IssuedAt, "issuedAt")
	if err != nil {
		return PlatformAuthorityPublicationResult{}, err
	}
	expiresAt, err := parseCanonicalPlatformAuthorityTime(publication.ExpiresAt, "expiresAt")
	if err != nil {
		return PlatformAuthorityPublicationResult{}, err
	}
	observedAt, err := parseCanonicalPlatformAuthorityTime(publication.ObservedAt, "observedAt")
	if err != nil {
		return PlatformAuthorityPublicationResult{}, err
	}
	receivedAt := s.now().UTC()
	requestDigest := sha256.Sum256(payload)
	requestSHA256 := hex.EncodeToString(requestDigest[:])

	normalizedHealth, normalizedReadiness, err := normalizePlatformAuthorityObservations(
		publication, targetID, publication.PublisherIdentity, targetScope, observedAt, receivedAt,
	)
	if err != nil {
		return PlatformAuthorityPublicationResult{}, err
	}

	var result PlatformAuthorityPublicationResult
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var target persistence.ExecutionTarget
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Select("id", "tenant_id", "kind").Where("id = ?", targetID).Take(&target).Error; err != nil {
			return problem.Wrap(404, "execution_target_not_found", "Execution Target not found.", err)
		}
		if !samePlatformAuthorityOwnership(target.TenantID, targetScope.tenantID) {
			return platformAuthorityScopeDenied()
		}
		if normalizedHealth != nil && target.TenantID != nil && target.Kind == "kubernetes" {
			return problem.New(
				409,
				"platform_routing_health_conflicts_managed_kubernetes",
				"Tenant-owned Kubernetes Target health is owned by the managed Kubernetes Reconciler.",
			)
		}

		var receipt persistence.PlatformRoutingPublication
		receiptErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("publisher_identity = ? AND nonce = ?", publication.PublisherIdentity, nonce).
			Take(&receipt).Error
		if receiptErr == nil {
			replayed, replayErr := replayPlatformAuthorityPublication(
				receipt, targetID, publication.KeyID, key.publicKeySHA256, requestSHA256,
			)
			if replayErr != nil {
				return replayErr
			}
			result = replayed
			return nil
		}
		if !errors.Is(receiptErr, gorm.ErrRecordNotFound) {
			return problem.Wrap(500, "platform_routing_publication_receipt_load_failed", "Platform routing publication receipt could not be loaded.", receiptErr)
		}

		if issuedAt.After(receivedAt.Add(platformAuthorityClockSkew)) ||
			!expiresAt.After(issuedAt) || expiresAt.Sub(issuedAt) > platformAuthorityMaximumValidity ||
			receivedAt.After(expiresAt.Add(platformAuthorityClockSkew)) {
			return problem.New(
				401,
				"platform_routing_publication_expired",
				"The signed Platform routing publication is outside its validity window.",
			)
		}
		if normalizedHealth != nil && !normalizedHealth.ExpiresAt.After(receivedAt) {
			return problem.New(400, "platform_routing_health_already_expired", "The signed health observation is already expired.")
		}
		for _, readinessInput := range normalizedReadiness {
			if !readinessInput.ExpiresAt.After(receivedAt) {
				return problem.New(400, "platform_routing_dr_already_expired", "A signed DR readiness observation is already expired.")
			}
		}

		result = PlatformAuthorityPublicationResult{
			ExecutionTargetID: targetID,
			TenantID:          copyUUIDPointer(target.TenantID),
			PublisherIdentity: publication.PublisherIdentity,
			Nonce:             nonce,
			ObservedAt:        observedAt,
			Replayed:          false,
		}
		if normalizedHealth != nil {
			health, observeErr := observeHealthInTransaction(ctx, tx, *normalizedHealth)
			if observeErr != nil {
				return observeErr
			}
			view := HealthViewOf(health)
			result.Health = &view
		}
		for _, readinessInput := range normalizedReadiness {
			readiness, observeErr := observeDRReadinessInTransaction(ctx, tx, readinessInput)
			if observeErr != nil {
				return observeErr
			}
			result.DRReadiness = append(result.DRReadiness, DRReadinessViewOf(readiness))
		}

		response, responseSHA256, responseErr := platformAuthorityResponseReceipt(result)
		if responseErr != nil {
			return problem.Wrap(500, "platform_routing_publication_response_failed", "Platform routing publication response could not be sealed.", responseErr)
		}
		receipt = persistence.PlatformRoutingPublication{
			PublisherIdentity: publication.PublisherIdentity,
			Nonce:             nonce,
			KeyID:             publication.KeyID,
			PublicKeySHA256:   key.publicKeySHA256,
			ExecutionTargetID: targetID,
			RequestSHA256:     requestSHA256,
			ObservedAt:        observedAt,
			Response:          response,
			ResponseSHA256:    responseSHA256,
			ReceivedAt:        receivedAt,
		}
		if err := tx.WithContext(ctx).Create(&receipt).Error; err != nil {
			return problem.Wrap(409, "platform_routing_publication_receipt_conflict", "Platform routing publication receipt could not be committed; retry the exact signed request.", err)
		}
		return nil
	})
	return result, err
}

func normalizePlatformAuthorityObservations(
	publication PlatformAuthorityPublication,
	targetID uuid.UUID,
	publisherIdentity string,
	scope platformAuthorityTargetScope,
	observedAt time.Time,
	receivedAt time.Time,
) (*normalizedHealthObservation, []normalizedDRReadinessObservation, error) {
	if publication.Health == nil && len(publication.DRReadiness) == 0 {
		return nil, nil, problem.New(400, "platform_routing_publication_empty", "Publish health and/or at least one DR readiness observation.")
	}
	if len(publication.DRReadiness) > platformAuthorityMaximumItems {
		return nil, nil, problem.New(400, "platform_routing_publication_too_large", "A Platform routing publication supports at most 32 DR readiness observations.")
	}
	var health *normalizedHealthObservation
	if publication.Health != nil {
		if !scope.publishHealth {
			return nil, nil, platformAuthorityScopeDenied()
		}
		if publication.Health.ReservationAuthority != nil && !scope.publishReservations {
			return nil, nil, platformAuthorityScopeDenied()
		}
		if publication.Health.Status != strings.ToLower(strings.TrimSpace(publication.Health.Status)) ||
			publication.Health.CapacityStatus != strings.ToLower(strings.TrimSpace(publication.Health.CapacityStatus)) {
			return nil, nil, problem.New(400, "invalid_platform_routing_health_canonical_form", "Health status and capacityStatus must use canonical lowercase values without surrounding whitespace.")
		}
		if authority := publication.Health.ReservationAuthority; authority != nil {
			if authority.Mode != ReservationAuthorityExactActiveV1 {
				return nil, nil, problem.New(
					400,
					"invalid_platform_routing_reservation_authority",
					"Reservation authority mode must be exact-active-v1.",
				)
			}
			if authority.Acknowledgements == nil {
				return nil, nil, problem.New(
					400,
					"invalid_platform_routing_reservation_authority",
					"Reservation authority acknowledgements must be a JSON array.",
				)
			}
			for index, identity := range authority.Acknowledgements {
				if identity.ExecutionID == uuid.Nil || identity.Generation < 0 ||
					(index > 0 && !reservationIdentityLess(authority.Acknowledgements[index-1], identity)) {
					return nil, nil, problem.New(
						400,
						"invalid_platform_routing_reservation_order",
						"Reservation acknowledgements must be unique and sorted by executionId and generation.",
					)
				}
			}
		}
		normalized, err := normalizeHealthObservation(HealthObservation{
			ExecutionTargetID: targetID, Status: publication.Health.Status,
			CapacityStatus:         publication.Health.CapacityStatus,
			AvailableCapacityUnits: publication.Health.AvailableCapacityUnits,
			AllocatedCapacityUnits: publication.Health.AllocatedCapacityUnits,
			ReservationAuthority:   publication.Health.ReservationAuthority,
			Source:                 publisherIdentity,
			Reason:                 publication.Health.Reason,
			ObservedAt:             observedAt,
			TTL:                    time.Duration(publication.Health.TTLSeconds) * time.Second,
		}, receivedAt)
		if err != nil {
			return nil, nil, err
		}
		health = &normalized
	}

	readiness := make([]normalizedDRReadinessObservation, 0, len(publication.DRReadiness))
	previousSourceDomain := ""
	for index, item := range publication.DRReadiness {
		if item.SourceDRDomain != strings.TrimSpace(item.SourceDRDomain) ||
			item.DRDomain != strings.TrimSpace(item.DRDomain) ||
			(index > 0 && item.SourceDRDomain <= previousSourceDomain) {
			return nil, nil, problem.New(400, "invalid_platform_routing_dr_order", "DR readiness observations must be unique and sorted by sourceDrDomain.")
		}
		expectedDestination, allowed := scope.drRoutes[item.SourceDRDomain]
		if !allowed || expectedDestination != item.DRDomain {
			return nil, nil, platformAuthorityScopeDenied()
		}
		replicatedThroughAt, err := parseCanonicalPlatformAuthorityTime(
			item.ReplicatedThroughAt,
			fmt.Sprintf("drReadiness[%d].replicatedThroughAt", index),
		)
		if err != nil {
			return nil, nil, err
		}
		normalized, err := normalizeDRReadinessObservation(DRReadinessObservation{
			ExecutionTargetID: targetID, SourceDRDomain: item.SourceDRDomain, DRDomain: item.DRDomain,
			ReplicatedThroughAt: replicatedThroughAt, ArtifactsReady: item.ArtifactsReady,
			CheckpointsReady: item.CheckpointsReady, MemoryReady: item.MemoryReady,
			PublisherIdentity: publisherIdentity, Reason: item.Reason, ObservedAt: observedAt,
			TTL: time.Duration(item.TTLSeconds) * time.Second,
		}, receivedAt)
		if err != nil {
			return nil, nil, err
		}
		readiness = append(readiness, normalized)
		previousSourceDomain = item.SourceDRDomain
	}
	return health, readiness, nil
}

func reservationIdentityLess(left, right ReservationIdentity) bool {
	leftID := left.ExecutionID.String()
	rightID := right.ExecutionID.String()
	if leftID != rightID {
		return leftID < rightID
	}
	return left.Generation < right.Generation
}

func replayPlatformAuthorityPublication(
	receipt persistence.PlatformRoutingPublication,
	targetID uuid.UUID,
	keyID string,
	publicKeySHA256 string,
	requestSHA256 string,
) (PlatformAuthorityPublicationResult, error) {
	if receipt.ExecutionTargetID != targetID || receipt.KeyID != keyID ||
		receipt.PublicKeySHA256 != publicKeySHA256 || receipt.RequestSHA256 != requestSHA256 {
		return PlatformAuthorityPublicationResult{}, problem.New(
			409,
			"platform_routing_publication_nonce_conflict",
			"The publisher nonce was already used by a different signed publication.",
		)
	}
	encoded, err := json.Marshal(receipt.Response)
	if err != nil {
		return PlatformAuthorityPublicationResult{}, platformAuthorityReceiptCorrupt(err)
	}
	digest := sha256.Sum256(encoded)
	if hex.EncodeToString(digest[:]) != receipt.ResponseSHA256 {
		return PlatformAuthorityPublicationResult{}, platformAuthorityReceiptCorrupt(errors.New("response digest mismatch"))
	}
	var result PlatformAuthorityPublicationResult
	if err := json.Unmarshal(encoded, &result); err != nil {
		return PlatformAuthorityPublicationResult{}, platformAuthorityReceiptCorrupt(err)
	}
	if result.ExecutionTargetID != targetID || result.Nonce != receipt.Nonce ||
		result.PublisherIdentity != receipt.PublisherIdentity || !result.ObservedAt.Equal(receipt.ObservedAt) {
		return PlatformAuthorityPublicationResult{}, platformAuthorityReceiptCorrupt(errors.New("response scope mismatch"))
	}
	result.Replayed = true
	return result, nil
}

func platformAuthorityResponseReceipt(result PlatformAuthorityPublicationResult) (map[string]any, string, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, "", err
	}
	var response map[string]any
	if err := json.Unmarshal(encoded, &response); err != nil {
		return nil, "", err
	}
	canonical, err := json.Marshal(response)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(canonical)
	return response, hex.EncodeToString(digest[:]), nil
}

func platformAuthoritySigningPayload(publication PlatformAuthorityPublication) ([]byte, error) {
	encoded, err := json.Marshal(platformAuthoritySignedPayload{
		SchemaVersion: publication.SchemaVersion, PublisherIdentity: publication.PublisherIdentity,
		KeyID: publication.KeyID, Nonce: publication.Nonce, IssuedAt: publication.IssuedAt,
		ExpiresAt: publication.ExpiresAt, ExecutionTargetID: publication.ExecutionTargetID,
		ObservedAt: publication.ObservedAt, Health: publication.Health, DRReadiness: publication.DRReadiness,
	})
	if err != nil {
		return nil, err
	}
	return append([]byte(platformAuthoritySignatureDomain), encoded...), nil
}

func decodePlatformAuthoritySignature(encoded string) ([]byte, error) {
	if encoded == "" || encoded != strings.TrimSpace(encoded) {
		return nil, errors.New("invalid signature")
	}
	signature, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(signature) != ed25519.SignatureSize ||
		base64.StdEncoding.EncodeToString(signature) != encoded {
		return nil, errors.New("invalid signature")
	}
	return signature, nil
}

func parseCanonicalPlatformAuthorityUUID(value, label string) (uuid.UUID, error) {
	parsed, err := uuid.Parse(value)
	if err != nil || parsed == uuid.Nil || parsed.String() != value {
		return uuid.Nil, problem.New(400, "invalid_platform_routing_publication_identifier", label+" must be a canonical lowercase UUID.")
	}
	return parsed, nil
}

func parseCanonicalPlatformAuthorityTime(value, label string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.Location() != time.UTC || parsed.Format(time.RFC3339Nano) != value {
		return time.Time{}, problem.New(400, "invalid_platform_routing_publication_timestamp", label+" must be canonical UTC RFC3339Nano.")
	}
	return parsed, nil
}

func normalizePlatformAuthorityIdentifier(value string, maximum int) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) == 0 || len(value) > maximum {
		return "", fmt.Errorf("must contain between 1 and %d safe ASCII characters", maximum)
	}
	for index := 0; index < len(value); index++ {
		if value[index] < 0x21 || value[index] > 0x7e {
			return "", errors.New("must use safe ASCII without whitespace")
		}
	}
	return value, nil
}

func normalizePlatformAuthorityDomain(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 200 || strings.ContainsAny(value, "\r\n\t\x00") {
		return "", errors.New("must contain between 1 and 200 characters without control whitespace")
	}
	return value, nil
}

func samePlatformAuthorityOwnership(actual, expected *uuid.UUID) bool {
	if actual == nil || expected == nil {
		return actual == nil && expected == nil
	}
	return *actual == *expected
}

func copyUUIDPointer(value *uuid.UUID) *uuid.UUID {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func platformAuthorityAuthenticationFailed() error {
	return problem.New(401, "platform_routing_publisher_authentication_failed", "Platform routing publisher authentication failed.")
}

func platformAuthorityScopeDenied() error {
	return problem.New(403, "platform_routing_publisher_scope_denied", "The authenticated publisher is not authorized for this Target authority.")
}

func platformAuthorityReceiptCorrupt(err error) error {
	return problem.Wrap(500, "platform_routing_publication_receipt_corrupt", "Platform routing publication receipt integrity validation failed.", err)
}
