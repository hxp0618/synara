package secretguard

import (
	"encoding/json"
	"unicode/utf8"
)

const (
	maximumSanitizeDepth = 64
	maximumSanitizeNodes = 100_000
)

func (g *Guard) SanitizeString(value string) (string, bool, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.closed || g.matcher == nil {
		return "", false, ErrClosed
	}
	if !utf8.ValidString(value) {
		return "", false, newExposureError(ReasonInvalidText)
	}
	result, count, err := redactBytes(g.matcher, []byte(value))
	if err != nil {
		return "", false, err
	}
	defer zero(result)
	return string(result), count > 0, nil
}

// Sanitize recursively copies a JSON-compatible value. String values are
// redacted; a matching map key is rejected because renaming keys can change
// protocol semantics.
func (g *Guard) Sanitize(value any) (any, bool, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.closed || g.matcher == nil {
		return nil, false, ErrClosed
	}
	nodes := 0
	return sanitizeValue(g.matcher, value, 0, &nodes)
}

func sanitizeValue(m *matcher, value any, depth int, nodes *int) (any, bool, error) {
	*nodes = *nodes + 1
	if depth > maximumSanitizeDepth || *nodes > maximumSanitizeNodes {
		return nil, false, newExposureError(ReasonValueLimit)
	}
	switch typed := value.(type) {
	case nil, bool, float64, float32, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, json.Number:
		return typed, false, nil
	case string:
		if !utf8.ValidString(typed) {
			return nil, false, newExposureError(ReasonInvalidText)
		}
		result, count, err := redactBytes(m, []byte(typed))
		if err != nil {
			return nil, false, err
		}
		defer zero(result)
		return string(result), count > 0, nil
	case []byte:
		if detectRepresentations(m, typed) {
			return nil, false, newExposureError(ReasonBinaryMatch)
		}
		return append([]byte(nil), typed...), false, nil
	case map[string]any:
		result := make(map[string]any, len(typed))
		changed := false
		for key, item := range typed {
			if !utf8.ValidString(key) {
				return nil, false, newExposureError(ReasonInvalidText)
			}
			if detectRepresentations(m, []byte(key)) {
				return nil, false, newExposureError(ReasonMapKeyMatch)
			}
			sanitized, itemChanged, err := sanitizeValue(m, item, depth+1, nodes)
			if err != nil {
				return nil, false, err
			}
			result[key] = sanitized
			changed = changed || itemChanged
		}
		return result, changed, nil
	case map[string]string:
		result := make(map[string]string, len(typed))
		changed := false
		for key, item := range typed {
			if !utf8.ValidString(key) {
				return nil, false, newExposureError(ReasonInvalidText)
			}
			if detectRepresentations(m, []byte(key)) {
				return nil, false, newExposureError(ReasonMapKeyMatch)
			}
			sanitized, itemChanged, err := sanitizeValue(m, item, depth+1, nodes)
			if err != nil {
				return nil, false, err
			}
			result[key] = sanitized.(string)
			changed = changed || itemChanged
		}
		return result, changed, nil
	case []any:
		result := make([]any, len(typed))
		changed := false
		for index, item := range typed {
			sanitized, itemChanged, err := sanitizeValue(m, item, depth+1, nodes)
			if err != nil {
				return nil, false, err
			}
			result[index] = sanitized
			changed = changed || itemChanged
		}
		return result, changed, nil
	case []string:
		result := make([]string, len(typed))
		changed := false
		for index, item := range typed {
			sanitized, itemChanged, err := sanitizeValue(m, item, depth+1, nodes)
			if err != nil {
				return nil, false, err
			}
			result[index] = sanitized.(string)
			changed = changed || itemChanged
		}
		return result, changed, nil
	default:
		return nil, false, newExposureError(ReasonUnsupportedValue)
	}
}

func redactBytes(m *matcher, value []byte) ([]byte, int, error) {
	best, found := representationMatches(m, value)
	if !found {
		return append([]byte(nil), value...), 0, nil
	}
	result := make([]byte, 0, len(value))
	redactions := 0
	for position := 0; position < len(value); {
		if best[position] > 0 {
			result = append(result, RedactionMarker...)
			redactions++
			position += int(best[position])
			continue
		}
		result = append(result, value[position])
		position++
	}
	if !utf8.Valid(result) {
		zero(result)
		return nil, 0, newExposureError(ReasonInvalidText)
	}
	return result, redactions, nil
}
