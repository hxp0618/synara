package executions

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func TestValidateRuntimeEventContract(t *testing.T) {
	valid := RuntimeEventInput{
		EventID: uuid.New(), EventVersion: RuntimeEventVersionV1,
		EventType: "runtime.output.delta", Payload: map[string]any{"text": "hello"},
	}
	if err := validateRuntimeEventContract(valid); err != nil {
		t.Fatalf("valid Runtime Event was rejected: %v", err)
	}

	tests := []struct {
		name       string
		mutate     func(*RuntimeEventInput)
		wantStatus int
		wantCode   string
	}{
		{
			name: "unsupported version",
			mutate: func(input *RuntimeEventInput) {
				input.EventVersion = RuntimeEventVersionV1 + 1
			},
			wantStatus: 422,
			wantCode:   "runtime_event_version_unsupported",
		},
		{
			name: "missing payload",
			mutate: func(input *RuntimeEventInput) {
				input.Payload = nil
			},
			wantStatus: 400,
			wantCode:   "invalid_runtime_event_payload",
		},
		{
			name: "non JSON payload",
			mutate: func(input *RuntimeEventInput) {
				input.Payload = map[string]any{"value": math.NaN()}
			},
			wantStatus: 400,
			wantCode:   "invalid_runtime_event_payload",
		},
		{
			name: "oversized payload",
			mutate: func(input *RuntimeEventInput) {
				input.Payload = map[string]any{"text": strings.Repeat("x", RuntimeEventMaxPayloadBytes)}
			},
			wantStatus: 413,
			wantCode:   "runtime_event_payload_too_large",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := valid
			test.mutate(&input)
			err := validateRuntimeEventContract(input)
			var apiError *problem.Error
			if !errors.As(err, &apiError) {
				t.Fatalf("expected problem error, got %v", err)
			}
			if apiError.Status != test.wantStatus || apiError.Code != test.wantCode {
				t.Fatalf("error = status %d code %q, want status %d code %q", apiError.Status, apiError.Code, test.wantStatus, test.wantCode)
			}
		})
	}
}
