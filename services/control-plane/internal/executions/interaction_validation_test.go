package executions

import (
	"errors"
	"strings"
	"testing"

	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func TestValidateUserInputResolution(t *testing.T) {
	payload := map[string]any{
		"questions": []any{
			map[string]any{
				"id": "environment", "header": "Environment", "question": "Which environment?",
				"options": []any{},
			},
			map[string]any{
				"id": "checks", "header": "Checks", "question": "Which checks?", "multiSelect": true,
				"options": []any{
					map[string]any{"label": "Unit", "description": "Unit tests"},
					map[string]any{"label": "Integration", "description": "Integration tests"},
				},
			},
		},
	}

	if err := validateUserInputResolution(payload, map[string]any{
		"environment": "staging",
		"checks":      []any{"Unit", "Integration"},
	}); err != nil {
		t.Fatalf("valid answers were rejected: %v", err)
	}

	tests := []struct {
		name    string
		answers map[string]any
	}{
		{name: "missing", answers: map[string]any{"environment": "staging"}},
		{name: "extra", answers: map[string]any{"environment": "staging", "checks": "Unit", "other": "value"}},
		{name: "empty", answers: map[string]any{"environment": " ", "checks": []any{"Unit"}}},
		{name: "single array", answers: map[string]any{"environment": []any{"staging"}, "checks": []any{"Unit"}}},
		{name: "unknown selection", answers: map[string]any{"environment": "staging", "checks": []any{"Smoke"}}},
		{name: "duplicate selection", answers: map[string]any{"environment": "staging", "checks": []any{"Unit", "Unit"}}},
		{name: "oversized", answers: map[string]any{"environment": strings.Repeat("x", maximumUserInputAnswerBytes+1), "checks": []any{"Unit"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateUserInputResolution(payload, test.answers)
			var apiError *problem.Error
			if !errors.As(err, &apiError) || apiError.Code != "invalid_user_input_answers" {
				t.Fatalf("expected invalid_user_input_answers, got %v", err)
			}
		})
	}
}

func TestValidateUserInputResolutionRejectsCorruptDuplicateOptionLabels(t *testing.T) {
	err := validateUserInputResolution(map[string]any{
		"questions": []any{map[string]any{
			"id": "checks", "header": "Checks", "question": "Which checks?", "multiSelect": true,
			"options": []any{
				map[string]any{"label": "Unit", "description": "First"},
				map[string]any{"label": "Unit", "description": "Duplicate"},
			},
		}},
	}, map[string]any{"checks": []any{"Unit"}})
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "interaction_event_payload_corrupt" {
		t.Fatalf("expected corrupt option labels to be rejected, got %v", err)
	}
}
