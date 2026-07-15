package secretguard

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"unicode/utf8"
)

type patternBuilder struct {
	limits   Limits
	patterns [][]byte
	byDigest map[[sha256.Size]byte][]int
	total    int
}

func newPatternBuilder(limits Limits) *patternBuilder {
	return &patternBuilder{limits: limits, byDigest: make(map[[sha256.Size]byte][]int)}
}

func (b *patternBuilder) addSecret(secret Secret) error {
	if len(secret.Value) < b.limits.MinimumSecretBytes {
		return newExposureError(ReasonSecretTooShort)
	}
	if err := b.addVariantSet(secret.Value); err != nil {
		return err
	}
	if len(secret.BasicAuthUsername) == 0 {
		return nil
	}
	if bytes.IndexByte(secret.BasicAuthUsername, ':') >= 0 {
		return newExposureError(ReasonInvalidBasicAuth)
	}
	combined := make([]byte, 0, len(secret.BasicAuthUsername)+1+len(secret.Value))
	combined = append(combined, secret.BasicAuthUsername...)
	combined = append(combined, ':')
	combined = append(combined, secret.Value...)
	defer zero(combined)
	if err := b.addVariantSet(combined); err != nil {
		return err
	}
	encoded := encodeBase64(base64.StdEncoding, combined)
	defer zero(encoded)
	for _, prefix := range [][]byte{[]byte("Basic "), []byte("basic ")} {
		candidate := make([]byte, 0, len(prefix)+len(encoded))
		candidate = append(candidate, prefix...)
		candidate = append(candidate, encoded...)
		if err := b.add(candidate); err != nil {
			zero(candidate)
			return err
		}
		zero(candidate)
	}
	return nil
}

func (b *patternBuilder) addVariantSet(value []byte) error {
	variants := [][]byte{
		append([]byte(nil), value...),
		jsonEscape(value),
		jsonUnicodeEscape(value, false),
		jsonUnicodeEscape(value, true),
		percentEncode(value, true, true),
		percentEncode(value, true, false),
		percentEncode(value, false, true),
		percentEncode(value, false, false),
		encodeBase64(base64.StdEncoding, value),
		encodeBase64(base64.RawStdEncoding, value),
		encodeBase64(base64.URLEncoding, value),
		encodeBase64(base64.RawURLEncoding, value),
	}
	for _, candidate := range variants {
		if err := b.add(candidate); err != nil {
			for _, remaining := range variants {
				zero(remaining)
			}
			return err
		}
	}
	for _, candidate := range variants {
		zero(candidate)
	}
	return nil
}

func jsonUnicodeEscape(value []byte, uppercase bool) []byte {
	if !utf8.Valid(value) {
		return nil
	}
	hex := "0123456789abcdef"
	if uppercase {
		hex = "0123456789ABCDEF"
	}
	result := make([]byte, 0, len(value)*6)
	for len(value) > 0 {
		r, size := utf8.DecodeRune(value)
		value = value[size:]
		if r <= 0xffff {
			result = appendJSONUnicodeCodeUnit(result, uint16(r), hex)
			continue
		}
		offset := uint32(r) - 0x10000
		result = appendJSONUnicodeCodeUnit(result, uint16(0xd800+(offset>>10)), hex)
		result = appendJSONUnicodeCodeUnit(result, uint16(0xdc00+(offset&0x3ff)), hex)
	}
	return result
}

func appendJSONUnicodeCodeUnit(result []byte, value uint16, hex string) []byte {
	return append(
		result,
		'\\', 'u',
		hex[(value>>12)&0xf],
		hex[(value>>8)&0xf],
		hex[(value>>4)&0xf],
		hex[value&0xf],
	)
}

func (b *patternBuilder) add(candidate []byte) error {
	if len(candidate) == 0 {
		return nil
	}
	if len(candidate) > b.limits.MaximumPatternBytes {
		return newExposureError(ReasonPatternTooLong)
	}
	digest := sha256.Sum256(candidate)
	for _, index := range b.byDigest[digest] {
		if bytes.Equal(b.patterns[index], candidate) {
			return nil
		}
	}
	if len(b.patterns) >= b.limits.MaximumPatterns {
		return newExposureError(ReasonPatternLimit)
	}
	if b.total+len(candidate) > b.limits.MaximumTotalPatternBytes {
		return newExposureError(ReasonPatternBytesLimit)
	}
	cloned := append([]byte(nil), candidate...)
	index := len(b.patterns)
	b.patterns = append(b.patterns, cloned)
	b.byDigest[digest] = append(b.byDigest[digest], index)
	b.total += len(cloned)
	return nil
}

func (b *patternBuilder) close() {
	for _, pattern := range b.patterns {
		zero(pattern)
	}
	b.patterns = nil
	b.byDigest = nil
	b.total = 0
}

func encodeBase64(encoding *base64.Encoding, value []byte) []byte {
	result := make([]byte, encoding.EncodedLen(len(value)))
	encoding.Encode(result, value)
	return result
}

func percentEncode(value []byte, encodeAll, uppercase bool) []byte {
	hex := "0123456789ABCDEF"
	if !uppercase {
		hex = "0123456789abcdef"
	}
	result := make([]byte, 0, len(value)*3)
	for _, current := range value {
		if !encodeAll && isURLUnreserved(current) {
			result = append(result, current)
			continue
		}
		result = append(result, '%', hex[current>>4], hex[current&0x0f])
	}
	return result
}

func isURLUnreserved(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' || value == '-' || value == '.' || value == '_' || value == '~'
}

func jsonEscape(value []byte) []byte {
	hex := "0123456789abcdef"
	result := make([]byte, 0, len(value))
	for index := 0; index < len(value); index++ {
		current := value[index]
		switch current {
		case '\\', '"':
			result = append(result, '\\', current)
		case '\b':
			result = append(result, '\\', 'b')
		case '\f':
			result = append(result, '\\', 'f')
		case '\n':
			result = append(result, '\\', 'n')
		case '\r':
			result = append(result, '\\', 'r')
		case '\t':
			result = append(result, '\\', 't')
		case '<', '>', '&':
			result = append(result, '\\', 'u', '0', '0', hex[current>>4], hex[current&0x0f])
		default:
			if current < 0x20 {
				result = append(result, '\\', 'u', '0', '0', hex[current>>4], hex[current&0x0f])
			} else if index+2 < len(value) && current == 0xe2 && value[index+1] == 0x80 &&
				(value[index+2] == 0xa8 || value[index+2] == 0xa9) {
				result = append(result, '\\', 'u', '2', '0', '2', hex[value[index+2]&0x0f])
				index += 2
			} else {
				result = append(result, current)
			}
		}
	}
	return result
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
