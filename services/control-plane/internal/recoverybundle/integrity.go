package recoverybundle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

const SchemaVersionV1 = 1

var (
	ErrPayloadInvalid   = errors.New("Recovery Bundle payload is invalid")
	ErrIntegrityFailed  = errors.New("Recovery Bundle payload hash does not match its immutable envelope")
	ErrEnvelopeMismatch = errors.New("Recovery Bundle payload does not match its immutable execution envelope")
)

type persistedEnvelope struct {
	SchemaVersion                int        `json:"schemaVersion"`
	ExecutionID                  uuid.UUID  `json:"executionId"`
	SessionID                    uuid.UUID  `json:"sessionId"`
	TurnID                       uuid.UUID  `json:"turnId"`
	Generation                   int64      `json:"generation"`
	RecoveryReason               string     `json:"recoveryReason"`
	PreviousBundleID             *uuid.UUID `json:"previousBundleId,omitempty"`
	AuthoritativeHistorySequence int64      `json:"authoritativeHistorySequence"`
	Workload                     struct {
		SessionID      uuid.UUID `json:"sessionId"`
		TurnID         uuid.UUID `json:"turnId"`
		ResumeSnapshot *struct {
			AuthoritativeHistorySequence int64 `json:"authoritativeHistorySequence"`
		} `json:"resumeSnapshot"`
	} `json:"workload"`
}

// Encode normalizes a typed payload through JSON and hashes the same canonical
// object representation persisted by the Control Plane.
func Encode(payload any) (map[string]any, string, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, "", err
	}
	var normalized map[string]any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		return nil, "", err
	}
	canonical, err := json.Marshal(normalized)
	if err != nil {
		return nil, "", err
	}
	return normalized, digest(canonical), nil
}

// DecodeAndValidate verifies both the canonical payload hash and every field
// duplicated by the immutable database envelope before decoding into output.
// Callers map the sentinel errors to their API-specific problem codes.
func DecodeAndValidate(model persistence.ExecutionRecoveryBundle, output any) error {
	canonical, err := json.Marshal(model.Payload)
	if err != nil {
		return errors.Join(ErrPayloadInvalid, err)
	}
	if digest(canonical) != model.PayloadSHA256 {
		return ErrIntegrityFailed
	}
	var envelope persistedEnvelope
	if err := json.Unmarshal(canonical, &envelope); err != nil {
		return errors.Join(ErrPayloadInvalid, err)
	}
	if envelope.SchemaVersion != SchemaVersionV1 ||
		model.SchemaVersion != envelope.SchemaVersion ||
		model.ExecutionID != envelope.ExecutionID ||
		model.SessionID != envelope.SessionID ||
		model.TurnID != envelope.TurnID ||
		model.Generation != envelope.Generation ||
		model.RecoveryReason != envelope.RecoveryReason ||
		!equalUUIDPointers(model.PreviousBundleID, envelope.PreviousBundleID) ||
		model.AuthoritativeHistorySequence != envelope.AuthoritativeHistorySequence ||
		envelope.Workload.SessionID != model.SessionID ||
		envelope.Workload.TurnID != model.TurnID ||
		envelope.Workload.ResumeSnapshot == nil ||
		envelope.Workload.ResumeSnapshot.AuthoritativeHistorySequence != model.AuthoritativeHistorySequence {
		return ErrEnvelopeMismatch
	}
	if output != nil {
		if err := json.Unmarshal(canonical, output); err != nil {
			return errors.Join(ErrPayloadInvalid, err)
		}
	}
	return nil
}

func digest(payload []byte) string {
	hash := sha256.Sum256(payload)
	return hex.EncodeToString(hash[:])
}

func equalUUIDPointers(left, right *uuid.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
