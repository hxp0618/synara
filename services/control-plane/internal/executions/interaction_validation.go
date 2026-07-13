package executions

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	maximumUserInputQuestions       = 100
	maximumUserInputAnswerBytes     = 100_000
	maximumUserInputResolutionBytes = 1_000_000
)

type userInputQuestionSchema struct {
	multiSelect bool
	options     map[string]struct{}
}

func validateUserInputResolution(payload, answers map[string]any) error {
	if answers == nil {
		return problem.New(400, "invalid_user_input_answers", "answers must be a JSON object.")
	}
	encoded, err := json.Marshal(answers)
	if err != nil || len(encoded) > maximumUserInputResolutionBytes {
		return problem.New(400, "invalid_user_input_answers", "answers exceed the maximum encoded size.")
	}
	questions, ok := payload["questions"].([]any)
	if !ok || len(questions) == 0 || len(questions) > maximumUserInputQuestions {
		return problem.New(500, "interaction_event_payload_corrupt", "The persisted user-input questions are invalid.")
	}
	schemas := make(map[string]userInputQuestionSchema, len(questions))
	for _, rawQuestion := range questions {
		question, ok := rawQuestion.(map[string]any)
		if !ok {
			return problem.New(500, "interaction_event_payload_corrupt", "The persisted user-input question is invalid.")
		}
		questionID, ok := question["id"].(string)
		questionID = strings.TrimSpace(questionID)
		if !ok || questionID == "" {
			return problem.New(500, "interaction_event_payload_corrupt", "The persisted user-input question id is invalid.")
		}
		if _, duplicate := schemas[questionID]; duplicate {
			return problem.New(500, "interaction_event_payload_corrupt", "The persisted user-input question ids are not unique.")
		}
		options, ok := question["options"].([]any)
		if !ok {
			return problem.New(500, "interaction_event_payload_corrupt", "The persisted user-input options are invalid.")
		}
		optionLabels := make(map[string]struct{}, len(options))
		for _, rawOption := range options {
			option, ok := rawOption.(map[string]any)
			if !ok {
				return problem.New(500, "interaction_event_payload_corrupt", "The persisted user-input option is invalid.")
			}
			label, ok := option["label"].(string)
			label = strings.TrimSpace(label)
			if !ok || label == "" {
				return problem.New(500, "interaction_event_payload_corrupt", "The persisted user-input option label is invalid.")
			}
			if _, duplicate := optionLabels[label]; duplicate {
				return problem.New(500, "interaction_event_payload_corrupt", "The persisted user-input option labels are not unique.")
			}
			optionLabels[label] = struct{}{}
		}
		schemas[questionID] = userInputQuestionSchema{
			multiSelect: question["multiSelect"] == true,
			options:     optionLabels,
		}
	}
	if len(answers) != len(schemas) {
		return problem.New(400, "invalid_user_input_answers", "answers must contain exactly one answer for every question.")
	}
	for questionID, value := range answers {
		schema, known := schemas[questionID]
		if !known {
			return problem.New(400, "invalid_user_input_answers", fmt.Sprintf("answer %q does not match a pending question.", questionID))
		}
		if err := validateUserInputAnswer(questionID, value, schema); err != nil {
			return err
		}
	}
	return nil
}

func validateUserInputAnswer(questionID string, value any, schema userInputQuestionSchema) error {
	switch answer := value.(type) {
	case string:
		if err := validateUserInputAnswerText(answer); err != nil {
			return problem.New(400, "invalid_user_input_answers", fmt.Sprintf("answer %q %s", questionID, err.Error()))
		}
		return nil
	case []string:
		values := make([]any, len(answer))
		for index, entry := range answer {
			values[index] = entry
		}
		return validateUserInputSelections(questionID, values, schema)
	case []any:
		return validateUserInputSelections(questionID, answer, schema)
	default:
		return problem.New(400, "invalid_user_input_answers", fmt.Sprintf("answer %q must be a string or string array.", questionID))
	}
}

func validateUserInputSelections(questionID string, values []any, schema userInputQuestionSchema) error {
	if !schema.multiSelect {
		return problem.New(400, "invalid_user_input_answers", fmt.Sprintf("answer %q does not allow multiple selections.", questionID))
	}
	if len(values) == 0 || len(values) > maximumUserInputQuestions {
		return problem.New(400, "invalid_user_input_answers", fmt.Sprintf("answer %q must contain between 1 and 100 selections.", questionID))
	}
	seen := make(map[string]struct{}, len(values))
	for _, rawValue := range values {
		value, ok := rawValue.(string)
		value = strings.TrimSpace(value)
		if !ok || value == "" || len(value) > maximumUserInputAnswerBytes {
			return problem.New(400, "invalid_user_input_answers", fmt.Sprintf("answer %q contains an invalid selection.", questionID))
		}
		if _, allowed := schema.options[value]; !allowed {
			return problem.New(400, "invalid_user_input_answers", fmt.Sprintf("answer %q contains an unknown selection.", questionID))
		}
		if _, duplicate := seen[value]; duplicate {
			return problem.New(400, "invalid_user_input_answers", fmt.Sprintf("answer %q contains a duplicate selection.", questionID))
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validateUserInputAnswerText(value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fmt.Errorf("must not be empty.")
	}
	if len(value) > maximumUserInputAnswerBytes {
		return fmt.Errorf("exceeds the maximum size.")
	}
	return nil
}
