package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/routing"
)

type platformRoutingPublisherEnvelope struct {
	Publishers *[]platformRoutingPublisherJSON `json:"publishers"`
}

type platformRoutingPublisherJSON struct {
	PublisherIdentity string                      `json:"publisherIdentity"`
	Keys              []platformRoutingKeyJSON    `json:"keys"`
	Targets           []platformRoutingTargetJSON `json:"targets"`
}

type platformRoutingKeyJSON struct {
	KeyID            string `json:"keyId"`
	Ed25519PublicKey string `json:"ed25519PublicKey"`
}

type platformRoutingTargetJSON struct {
	ExecutionTargetID   string                       `json:"executionTargetId"`
	Ownership           string                       `json:"ownership"`
	TenantID            *string                      `json:"tenantId,omitempty"`
	PublishHealth       bool                         `json:"publishHealth"`
	PublishReservations bool                         `json:"publishReservations"`
	DRRoutes            []platformRoutingDRRouteJSON `json:"drRoutes"`
}

type platformRoutingDRRouteJSON struct {
	SourceDRDomain string `json:"sourceDrDomain"`
	DRDomain       string `json:"drDomain"`
}

func parsePlatformRoutingPublishers(raw string) ([]routing.PlatformAuthorityPublisherConfig, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var items []platformRoutingPublisherJSON
	switch raw[0] {
	case '[':
		if err := decodeStrictConfigJSON(raw, &items); err != nil {
			return nil, err
		}
	case '{':
		envelope := platformRoutingPublisherEnvelope{}
		if err := decodeStrictConfigJSON(raw, &envelope); err != nil {
			return nil, err
		}
		if envelope.Publishers == nil {
			return nil, errors.New("must be a JSON array or an object with a publishers array")
		}
		items = *envelope.Publishers
	default:
		return nil, errors.New("must be a JSON array or an object with a publishers array")
	}

	configs := make([]routing.PlatformAuthorityPublisherConfig, 0, len(items))
	for publisherIndex, item := range items {
		publisher := routing.PlatformAuthorityPublisherConfig{PublisherIdentity: item.PublisherIdentity}
		for keyIndex, rawKey := range item.Keys {
			publicKey, err := decodePlatformRoutingPublicKey(rawKey.Ed25519PublicKey)
			if err != nil {
				return nil, fmt.Errorf("publishers[%d].keys[%d].ed25519PublicKey: %w", publisherIndex, keyIndex, err)
			}
			publisher.Keys = append(publisher.Keys, routing.PlatformAuthorityPublisherKey{
				KeyID: rawKey.KeyID, PublicKey: publicKey,
			})
		}
		for targetIndex, rawTarget := range item.Targets {
			targetID, err := uuid.Parse(strings.TrimSpace(rawTarget.ExecutionTargetID))
			if err != nil || targetID == uuid.Nil {
				return nil, fmt.Errorf("publishers[%d].targets[%d].executionTargetId must be a UUID", publisherIndex, targetIndex)
			}
			target := routing.PlatformAuthorityTargetScope{
				ExecutionTargetID:   targetID,
				Ownership:           rawTarget.Ownership,
				PublishHealth:       rawTarget.PublishHealth,
				PublishReservations: rawTarget.PublishReservations,
			}
			if rawTarget.TenantID != nil {
				tenantID, parseErr := uuid.Parse(strings.TrimSpace(*rawTarget.TenantID))
				if parseErr != nil || tenantID == uuid.Nil {
					return nil, fmt.Errorf("publishers[%d].targets[%d].tenantId must be a UUID", publisherIndex, targetIndex)
				}
				target.TenantID = &tenantID
			}
			for _, rawRoute := range rawTarget.DRRoutes {
				target.DRRoutes = append(target.DRRoutes, routing.PlatformAuthorityDRRouteScope{
					SourceDRDomain: rawRoute.SourceDRDomain,
					DRDomain:       rawRoute.DRDomain,
				})
			}
			publisher.Targets = append(publisher.Targets, target)
		}
		configs = append(configs, publisher)
	}
	return routing.NormalizePlatformAuthorityPublisherConfigs(configs)
}

func decodePlatformRoutingPublicKey(encoded string) (ed25519.PublicKey, error) {
	encoded = strings.TrimSpace(encoded)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(encoded)
	}
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return nil, errors.New("must decode to an Ed25519 public key")
	}
	return ed25519.PublicKey(append([]byte(nil), decoded...)), nil
}
