// Package databasetime normalizes timestamps returned by both PostgreSQL and
// SQLite aggregate queries. Drivers do not agree on whether those values are
// time.Time or formatted text, so callers should scan text and parse it here.
package databasetime

import (
	"fmt"
	"strings"
	"time"
)

// Parse accepts the timestamp shapes emitted by the supported PostgreSQL and
// SQLite drivers and returns a UTC value.
func Parse(value string) (time.Time, error) {
	trimmed := strings.TrimSpace(value)
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02 15:04:05.999999999Z07",
		"2006-01-02 15:04:05Z07",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	} {
		parsed, err := time.Parse(layout, trimmed)
		if err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("parse database timestamp %q", value)
}
