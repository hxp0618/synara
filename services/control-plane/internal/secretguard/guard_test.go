package secretguard

import (
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestGuardRedactsAllCredentialRepresentations(t *testing.T) {
	secret := []byte("s3cr3t+/ ?\n")
	username := []byte("registry-user")
	guard, err := New([]Secret{{Value: secret, BasicAuthUsername: username}})
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()

	basicValue := append(append([]byte(nil), username...), ':')
	basicValue = append(basicValue, secret...)
	variants := map[string][]byte{
		"raw":                secret,
		"json escaped":       jsonEscape(secret),
		"percent all upper":  percentEncode(secret, true, true),
		"percent all lower":  percentEncode(secret, true, false),
		"percent URL upper":  percentEncode(secret, false, true),
		"percent URL lower":  percentEncode(secret, false, false),
		"base64 std":         []byte(base64.StdEncoding.EncodeToString(secret)),
		"base64 raw std":     []byte(base64.RawStdEncoding.EncodeToString(secret)),
		"base64 URL":         []byte(base64.URLEncoding.EncodeToString(secret)),
		"base64 raw URL":     []byte(base64.RawURLEncoding.EncodeToString(secret)),
		"basic raw":          basicValue,
		"basic header":       []byte("Basic " + base64.StdEncoding.EncodeToString(basicValue)),
		"basic lower header": []byte("basic " + base64.StdEncoding.EncodeToString(basicValue)),
	}
	for name, variant := range variants {
		t.Run(name, func(t *testing.T) {
			value := "before " + string(variant) + " after"
			sanitized, changed, sanitizeErr := guard.SanitizeString(value)
			if sanitizeErr != nil {
				t.Fatal(sanitizeErr)
			}
			if !changed || sanitized != "before "+RedactionMarker+" after" {
				t.Fatalf("representation was not fully redacted: changed=%v value=%q", changed, sanitized)
			}
		})
	}
	zero(basicValue)
}

func TestGuardUsesLeftmostLongestForOverlappingSecrets(t *testing.T) {
	guard, err := New([]Secret{{Value: []byte("abcdefgh")}, {Value: []byte("abcdefghijk")}})
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	sanitized, changed, err := guard.SanitizeString("abcdefghijk")
	if err != nil {
		t.Fatal(err)
	}
	if !changed || sanitized != RedactionMarker {
		t.Fatalf("longest same-start match was not selected: %q", sanitized)
	}

	earliest, err := New([]Secret{{Value: []byte("abcdefgh")}, {Value: []byte("bcdefghijk")}})
	if err != nil {
		t.Fatal(err)
	}
	defer earliest.Close()
	sanitized, changed, err = earliest.SanitizeString("abcdefghijk")
	if err != nil {
		t.Fatal(err)
	}
	if !changed || sanitized != RedactionMarker+"ijk" {
		t.Fatalf("leftmost match was not selected first: %q", sanitized)
	}
}

func TestGuardSanitizesNestedJSONWithoutMutatingInput(t *testing.T) {
	secret := "nested-secret"
	guard, err := New([]Secret{{Value: []byte(secret)}})
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	input := map[string]any{
		"message": "before " + secret + " after",
		"nested": []any{
			map[string]any{"value": secret},
			[]string{"safe", secret},
		},
		"count": 3,
	}
	sanitized, changed, err := guard.Sanitize(input)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("nested JSON was not reported as changed")
	}
	expected := map[string]any{
		"message": "before " + RedactionMarker + " after",
		"nested": []any{
			map[string]any{"value": RedactionMarker},
			[]string{"safe", RedactionMarker},
		},
		"count": 3,
	}
	if !reflect.DeepEqual(sanitized, expected) {
		t.Fatalf("unexpected sanitized value: %#v", sanitized)
	}
	if input["message"] != "before "+secret+" after" {
		t.Fatal("Sanitize mutated the input map")
	}
}

func TestGuardRejectsSecretInMapKeyAndBinaryValue(t *testing.T) {
	secret := []byte("map-key-secret")
	guard, err := New([]Secret{{Value: secret}})
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	_, _, err = guard.Sanitize(map[string]any{"prefix-" + string(secret): "value"})
	assertExposureReason(t, err, ReasonMapKeyMatch)
	_, _, err = guard.Sanitize(map[string]any{"payload": append([]byte("prefix-"), secret...)})
	assertExposureReason(t, err, ReasonBinaryMatch)
}

