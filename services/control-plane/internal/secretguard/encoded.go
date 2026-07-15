package secretguard

import "unicode/utf8"

type sourceSpan struct {
	start uint32
	end   uint32
}

func detectRepresentations(m *matcher, value []byte) bool {
	rawMatch, encodedHint := m.detectWithRepresentationHints(value)
	if rawMatch {
		return true
	}
	if !encodedHint {
		return false
	}
	if decoded, spans, changed := decodePercentQuery(value); changed {
		found := m.detect(decoded)
		zero(decoded)
		clear(spans)
		if found {
			return true
		}
	}
	if decoded, spans, changed := decodeJSONEscapes(value); changed {
		found := m.detect(decoded)
		zero(decoded)
		clear(spans)
		return found
	}
	return false
}

func representationMatches(m *matcher, value []byte) ([]uint32, bool) {
	rawMatch, encodedHint := m.detectWithRepresentationHints(value)
	var best []uint32
	if rawMatch {
		best = m.matches(value)
	}
	if !encodedHint {
		return best, rawMatch
	}
	if decoded, spans, changed := decodePercentQuery(value); changed {
		if m.detect(decoded) {
			if best == nil {
				best = make([]uint32, len(value))
			}
			mergeDecodedMatches(best, m.matches(decoded), spans)
			rawMatch = true
		}
		zero(decoded)
		clear(spans)
	}
	if decoded, spans, changed := decodeJSONEscapes(value); changed {
		if m.detect(decoded) {
			if best == nil {
				best = make([]uint32, len(value))
			}
			mergeDecodedMatches(best, m.matches(decoded), spans)
			rawMatch = true
		}
		zero(decoded)
		clear(spans)
	}
	return best, rawMatch
}

func mergeDecodedMatches(target, candidate []uint32, spans []sourceSpan) {
	for decodedStart, decodedLength := range candidate {
		if decodedLength == 0 {
			continue
		}
		decodedEnd := decodedStart + int(decodedLength) - 1
		if decodedStart >= len(spans) || decodedEnd >= len(spans) {
			continue
		}
		sourceStart := int(spans[decodedStart].start)
		sourceEnd := int(spans[decodedEnd].end)
		if sourceStart < 0 || sourceStart >= len(target) || sourceEnd <= sourceStart {
			continue
		}
		length := uint32(sourceEnd - sourceStart)
		if length > target[sourceStart] {
			target[sourceStart] = length
		}
	}
}

func decodePercentQuery(value []byte) ([]byte, []sourceSpan, bool) {
	if uint64(len(value)) > uint64(^uint32(0)) {
		return nil, nil, false
	}
	decoded := make([]byte, 0, len(value))
	spans := make([]sourceSpan, 0, len(value))
	changed := false
	for index := 0; index < len(value); {
		start := index
		if value[index] == '%' && index+2 < len(value) {
			high, highOK := decodeHex(value[index+1])
			low, lowOK := decodeHex(value[index+2])
			if highOK && lowOK {
				decoded = append(decoded, high<<4|low)
				spans = append(spans, sourceSpan{start: uint32(start), end: uint32(index + 3)})
				index += 3
				changed = true
				continue
			}
		}
		current := value[index]
		if current == '+' {
			current = ' '
			changed = true
		}
		decoded = append(decoded, current)
		spans = append(spans, sourceSpan{start: uint32(start), end: uint32(index + 1)})
		index++
	}
	if !changed {
		zero(decoded)
		return nil, nil, false
	}
	return decoded, spans, true
}

func decodeJSONEscapes(value []byte) ([]byte, []sourceSpan, bool) {
	if uint64(len(value)) > uint64(^uint32(0)) {
		return nil, nil, false
	}
	decoded := make([]byte, 0, len(value))
	spans := make([]sourceSpan, 0, len(value))
	changed := false
	for index := 0; index < len(value); {
		start := index
		if value[index] != '\\' || index+1 >= len(value) {
			decoded = append(decoded, value[index])
			spans = append(spans, sourceSpan{start: uint32(start), end: uint32(index + 1)})
			index++
			continue
		}
		switch value[index+1] {
		case '"', '\\', '/':
			decoded = append(decoded, value[index+1])
			spans = append(spans, sourceSpan{start: uint32(start), end: uint32(index + 2)})
			index += 2
			changed = true
			continue
		case 'b', 'f', 'n', 'r', 't':
			decoded = append(decoded, decodedJSONControl(value[index+1]))
			spans = append(spans, sourceSpan{start: uint32(start), end: uint32(index + 2)})
			index += 2
			changed = true
			continue
		case 'u':
			codeUnit, ok := decodeJSONCodeUnit(value, index)
			if !ok {
				break
			}
			end := index + 6
			r := rune(codeUnit)
			if codeUnit >= 0xd800 && codeUnit <= 0xdbff {
				low, lowOK := decodeJSONCodeUnit(value, end)
				if !lowOK || low < 0xdc00 || low > 0xdfff {
					break
				}
				r = 0x10000 + (rune(codeUnit-0xd800) << 10) + rune(low-0xdc00)
				end += 6
			} else if codeUnit >= 0xdc00 && codeUnit <= 0xdfff {
				break
			}
			var encoded [utf8.UTFMax]byte
			length := utf8.EncodeRune(encoded[:], r)
			for encodedIndex := 0; encodedIndex < length; encodedIndex++ {
				decoded = append(decoded, encoded[encodedIndex])
				spans = append(spans, sourceSpan{start: uint32(start), end: uint32(end)})
			}
			zero(encoded[:])
			index = end
			changed = true
			continue
		}
		decoded = append(decoded, value[index])
		spans = append(spans, sourceSpan{start: uint32(start), end: uint32(index + 1)})
		index++
	}
	if !changed {
		zero(decoded)
		return nil, nil, false
	}
	return decoded, spans, true
}

func decodeJSONCodeUnit(value []byte, start int) (uint16, bool) {
	if start+6 > len(value) || value[start] != '\\' || value[start+1] != 'u' {
		return 0, false
	}
	var result uint16
	for index := start + 2; index < start+6; index++ {
		decoded, ok := decodeHex(value[index])
		if !ok {
			return 0, false
		}
		result = result<<4 | uint16(decoded)
	}
	return result, true
}

func decodeHex(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10, true
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10, true
	default:
		return 0, false
	}
}

func decodedJSONControl(value byte) byte {
	switch value {
	case 'b':
		return '\b'
	case 'f':
		return '\f'
	case 'n':
		return '\n'
	case 'r':
		return '\r'
	default:
		return '\t'
	}
}
