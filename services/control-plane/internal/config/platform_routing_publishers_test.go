package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/routing"
)

func TestLoadParsesStrictPlatformRoutingPublisherConfiguration(t *testing.T) {
	clearConfigEnvironment(t)
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	targetID := uuid.New()
	t.Setenv("SYNARA_PLATFORM_ROUTING_PUBLISHERS_JSON", `{"publishers":[{
		"publisherIdentity":"production-routing-publisher",
		"keys":[{"keyId":"key-v1","ed25519PublicKey":"`+base64.StdEncoding.EncodeToString(publicKey)+`"}],
		"targets":[{
			"executionTargetId":"`+targetID.String()+`",
			"ownership":"platform-shared",
			"publishHealth":true,
			"publishReservations":true,
			"drRoutes":[{"sourceDrDomain":"region-a/cluster-a","drDomain":"region-b/cluster-b"}]
		}]
	}]}`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.PlatformRoutingPublishers) != 1 ||
		cfg.PlatformRoutingPublishers[0].PublisherIdentity != "production-routing-publisher" ||
		len(cfg.PlatformRoutingPublishers[0].Keys) != 1 ||
		len(cfg.PlatformRoutingPublishers[0].Targets) != 1 ||
		cfg.PlatformRoutingPublishers[0].Targets[0].ExecutionTargetID != targetID ||
		cfg.PlatformRoutingPublishers[0].Targets[0].Ownership != routing.PlatformAuthorityOwnershipShared ||
		!cfg.PlatformRoutingPublishers[0].Targets[0].PublishHealth ||
		!cfg.PlatformRoutingPublishers[0].Targets[0].PublishReservations {
		t.Fatalf("Platform routing publisher config = %#v", cfg.PlatformRoutingPublishers)
	}
}

func TestLoadRejectsUnknownAndOverlappingPlatformRoutingPublisherConfiguration(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encodedKey := base64.StdEncoding.EncodeToString(publicKey)
	targetID := uuid.NewString()

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_PLATFORM_ROUTING_PUBLISHERS_JSON", `[{
		"publisherIdentity":"publisher-a",
		"keys":[{"keyId":"key-a","ed25519PublicKey":"`+encodedKey+`","privateKey":"must-not-be-accepted"}],
		"targets":[{"executionTargetId":"`+targetID+`","ownership":"platform-shared","publishHealth":true,"drRoutes":[]}]
	}]`)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown Platform publisher field err = %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("SYNARA_PLATFORM_ROUTING_PUBLISHERS_JSON", `[
		{"publisherIdentity":"publisher-a","keys":[{"keyId":"key-a","ed25519PublicKey":"`+encodedKey+`"}],"targets":[{"executionTargetId":"`+targetID+`","ownership":"platform-shared","publishHealth":true,"drRoutes":[]}]},
		{"publisherIdentity":"publisher-b","keys":[{"keyId":"key-b","ed25519PublicKey":"`+encodedKey+`"}],"targets":[{"executionTargetId":"`+targetID+`","ownership":"platform-shared","publishHealth":true,"drRoutes":[]}]}
	]`)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "overlaps publishers") {
		t.Fatalf("overlapping Platform publisher authority err = %v", err)
	}
}
