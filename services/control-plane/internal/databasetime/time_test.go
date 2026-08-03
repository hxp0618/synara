package databasetime

import (
	"testing"
	"time"
)

func TestParseSupportedDatabaseTimestampShapes(t *testing.T) {
	want := time.Date(2026, 7, 30, 10, 30, 0, 123456000, time.UTC)
	for _, value := range []string{
		"2026-07-30T10:30:00.123456Z",
		"2026-07-30 10:30:00.123456+00:00",
		"2026-07-30 10:30:00.123456+00",
		"2026-07-30 10:30:00.123456",
	} {
		parsed, err := Parse(value)
		if err != nil || !parsed.Equal(want) {
			t.Fatalf("Parse(%q) = %s, %v, want %s", value, parsed, err, want)
		}
	}
}

func TestParseRejectsUnknownTimestamp(t *testing.T) {
	if _, err := Parse("not-a-timestamp"); err == nil {
		t.Fatal("invalid database timestamp was accepted")
	}
}
