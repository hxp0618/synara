package recoverybundle

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestDecodeAndValidateChecksCanonicalHashAndImmutableEnvelope(t *testing.T) {
	model := persistence.ExecutionRecoveryBundle{
		ID: uuid.New(), TenantID: uuid.New(), SessionID: uuid.New(), TurnID: uuid.New(),
		ExecutionID: uuid.New(), Generation: 2, SchemaVersion: SchemaVersionV1,
		RecoveryReason: "execution-recovery", AuthoritativeHistorySequence: 19, CreatedAt: time.Now().UTC(),
	}
	payload := map[string]any{
		"schemaVersion": model.SchemaVersion, "executionId": model.ExecutionID,
		"sessionId": model.SessionID, "turnId": model.TurnID, "generation": model.Generation,
		"recoveryReason":               model.RecoveryReason,
		"authoritativeHistorySequence": model.AuthoritativeHistorySequence,
		"execution":                    map[string]any{},
		"workload": map[string]any{
			"sessionId": model.SessionID, "turnId": model.TurnID,
			"resumeSnapshot": map[string]any{"authoritativeHistorySequence": model.AuthoritativeHistorySequence},
		},
	}
	var err error
	model.Payload, model.PayloadSHA256, err = Encode(payload)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := DecodeAndValidate(model, &decoded); err != nil {
		t.Fatalf("validate canonical Recovery Bundle: %v", err)
	}

	hashTampered := model
	hashTampered.PayloadSHA256 = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	if err := DecodeAndValidate(hashTampered, nil); !errors.Is(err, ErrIntegrityFailed) {
		t.Fatalf("hash tamper error = %v", err)
	}

	envelopeTampered := model
	envelopeTampered.Payload = clonePayload(t, model.Payload)
	envelopeTampered.Payload["executionId"] = uuid.New()
	envelopeTampered.Payload, envelopeTampered.PayloadSHA256, err = Encode(envelopeTampered.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := DecodeAndValidate(envelopeTampered, nil); !errors.Is(err, ErrEnvelopeMismatch) {
		t.Fatalf("envelope tamper error = %v", err)
	}
}

func clonePayload(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	cloned, _, err := Encode(payload)
	if err != nil {
		t.Fatal(err)
	}
	return cloned
}
