package executiontargets

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"

	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	ProcessContainmentTrustDisabled    = "disabled"
	ProcessContainmentTrustSignedV1    = "signed-v1"
	ProtectedCgroupSupervisorVersionV3 = "agentd-protected-cgroup-supervisor-v3"
	ProtectedCgroupProbeVersionV3      = 2
)

type ProcessContainmentPolicy struct {
	TrustMode       string            `json:"trustMode"`
	KeyID           string            `json:"keyId,omitempty"`
	PublicKey       ed25519.PublicKey `json:"-"`
	PublicKeySHA256 string            `json:"publicKeySha256,omitempty"`
}

func ParseProcessContainmentPolicy(capabilities map[string]any) (ProcessContainmentPolicy, error) {
	rawPolicy, found := capabilities["processContainmentPolicy"]
	if !found {
		return ProcessContainmentPolicy{TrustMode: ProcessContainmentTrustDisabled}, nil
	}
	policy, ok := rawPolicy.(map[string]any)
	if !ok {
		return ProcessContainmentPolicy{}, invalidProcessContainmentPolicy("processContainmentPolicy must be a JSON object.")
	}
	rawTrustMode, found := policy["trustMode"]
	if !found {
		return ProcessContainmentPolicy{}, invalidProcessContainmentPolicy("processContainmentPolicy.trustMode is required.")
	}
	trustMode, ok := rawTrustMode.(string)
	if !ok {
		return ProcessContainmentPolicy{}, invalidProcessContainmentPolicy("processContainmentPolicy.trustMode must be a string.")
	}
	switch trustMode {
	case ProcessContainmentTrustDisabled:
		if err := requireExactPolicyFields(policy, "trustMode"); err != nil {
			return ProcessContainmentPolicy{}, err
		}
		return ProcessContainmentPolicy{TrustMode: ProcessContainmentTrustDisabled}, nil
	case ProcessContainmentTrustSignedV1:
		if err := requireExactPolicyFields(policy, "trustMode", "keyId", "ed25519PublicKey"); err != nil {
			return ProcessContainmentPolicy{}, err
		}
		keyID, err := parseProcessContainmentKeyID(policy["keyId"])
		if err != nil {
			return ProcessContainmentPolicy{}, err
		}
		publicKey, err := parseProcessContainmentPublicKey(policy["ed25519PublicKey"])
		if err != nil {
			return ProcessContainmentPolicy{}, err
		}
		return ProcessContainmentPolicy{
			TrustMode:       ProcessContainmentTrustSignedV1,
			KeyID:           keyID,
			PublicKey:       publicKey,
			PublicKeySHA256: processContainmentPublicKeySHA256(publicKey),
		}, nil
	default:
		return ProcessContainmentPolicy{}, invalidProcessContainmentPolicy("processContainmentPolicy.trustMode is unsupported.")
	}
}

func normalizeProcessContainmentPolicyCapabilities(capabilities map[string]any) (map[string]any, error) {
	policy, err := ParseProcessContainmentPolicy(capabilities)
	if err != nil {
		return nil, err
	}
	normalized := make(map[string]any, len(capabilities))
	for key, value := range capabilities {
		normalized[key] = value
	}
	if _, found := capabilities["processContainmentPolicy"]; found {
		switch policy.TrustMode {
		case ProcessContainmentTrustDisabled:
			normalized["processContainmentPolicy"] = map[string]any{
				"trustMode": ProcessContainmentTrustDisabled,
			}
		case ProcessContainmentTrustSignedV1:
			normalized["processContainmentPolicy"] = map[string]any{
				"trustMode":        ProcessContainmentTrustSignedV1,
				"keyId":            policy.KeyID,
				"ed25519PublicKey": base64.StdEncoding.EncodeToString(policy.PublicKey),
			}
		}
	}
	return normalized, nil
}

func requireExactPolicyFields(policy map[string]any, required ...string) error {
	allowed := make(map[string]struct{}, len(required))
	for _, field := range required {
		allowed[field] = struct{}{}
	}
	for field := range policy {
		if _, ok := allowed[field]; !ok {
			return invalidProcessContainmentPolicy("processContainmentPolicy contains an unknown field.")
		}
	}
	for _, field := range required {
		if _, ok := policy[field]; !ok {
			return invalidProcessContainmentPolicy("processContainmentPolicy is missing a required field.")
		}
	}
	return nil
}

func parseProcessContainmentKeyID(value any) (string, error) {
	keyID, ok := value.(string)
	if !ok {
		return "", invalidProcessContainmentPolicy("processContainmentPolicy.keyId must be a string.")
	}
	if len(keyID) == 0 || len(keyID) > 160 {
		return "", invalidProcessContainmentPolicy("processContainmentPolicy.keyId length is invalid.")
	}
	for index := 0; index < len(keyID); index++ {
		if keyID[index] < 0x21 || keyID[index] > 0x7e {
			return "", invalidProcessContainmentPolicy("processContainmentPolicy.keyId must use safe ASCII without whitespace.")
		}
	}
	return keyID, nil
}

func parseProcessContainmentPublicKey(value any) (ed25519.PublicKey, error) {
	encoded, ok := value.(string)
	if !ok {
		return nil, invalidProcessContainmentPolicy("processContainmentPolicy.ed25519PublicKey must be a base64 string.")
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, invalidProcessContainmentPolicy("processContainmentPolicy.ed25519PublicKey must be valid base64.")
		}
	}
	if len(decoded) != ed25519.PublicKeySize {
		return nil, invalidProcessContainmentPolicy("processContainmentPolicy.ed25519PublicKey must decode to an Ed25519 public key.")
	}
	return ed25519.PublicKey(append([]byte(nil), decoded...)), nil
}

func processContainmentPublicKeySHA256(publicKey ed25519.PublicKey) string {
	digest := sha256.Sum256(publicKey)
	return hex.EncodeToString(digest[:])
}

func invalidProcessContainmentPolicy(message string) error {
	return problem.New(400, "invalid_execution_target_process_containment_policy", message)
}
