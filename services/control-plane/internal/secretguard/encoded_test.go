package secretguard

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestGuardSanitizesMixedEncodedRepresentations(t *testing.T) {
	percentSecret := []byte("mix A+/ space-token")
	unicodeSecret := []byte("unicode-secret-🙂-token")
	tests := []struct {
		name    string
		secret  []byte
		variant []byte
	}{
		{name: "partial mixed-case percent and QueryEscape plus", secret: percentSecret, variant: mixedPercentQuery(percentSecret)},
		{name: "partial mixed-case JSON unicode", secret: unicodeSecret, variant: mixedJSONUnicode(unicodeSecret)},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			guard, err := New([]Secret{{Value: testCase.secret}})
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			input := "before " + string(testCase.variant) + " after"
			sanitized, changed, err := guard.SanitizeString(input)
			if err != nil {
				t.Fatal(err)
			}
			if !changed || sanitized != "before "+RedactionMarker+" after" {
				t.Fatalf("SanitizeString() = %q, changed=%t", sanitized, changed)
			}
			if _, _, err := guard.Sanitize([]byte(input)); !IsExposure(err) {
				t.Fatalf("binary encoded representation error = %T %v", err, err)
			}
			if _, _, err := guard.Sanitize(map[string]any{input: "value"}); !IsExposure(err) {
				t.Fatalf("encoded map key error = %T %v", err, err)
			}
		})
	}
}

func TestEncodedRepresentationsAreStatefulAcrossEveryStreamBoundary(t *testing.T) {
	secret := []byte("stream A+/ encoded-secret")
	variants := map[string][]byte{
		"percent": mixedPercentQuery(secret),
		"json":    mixedJSONUnicode(secret),
	}
	for name, variant := range variants {
		t.Run(name, func(t *testing.T) {
			for split := 0; split <= len(variant); split++ {
				guard, err := New([]Secret{{Value: secret}})
				if err != nil {
					t.Fatal(err)
				}
				stream, err := guard.NewStream(StreamText)
				if err != nil {
					t.Fatal(err)
				}
				first, firstErr := stream.Transform(variant[:split])
				second, secondErr := stream.Transform(variant[split:])
				final, finishErr := stream.Finish()
				_ = stream.Close()
				_ = guard.Close()
				if firstErr != nil || secondErr != nil || finishErr != nil {
					t.Fatalf("split %d errors: %v, %v, %v", split, firstErr, secondErr, finishErr)
				}
				output := append(append(first, second...), final...)
				if string(output) != RedactionMarker || bytes.Contains(output, secret) {
					t.Fatalf("split %d output = %q", split, output)
				}
			}
		})
	}
}

func TestBinaryStreamBlocksMixedEncodedRepresentation(t *testing.T) {
	secret := []byte("binary encoded-secret")
	variant := mixedPercentQuery(secret)
	guard, err := New([]Secret{{Value: secret}})
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	stream, err := guard.NewStream(StreamBinaryDetectOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for _, current := range variant {
		if _, err := stream.Transform([]byte{current}); err != nil {
			if !IsExposure(err) {
				t.Fatalf("binary encoded stream error = %T %v", err, err)
			}
			return
		}
	}
	if _, err := stream.Finish(); !IsExposure(err) {
		t.Fatalf("binary encoded Finish() error = %T %v", err, err)
	}
}

func mixedPercentQuery(value []byte) []byte {
	hexUpper := "0123456789ABCDEF"
	hexLower := "0123456789abcdef"
	result := make([]byte, 0, len(value)*2)
	for index, current := range value {
		if current == ' ' {
			result = append(result, '+')
			continue
		}
		if current == '+' || index%2 == 0 {
			highHex := hexUpper
			lowHex := hexLower
			if index%4 >= 2 {
				highHex, lowHex = lowHex, highHex
			}
			result = append(result, '%', highHex[current>>4], lowHex[current&0xf])
			continue
		}
		result = append(result, current)
	}
	return result
}

func mixedJSONUnicode(value []byte) []byte {
	if !utf8.Valid(value) {
		return nil
	}
	var result strings.Builder
	runeIndex := 0
	for len(value) > 0 {
		r, size := utf8.DecodeRune(value)
		value = value[size:]
		if runeIndex%3 == 0 {
			result.WriteRune(r)
			runeIndex++
			continue
		}
		if r <= 0xffff {
			writeMixedJSONCodeUnit(&result, uint16(r), runeIndex)
		} else {
			offset := uint32(r) - 0x10000
			writeMixedJSONCodeUnit(&result, uint16(0xd800+(offset>>10)), runeIndex)
			writeMixedJSONCodeUnit(&result, uint16(0xdc00+(offset&0x3ff)), runeIndex+1)
		}
		runeIndex++
	}
	return []byte(result.String())
}

func writeMixedJSONCodeUnit(result *strings.Builder, value uint16, index int) {
	encoded := fmt.Sprintf("%04x", value)
	if index%2 == 0 {
		encoded = strings.ToUpper(encoded[:2]) + encoded[2:]
	} else {
		encoded = encoded[:2] + strings.ToUpper(encoded[2:])
	}
	result.WriteString("\\u")
	result.WriteString(encoded)
}
