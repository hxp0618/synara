package validation

import (
	"encoding/json"
	"errors"
	"strings"
)

func CommandJSON(value string) ([]string, error) {
	command := make([]string, 0)
	if err := json.Unmarshal([]byte(strings.TrimSpace(value)), &command); err != nil || len(command) == 0 {
		return nil, errors.New("must be a non-empty JSON string array")
	}
	for _, part := range command {
		if strings.TrimSpace(part) == "" || strings.ContainsRune(part, '\x00') {
			return nil, errors.New("contains an invalid argument")
		}
	}
	return command, nil
}
