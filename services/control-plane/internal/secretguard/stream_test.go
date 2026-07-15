package secretguard

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTextStreamRedactsAcrossEverySplitPoint(t *testing.T) {
	secret := "streaming-secret"
	input := []byte("prefix 🙂 " + secret + " suffix 🚀")
	expected := "prefix 🙂 " + RedactionMarker + " suffix 🚀"
	for split := 0; split <= len(input); split++ {
		t.Run(stringSplitName(split), func(t *testing.T) {
			guard, err := New([]Secret{{Value: []byte(secret)}})
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			stream, err := guard.NewStream(StreamText)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			var output []byte
			for _, chunk := range [][]byte{input[:split], input[split:]} {
				transformed, transformErr := stream.Transform(chunk)
				if transformErr != nil {
					t.Fatal(transformErr)
				}
				output = append(output, transformed...)
			}
			final, err := stream.Finish()
			if err != nil {
				t.Fatal(err)
			}
			output = append(output, final...)
			if !utf8.Valid(output) || string(output) != expected || stream.Redactions() != 1 {
				t.Fatalf("split %d produced %q with %d redactions", split, output, stream.Redactions())
			}
		})
	}
}

func TestTextStreamRedactsEncodedVariantAcrossEverySplitPoint(t *testing.T) {
	secret := []byte("encoded+/ secret")
	variant := percentEncode(secret, true, false)
	input := append(append([]byte("before "), variant...), []byte(" after")...)
	for split := 0; split <= len(input); split++ {
		guard, err := New([]Secret{{Value: secret}})
		if err != nil {
			t.Fatal(err)
		}
		stream, err := guard.NewStream(StreamText)
		if err != nil {
			t.Fatal(err)
		}
		output := transformChunks(t, stream, input[:split], input[split:])
		if string(output) != "before "+RedactionMarker+" after" {
			t.Fatalf("split %d leaked encoded variant: %q", split, output)
		}
		stream.Close()
		guard.Close()
	}
	zero(variant)
}

func TestBinaryStreamDetectsAcrossEverySplitPoint(t *testing.T) {
	secret := []byte("binary-secret")
	input := append(append([]byte("prefix-"), secret...), []byte("-suffix")...)
	for split := 0; split <= len(input); split++ {
		guard, err := New([]Secret{{Value: secret}})
		if err != nil {
			t.Fatal(err)
		}
		stream, err := guard.NewStream(StreamBinaryDetectOnly)
		if err != nil {
			t.Fatal(err)
		}
		var persisted []byte
		var detected error
		for _, chunk := range [][]byte{input[:split], input[split:]} {
			output, transformErr := stream.Transform(chunk)
			if transformErr != nil {
				detected = transformErr
				break
			}
			persisted = append(persisted, output...)
		}
		if detected == nil {
			_, detected = stream.Finish()
		}
		assertExposureReason(t, detected, ReasonBinaryMatch)
		if bytes.Contains(persisted, secret) {
			t.Fatalf("split %d returned unsafe binary bytes", split)
		}
		stream.Close()
		guard.Close()
	}
}

