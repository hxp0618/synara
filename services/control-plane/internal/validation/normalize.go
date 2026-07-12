package validation

import (
	"net/mail"
	"regexp"
	"strings"

	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
var codePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,63}$`)

func Email(value string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	parsed, err := mail.ParseAddress(normalized)
	if err != nil || parsed.Address != normalized || len(normalized) > 320 {
		return "", problem.New(400, "invalid_email", "Enter a valid email address.")
	}
	return normalized, nil
}

func Name(value, code, label string, maxLength int) (string, error) {
	normalized := strings.TrimSpace(value)
	if len(normalized) == 0 || len(normalized) > maxLength {
		return "", problem.New(400, code, label+" must be between 1 and "+itoa(maxLength)+" characters.")
	}
	return normalized, nil
}

func Slug(value, code, label string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if !slugPattern.MatchString(normalized) {
		return "", problem.New(400, code, label+" must contain 3-63 lowercase letters, numbers, or hyphens.")
	}
	return normalized, nil
}

func Code(value, fallback, errorCode, label string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" {
		normalized = fallback
	}
	if !codePattern.MatchString(normalized) {
		return "", problem.New(400, errorCode, label+" is invalid.")
	}
	return normalized, nil
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	buffer := [20]byte{}
	position := len(buffer)
	for value > 0 {
		position--
		buffer[position] = byte('0' + value%10)
		value /= 10
	}
	return string(buffer[position:])
}