func TestGuardFailsClosedForShortSecretAndLimits(t *testing.T) {
	_, err := New([]Secret{{Value: []byte("1234567")}})
	assertExposureReason(t, err, ReasonSecretTooShort)

	limits := DefaultLimits()
	limits.MaximumSecrets = 1
	_, err = NewWithLimits([]Secret{{Value: []byte("secret-one")}, {Value: []byte("secret-two")}}, limits)
	assertExposureReason(t, err, ReasonPatternLimit)

	limits = DefaultLimits()
	limits.MaximumPatterns = 1
	_, err = NewWithLimits([]Secret{{Value: []byte("pattern-limit")}}, limits)
	assertExposureReason(t, err, ReasonPatternLimit)

	limits = DefaultLimits()
	limits.MaximumTotalPatternBytes = 16
	_, err = NewWithLimits([]Secret{{Value: []byte("pattern-bytes-limit")}}, limits)
	assertExposureReason(t, err, ReasonPatternBytesLimit)

	limits = DefaultLimits()
	limits.MaximumPatternBytes = 16
	_, err = NewWithLimits([]Secret{{Value: []byte("pattern-too-long")}}, limits)
	assertExposureReason(t, err, ReasonPatternTooLong)

	_, err = New([]Secret{{Value: []byte("[REDACTED]")}})
	assertExposureReason(t, err, ReasonUnsafeReplacement)
}

func TestGuardCloseInvalidatesStreamsAndClearsBuffers(t *testing.T) {
	guard, err := New([]Secret{{Value: []byte("close-secret")}})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := guard.NewStream(StreamText)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Transform([]byte("close-sec")); err != nil {
		t.Fatal(err)
	}
	if len(stream.pending) == 0 {
		t.Fatal("test did not create a pending cross-chunk buffer")
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if guard.matcher != nil || len(stream.pending) != 0 || !stream.closed {
		t.Fatalf("Close retained matcher or stream buffers: guard=%#v stream=%#v", guard.matcher, stream)
	}
	if _, err := stream.Transform([]byte("ret")); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed stream returned %v", err)
	}
	if stats := guard.Stats(); stats != (Stats{}) {
		t.Fatalf("closed guard retained stats: %#v", stats)
	}
}

func TestGuardRejectsUnsupportedJSONValues(t *testing.T) {
	guard, err := New([]Secret{{Value: []byte("value-secret")}})
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	_, _, err = guard.Sanitize(struct{ Value string }{Value: "safe"})
	assertExposureReason(t, err, ReasonUnsupportedValue)
}

func TestGuardCloseIsSafeDuringConcurrentReads(t *testing.T) {
	guard, err := New([]Secret{{Value: []byte("concurrent-secret")}})
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for index := 0; index < 8; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for iteration := 0; iteration < 100; iteration++ {
				_, _, sanitizeErr := guard.SanitizeString("safe value")
				if sanitizeErr != nil && !errors.Is(sanitizeErr, ErrClosed) {
					t.Errorf("concurrent sanitize returned %v", sanitizeErr)
					return
				}
			}
		}()
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	workers.Wait()
}

func assertExposureReason(t *testing.T, err error, reason Reason) {
	t.Helper()
	var exposure *ExposureError
	if !errors.As(err, &exposure) || exposure.Code != ErrorCode || exposure.Reason != reason ||
		strings.Contains(exposure.Error(), "secret-one") {
		t.Fatalf("expected exposure reason %q, got %T %v", reason, err, err)
	}
}

func BenchmarkGuardSanitizeString1MiB(b *testing.B) {
	guard, err := New([]Secret{{Value: []byte("benchmark-secret")}})
	if err != nil {
		b.Fatal(err)
	}
	defer guard.Close()
	value := strings.Repeat("a", 1<<20) + "benchmark-secret"
	b.ReportAllocs()
	b.SetBytes(int64(len(value)))
	for index := 0; index < b.N; index++ {
		if _, _, err := guard.SanitizeString(value); err != nil {
			b.Fatal(fmt.Errorf("sanitize benchmark: %w", err))
		}
	}
}

func BenchmarkGuardSanitizeStringSafe1MiB(b *testing.B) {
	guard, err := New([]Secret{{Value: []byte("benchmark-secret")}})
	if err != nil {
		b.Fatal(err)
	}
	defer guard.Close()
	value := strings.Repeat("a", 1<<20)
	b.ReportAllocs()
	b.SetBytes(int64(len(value)))
	for index := 0; index < b.N; index++ {
		if _, _, err := guard.SanitizeString(value); err != nil {
			b.Fatal(fmt.Errorf("sanitize safe benchmark: %w", err))
		}
	}
}