func TestBinaryStreamReturnsUnchangedSafeContent(t *testing.T) {
	guard, err := New([]Secret{{Value: []byte("binary-secret")}})
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	stream, err := guard.NewStream(StreamBinaryDetectOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	input := []byte{0, 1, 2, 3, 4, 0xff, 0xfe}
	output := transformChunks(t, stream, input[:3], input[3:])
	if !bytes.Equal(output, input) {
		t.Fatalf("safe binary content changed: %v", output)
	}
}

func TestTextStreamHandlesOperationalBoundaries(t *testing.T) {
	for _, boundary := range []int{8 << 10, 32 << 10, 1 << 20} {
		t.Run(stringSplitName(boundary), func(t *testing.T) {
			secret := []byte("boundary-secret")
			prefix := bytes.Repeat([]byte{'a'}, boundary-4)
			input := append(prefix, secret...)
			input = append(input, []byte("-tail")...)
			guard, err := New([]Secret{{Value: secret}})
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			stream, err := guard.NewStream(StreamText)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			output := transformChunks(t, stream, input[:boundary], input[boundary:])
			expected := string(prefix) + RedactionMarker + "-tail"
			if string(output) != expected || bytes.Contains(output, secret) {
				t.Fatalf("boundary %d was not safely redacted", boundary)
			}
		})
	}
}

func TestTextStreamFailsClosedForInvalidUTF8(t *testing.T) {
	guard, err := New([]Secret{{Value: []byte("valid-secret")}})
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	stream, err := guard.NewStream(StreamText)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := stream.Transform([]byte{0xff}); err != nil && !IsExposure(err) {
		t.Fatal(err)
	}
	_, err = stream.Finish()
	assertExposureReason(t, err, ReasonInvalidText)
}

func TestStreamUsesLeftmostLongestAcrossChunks(t *testing.T) {
	guard, err := New([]Secret{{Value: []byte("abcdefgh")}, {Value: []byte("abcdefghijk")}})
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	stream, err := guard.NewStream(StreamText)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	output := transformChunks(t, stream, []byte("abcde"), []byte("fghijk"))
	if string(output) != RedactionMarker || stream.Redactions() != 1 {
		t.Fatalf("stream did not choose the longest same-start match: %q", output)
	}
}

func transformChunks(t *testing.T, stream *Stream, chunks ...[]byte) []byte {
	t.Helper()
	var output []byte
	for _, chunk := range chunks {
		transformed, err := stream.Transform(chunk)
		if err != nil {
			t.Fatal(err)
		}
		output = append(output, transformed...)
	}
	final, err := stream.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return append(output, final...)
}

func stringSplitName(value int) string {
	return strings.ReplaceAll(strings.TrimSpace(strings.Repeat("0", 0)+fmtInt(value)), " ", "-")
}

func fmtInt(value int) string {
	if value == 0 {
		return "0"
	}
	var buffer [32]byte
	position := len(buffer)
	for value > 0 {
		position--
		buffer[position] = byte('0' + value%10)
		value /= 10
	}
	return string(buffer[position:])
}

func TestInvalidStreamModeFailsClosed(t *testing.T) {
	guard, err := New([]Secret{{Value: []byte("stream-secret")}})
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	_, err = guard.NewStream(StreamMode("unknown"))
	assertExposureReason(t, err, ReasonInvalidMode)
}

func TestFinishedStreamCannotBeReused(t *testing.T) {
	guard, err := New([]Secret{{Value: []byte("stream-secret")}})
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	stream, err := guard.NewStream(StreamText)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	transformChunks(t, stream, []byte("safe"))
	if _, err := stream.Transform([]byte("more")); !errors.Is(err, ErrClosed) {
		t.Fatalf("finished stream returned %v", err)
	}
}

func BenchmarkTextStream1MiB(b *testing.B) {
	guard, err := New([]Secret{{Value: []byte("benchmark-secret")}})
	if err != nil {
		b.Fatal(err)
	}
	defer guard.Close()
	input := []byte(strings.Repeat("a", 1<<20) + "benchmark-secret")
	b.ReportAllocs()
	b.SetBytes(int64(len(input)))
	for index := 0; index < b.N; index++ {
		stream, streamErr := guard.NewStream(StreamText)
		if streamErr != nil {
			b.Fatal(streamErr)
		}
		if _, streamErr = stream.Transform(input); streamErr != nil {
			b.Fatal(streamErr)
		}
		if _, streamErr = stream.Finish(); streamErr != nil {
			b.Fatal(streamErr)
		}
		stream.Close()
	}
}

func BenchmarkTextStreamSafe1MiB(b *testing.B) {
	guard, err := New([]Secret{{Value: []byte("benchmark-secret")}})
	if err != nil {
		b.Fatal(err)
	}
	defer guard.Close()
	input := []byte(strings.Repeat("a", 1<<20))
	b.ReportAllocs()
	b.SetBytes(int64(len(input)))
	for index := 0; index < b.N; index++ {
		stream, streamErr := guard.NewStream(StreamText)
		if streamErr != nil {
			b.Fatal(streamErr)
		}
		if _, streamErr = stream.Transform(input); streamErr != nil {
			b.Fatal(streamErr)
		}
		if _, streamErr = stream.Finish(); streamErr != nil {
			b.Fatal(streamErr)
		}
		stream.Close()
	}
}
