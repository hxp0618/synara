package agentd

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/artifacts"
	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/secretguard"
)

var terminalCollectorTestTime = time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)

type terminalCollectorTestUpload struct {
	artifact RunnerArtifact
	body     []byte
	result   artifacts.Artifact
}

type terminalCollectorTestClient struct {
	events  []RunnerMessage
	uploads []terminalCollectorTestUpload
}

func (c *terminalCollectorTestClient) AppendEvent(
	_ context.Context,
	_ uuid.UUID,
	_ executions.Lease,
	message RunnerMessage,
) error {
	c.events = append(c.events, message)
	return nil
}

func (c *terminalCollectorTestClient) UploadArtifact(
	_ context.Context,
	_ uuid.UUID,
	_ executions.Lease,
	artifact RunnerArtifact,
	absolutePath string,
) (artifacts.Artifact, error) {
	body, err := os.ReadFile(absolutePath)
	if err != nil {
		return artifacts.Artifact{}, err
	}
	artifactID := uuid.NewSHA1(
		uuid.NameSpaceURL,
		[]byte(strings.Join([]string{artifact.Path, sha256Hex(body)}, "\x00")),
	)
	result := artifacts.Artifact{ID: artifactID}
	c.uploads = append(c.uploads, terminalCollectorTestUpload{
		artifact: artifact,
		body:     append([]byte(nil), body...),
		result:   result,
	})
	return result, nil
}

func TestTerminalLogCollectorSmallOutputUsesPreviewWithoutArtifact(t *testing.T) {
	client, collector := newTerminalCollectorForTest(t, uuid.New(), executions.Lease{Generation: 7})
	terminalID := "terminal-small"
	payload := bytes.Repeat([]byte("a"), int(terminalLogPreviewBytes))

	terminalCollectorMustHandle(t, collector, terminalCollectorLifecycleMessage(terminalID, "terminal.started"))
	terminalCollectorMustHandle(t, collector, terminalCollectorOutputMessage(terminalID, "utf-8", 0, payload))
	terminalCollectorMustHandle(t, collector, terminalCollectorLifecycleMessage(terminalID, "terminal.exited"))

	if len(client.uploads) != 0 {
		t.Fatalf("expected no Artifact upload for %d-byte output, got %d", len(payload), len(client.uploads))
	}
	previews := terminalCollectorPreviewEvents(client.events, terminalID)
	if len(previews) != 1 {
		t.Fatalf("expected one preview event, got %d", len(previews))
	}
	if delta, _ := previews[0].Payload["delta"].(string); delta != string(payload) {
		t.Fatalf("preview mismatch: got %d bytes, want %d", len(delta), len(payload))
	}
	if got := terminalCollectorInt64(t, previews[0].Payload, "byteOffset"); got != 0 {
		t.Fatalf("preview byteOffset = %d, want 0", got)
	}
	if got := terminalCollectorInt64(t, previews[0].Payload, "byteLength"); got != int64(len(payload)) {
		t.Fatalf("preview byteLength = %d, want %d", got, len(payload))
	}
	if truncated, _ := previews[0].Payload["truncated"].(bool); truncated {
		t.Fatal("preview unexpectedly marked truncated")
	}

	completion := terminalCollectorSingleTerminalEvent(t, client.events, terminalID, "terminal.exited")
	terminal := terminalCollectorTerminalPayloadRequired(t, completion)
	terminalCollectorRequireInt64(t, terminal, "totalBytes", int64(len(payload)))
	terminalCollectorRequireInt64(t, terminal, "previewBytes", int64(len(payload)))
	terminalCollectorRequireInt64(t, terminal, "segmentCount", 0)
	if truncated, _ := terminal["truncated"].(bool); truncated {
		t.Fatal("completion unexpectedly marked truncated")
	}
}

func TestTerminalLogCollectorLargeOutputUploadsCompleteArtifactFromZero(t *testing.T) {
	client, collector := newTerminalCollectorForTest(t, uuid.New(), executions.Lease{Generation: 8})
	terminalID := "terminal-large"
	first := bytes.Repeat([]byte("a"), 24<<10)
	second := bytes.Repeat([]byte("b"), 12<<10+17)
	want := append(append([]byte(nil), first...), second...)

	terminalCollectorMustHandle(t, collector, terminalCollectorLifecycleMessage(terminalID, "terminal.started"))
	terminalCollectorMustHandle(t, collector, terminalCollectorOutputMessage(terminalID, "utf-8", 0, first))
	terminalCollectorMustHandle(t, collector, terminalCollectorOutputMessage(
		terminalID,
		"utf-8",
		int64(len(first)),
		second,
	))
	terminalCollectorMustHandle(t, collector, terminalCollectorLifecycleMessage(terminalID, "terminal.exited"))

	if len(client.uploads) != 1 {
		t.Fatalf("expected one Artifact upload, got %d", len(client.uploads))
	}
	upload := client.uploads[0]
	if !bytes.Equal(upload.body, want) {
		t.Fatalf("Artifact body mismatch: got %d bytes, want %d", len(upload.body), len(want))
	}
	if upload.artifact.Kind != "terminal_log" {
		t.Fatalf("Artifact kind = %q, want terminal_log", upload.artifact.Kind)
	}
	if upload.artifact.ContentType != "text/plain; charset=utf-8" {
		t.Fatalf("Artifact content type = %q, want UTF-8 text", upload.artifact.ContentType)
	}

	reference := terminalCollectorSingleTerminalEvent(t, client.events, terminalID, "terminal.output.reference")
	terminal := terminalCollectorTerminalPayloadRequired(t, reference)
	terminalCollectorRequireInt64(t, terminal, "offset", 0)
	terminalCollectorRequireInt64(t, terminal, "length", int64(len(want)))
	terminalCollectorRequireInt64(t, terminal, "segmentIndex", 0)
	if got, _ := terminal["artifactId"].(string); got != upload.result.ID.String() {
		t.Fatalf("reference artifactId = %q, want %q", got, upload.result.ID)
	}
	if got, _ := terminal["encoding"].(string); got != "utf-8" {
		t.Fatalf("reference encoding = %q, want utf-8", got)
	}

	previews := terminalCollectorPreviewEvents(client.events, terminalID)
	if len(previews) != 2 {
		t.Fatalf("expected two preview chunks, got %d", len(previews))
	}
	if got := terminalCollectorPreviewBytes(previews); !bytes.Equal(got, want[:terminalLogPreviewBytes]) {
		t.Fatalf("preview mismatch: got %d bytes, want %d", len(got), terminalLogPreviewBytes)
	}
	if truncated, _ := previews[len(previews)-1].Payload["truncated"].(bool); !truncated {
		t.Fatal("final preview chunk was not marked truncated")
	}
}

func TestTerminalLogCollectorIngestsBoundRuntimeOutputFromByteZero(t *testing.T) {
	client, collector := newTerminalCollectorForTest(t, uuid.New(), executions.Lease{Generation: 18})
	terminalID := "terminal-runtime-output"
	payload := bytes.Repeat([]byte("safe UTF-8 output 你好\n"), 4_096)
	path := filepath.Join(t.TempDir(), "command.log")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := openRegularArtifactSource(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	reportedSize := int64(len(payload))

	terminalCollectorMustHandle(t, collector, terminalCollectorLifecycleMessage(terminalID, "terminal.started"))
	if err := collector.HandleArtifactSource(context.Background(), RunnerArtifact{
		Path: "projects/session/tool-results/command.log", Kind: "terminal_log",
		ContentType: "text/plain; charset=utf-8", SourceRoot: "runtime-output",
		TerminalID: terminalID, Encoding: "utf-8", ReportedSize: &reportedSize,
	}, source, &terminalCollectorTestTime); err != nil {
		t.Fatal(err)
	}
	terminalCollectorMustHandle(t, collector, terminalCollectorLifecycleMessage(terminalID, "terminal.exited"))

	if len(client.uploads) != 1 {
		t.Fatalf("expected one Runtime Output Artifact, got %d", len(client.uploads))
	}
	if !bytes.Equal(client.uploads[0].body, payload) {
		t.Fatalf("Runtime Output Artifact body mismatch: got %d bytes, want %d", len(client.uploads[0].body), len(payload))
	}
	if client.uploads[0].artifact.ContentType != "text/plain; charset=utf-8" {
		t.Fatalf("Runtime Output Artifact content type = %q", client.uploads[0].artifact.ContentType)
	}
	previews := terminalCollectorPreviewEvents(client.events, terminalID)
	if got := terminalCollectorPreviewBytes(previews); !bytes.Equal(got, payload[:terminalLogPreviewBytes]) {
		t.Fatalf("Runtime Output preview mismatch: got %d bytes, want %d", len(got), terminalLogPreviewBytes)
	}
	completion := terminalCollectorSingleTerminalEvent(t, client.events, terminalID, "terminal.exited")
	terminal := terminalCollectorTerminalPayloadRequired(t, completion)
	terminalCollectorRequireInt64(t, terminal, "totalBytes", int64(len(payload)))
	terminalCollectorRequireInt64(t, terminal, "previewBytes", terminalLogPreviewBytes)
	terminalCollectorRequireInt64(t, terminal, "segmentCount", 1)
}

func TestTerminalLogCollectorKeepsUnsafeRuntimeOutputArtifactOnly(t *testing.T) {
	client, collector := newTerminalCollectorForTest(t, uuid.New(), executions.Lease{Generation: 19})
	terminalID := "terminal-runtime-binary"
	payload := append([]byte("safe prefix"), 0, 0xff, 0xfe)
	path := filepath.Join(t.TempDir(), "command.bin")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := openRegularArtifactSource(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	reportedSize := int64(len(payload))

	terminalCollectorMustHandle(t, collector, terminalCollectorLifecycleMessage(terminalID, "terminal.started"))
	if err := collector.HandleArtifactSource(context.Background(), RunnerArtifact{
		Path: "projects/session/tool-results/command.bin", Kind: "terminal_log",
		ContentType: "text/plain; charset=utf-8", SourceRoot: "runtime-output",
		TerminalID: terminalID, Encoding: "utf-8", ReportedSize: &reportedSize,
	}, source, &terminalCollectorTestTime); err != nil {
		t.Fatal(err)
	}
	terminalCollectorMustHandle(t, collector, terminalCollectorLifecycleMessage(terminalID, "terminal.exited"))

	if len(terminalCollectorPreviewEvents(client.events, terminalID)) != 0 {
		t.Fatal("unsafe Runtime Output leaked into Session Event preview")
	}
	if len(client.uploads) != 1 || !bytes.Equal(client.uploads[0].body, payload) {
		t.Fatalf("unsafe Runtime Output Artifact mismatch: %#v", client.uploads)
	}
	if client.uploads[0].artifact.ContentType != "application/octet-stream" {
		t.Fatalf("unsafe Runtime Output content type = %q, want application/octet-stream", client.uploads[0].artifact.ContentType)
	}
	reference := terminalCollectorSingleTerminalEvent(t, client.events, terminalID, "terminal.output.reference")
	if got, _ := terminalCollectorTerminalPayloadRequired(t, reference)["encoding"].(string); got != "binary" {
		t.Fatalf("unsafe Runtime Output reference encoding = %q, want binary", got)
	}
}

func TestTerminalLogCollectorRejectsRuntimeOutputAfterInlineDelta(t *testing.T) {
	_, collector := newTerminalCollectorForTest(t, uuid.New(), executions.Lease{Generation: 20})
	terminalID := "terminal-runtime-duplicate"
	path := filepath.Join(t.TempDir(), "command.log")
	if err := os.WriteFile(path, []byte("full output"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := openRegularArtifactSource(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()

	terminalCollectorMustHandle(t, collector, terminalCollectorLifecycleMessage(terminalID, "terminal.started"))
	terminalCollectorMustHandle(t, collector, terminalCollectorOutputMessage(terminalID, "utf-8", 0, []byte("inline")))
	if err := collector.HandleArtifactSource(context.Background(), RunnerArtifact{
		Path: "projects/session/tool-results/command.log", Kind: "terminal_log",
		ContentType: "text/plain; charset=utf-8", SourceRoot: "runtime-output",
		TerminalID: terminalID, Encoding: "utf-8",
	}, source, &terminalCollectorTestTime); err == nil {
		t.Fatal("Runtime Output Artifact was accepted after inline command output")
	}
}

func TestTerminalLogCollectorSegmentsLargeOutputWithContiguousOffsets(t *testing.T) {
	client, collector := newTerminalCollectorForTest(t, uuid.New(), executions.Lease{Generation: 9})
	terminalID := "terminal-segmented"
	payload := make([]byte, 2*terminalLogSegmentBytes+257)
	for index := range payload {
		payload[index] = byte('a' + index%26)
	}

	terminalCollectorMustHandle(t, collector, terminalCollectorLifecycleMessage(terminalID, "terminal.started"))
	terminalCollectorMustHandle(t, collector, terminalCollectorOutputMessage(terminalID, "utf-8", 0, payload))
	terminalCollectorMustHandle(t, collector, terminalCollectorLifecycleMessage(terminalID, "terminal.exited"))

	if len(client.uploads) != 3 {
		t.Fatalf("expected three Artifact segments, got %d", len(client.uploads))
	}
	references := terminalCollectorTerminalEvents(client.events, terminalID, "terminal.output.reference")
	if len(references) != len(client.uploads) {
		t.Fatalf("reference count = %d, want %d", len(references), len(client.uploads))
	}

	var offset int64
	for index, upload := range client.uploads {
		wantLength := min(terminalLogSegmentBytes, int64(len(payload))-offset)
		wantBody := payload[offset : offset+wantLength]
		if !bytes.Equal(upload.body, wantBody) {
			t.Fatalf("segment %d body mismatch", index)
		}
		terminal := terminalCollectorTerminalPayloadRequired(t, references[index])
		terminalCollectorRequireInt64(t, terminal, "offset", offset)
		terminalCollectorRequireInt64(t, terminal, "length", wantLength)
		terminalCollectorRequireInt64(t, terminal, "segmentIndex", int64(index))
		if got, _ := terminal["artifactId"].(string); got != upload.result.ID.String() {
			t.Fatalf("segment %d artifactId = %q, want %q", index, got, upload.result.ID)
		}
		offset += wantLength
	}
	if offset != int64(len(payload)) {
		t.Fatalf("referenced byte range ended at %d, want %d", offset, len(payload))
	}

	completion := terminalCollectorSingleTerminalEvent(t, client.events, terminalID, "terminal.exited")
	terminal := terminalCollectorTerminalPayloadRequired(t, completion)
	terminalCollectorRequireInt64(t, terminal, "totalBytes", int64(len(payload)))
	terminalCollectorRequireInt64(t, terminal, "segmentCount", 3)
}

func TestTerminalLogCollectorKeepsInterleavedTerminalStateIndependent(t *testing.T) {
	client, collector := newTerminalCollectorForTest(t, uuid.New(), executions.Lease{Generation: 10})
	firstID := "terminal-a"
	secondID := "terminal-b"
	firstChunks := [][]byte{[]byte("alpha-"), []byte("omega")}
	secondChunks := [][]byte{[]byte("bravo-"), []byte("tail")}

	terminalCollectorMustHandle(t, collector, terminalCollectorLifecycleMessage(firstID, "terminal.started"))
	terminalCollectorMustHandle(t, collector, terminalCollectorLifecycleMessage(secondID, "terminal.started"))
	terminalCollectorMustHandle(t, collector, terminalCollectorOutputMessage(firstID, "utf-8", 0, firstChunks[0]))
	terminalCollectorMustHandle(t, collector, terminalCollectorOutputMessage(secondID, "utf-8", 0, secondChunks[0]))
	terminalCollectorMustHandle(t, collector, terminalCollectorOutputMessage(
		firstID,
		"utf-8",
		int64(len(firstChunks[0])),
		firstChunks[1],
	))
	terminalCollectorMustHandle(t, collector, terminalCollectorOutputMessage(
		secondID,
		"utf-8",
		int64(len(secondChunks[0])),
		secondChunks[1],
	))

	terminalCollectorMustHandle(t, collector, terminalCollectorLifecycleMessage(secondID, "terminal.exited"))
	if _, found := collector.states[secondID]; found {
		t.Fatalf("completed terminal %q still has collector state", secondID)
	}
	if _, found := collector.states[firstID]; !found {
		t.Fatalf("completing %q removed interleaved terminal %q", secondID, firstID)
	}
	terminalCollectorMustHandle(t, collector, terminalCollectorLifecycleMessage(firstID, "terminal.exited"))

	if len(client.uploads) != 0 {
		t.Fatalf("expected no Artifact uploads, got %d", len(client.uploads))
	}
	firstPreview := terminalCollectorPreviewBytes(terminalCollectorPreviewEvents(client.events, firstID))
	if want := bytes.Join(firstChunks, nil); !bytes.Equal(firstPreview, want) {
		t.Fatalf("first terminal preview = %q, want %q", firstPreview, want)
	}
	secondPreview := terminalCollectorPreviewBytes(terminalCollectorPreviewEvents(client.events, secondID))
	if want := bytes.Join(secondChunks, nil); !bytes.Equal(secondPreview, want) {
		t.Fatalf("second terminal preview = %q, want %q", secondPreview, want)
	}

	firstEvents := terminalCollectorPreviewEvents(client.events, firstID)
	secondEvents := terminalCollectorPreviewEvents(client.events, secondID)
	terminalCollectorRequireInt64(t, firstEvents[1].Payload, "byteOffset", int64(len(firstChunks[0])))
	terminalCollectorRequireInt64(t, secondEvents[1].Payload, "byteOffset", int64(len(secondChunks[0])))
}

func TestTerminalLogCollectorBinaryOutputRequiresCanonicalBase64AndUsesArtifactOnly(t *testing.T) {
	t.Run("canonical", func(t *testing.T) {
		client, collector := newTerminalCollectorForTest(t, uuid.New(), executions.Lease{Generation: 11})
		terminalID := "terminal-binary"
		payload := []byte{0x00, 0x01, 0x02, 0x7f, 0x80, 0xff}

		terminalCollectorMustHandle(t, collector, terminalCollectorLifecycleMessage(terminalID, "terminal.started"))
		terminalCollectorMustHandle(t, collector, terminalCollectorOutputMessage(terminalID, "binary", 0, payload))
		terminalCollectorMustHandle(t, collector, terminalCollectorLifecycleMessage(terminalID, "terminal.exited"))

		if previews := terminalCollectorPreviewEvents(client.events, terminalID); len(previews) != 0 {
			t.Fatalf("binary output persisted %d content.delta preview events", len(previews))
		}
		if len(client.uploads) != 1 {
			t.Fatalf("expected one binary Artifact upload, got %d", len(client.uploads))
		}
		upload := client.uploads[0]
		if !bytes.Equal(upload.body, payload) {
			t.Fatalf("binary Artifact body = %v, want %v", upload.body, payload)
		}
		if upload.artifact.ContentType != "application/octet-stream" {
			t.Fatalf("binary Artifact content type = %q", upload.artifact.ContentType)
		}
		if !strings.HasSuffix(upload.artifact.Path, ".bin") {
			t.Fatalf("binary Artifact path = %q, want .bin suffix", upload.artifact.Path)
		}
		reference := terminalCollectorSingleTerminalEvent(t, client.events, terminalID, "terminal.output.reference")
		terminal := terminalCollectorTerminalPayloadRequired(t, reference)
		if got, _ := terminal["encoding"].(string); got != "binary" {
			t.Fatalf("binary reference encoding = %q, want binary", got)
		}
	})

	for _, testCase := range []struct {
		name       string
		delta      string
		byteLength int64
	}{
		{name: "missing padding", delta: "AQI", byteLength: 2},
		{name: "embedded newline", delta: "AQI=\n", byteLength: 2},
		{name: "nonzero padding bits", delta: "AB==", byteLength: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			client, collector := newTerminalCollectorForTest(t, uuid.New(), executions.Lease{Generation: 12})
			err := collector.Handle(context.Background(), RunnerMessage{
				Type:         "event",
				EventVersion: executions.RuntimeEventVersionV2,
				EventType:    "content.delta",
				Payload: map[string]any{
					"streamKind": "command_output",
					"terminalId": "terminal-invalid-binary",
					"encoding":   "binary",
					"delta":      testCase.delta,
					"byteOffset": int64(0),
					"byteLength": testCase.byteLength,
				},
				OccurredAt: &terminalCollectorTestTime,
			})
			if err == nil || !strings.Contains(err.Error(), "canonical base64") {
				t.Fatalf("Handle() error = %v, want canonical base64 protocol failure", err)
			}
			if len(client.events) != 0 || len(client.uploads) != 0 {
				t.Fatalf("invalid binary output persisted events=%d uploads=%d", len(client.events), len(client.uploads))
			}
			if len(collector.states) != 0 {
				t.Fatalf("invalid binary output created %d terminal states", len(collector.states))
			}
		})
	}
}

func TestTerminalLogCollectorControlCharactersAreArtifactOnly(t *testing.T) {
	client, collector := newTerminalCollectorForTest(t, uuid.New(), executions.Lease{Generation: 13})
	terminalID := "terminal-control"
	payload := []byte("visible\x00hidden\x1b[31m")

	terminalCollectorMustHandle(t, collector, terminalCollectorLifecycleMessage(terminalID, "terminal.started"))
	terminalCollectorMustHandle(t, collector, terminalCollectorOutputMessage(terminalID, "utf-8", 0, payload))
	terminalCollectorMustHandle(t, collector, terminalCollectorLifecycleMessage(terminalID, "terminal.exited"))

	if previews := terminalCollectorPreviewEvents(client.events, terminalID); len(previews) != 0 {
		t.Fatalf("unsafe control output persisted %d preview events", len(previews))
	}
	if len(client.uploads) != 1 {
		t.Fatalf("expected one Artifact upload, got %d", len(client.uploads))
	}
	if !bytes.Equal(client.uploads[0].body, payload) {
		t.Fatalf("control-character Artifact body = %q, want %q", client.uploads[0].body, payload)
	}
	completion := terminalCollectorSingleTerminalEvent(t, client.events, terminalID, "terminal.exited")
	terminal := terminalCollectorTerminalPayloadRequired(t, completion)
	terminalCollectorRequireInt64(t, terminal, "previewBytes", 0)
	if truncated, _ := terminal["truncated"].(bool); !truncated {
		t.Fatal("control-character completion was not marked truncated")
	}
}

func TestTerminalLogCollectorEventIDsAreStable(t *testing.T) {
	executionID := uuid.MustParse("2f1f5ebc-85a7-4fb8-bce2-d09a4bf338be")
	lease := executions.Lease{Generation: 14}
	run := func(t *testing.T) map[string]uuid.UUID {
		t.Helper()
		client, collector := newTerminalCollectorForTest(t, executionID, lease)
		terminalID := "terminal-stable"
		payload := bytes.Repeat([]byte("s"), int(terminalLogPreviewBytes+1))

		terminalCollectorMustHandle(t, collector, terminalCollectorLifecycleMessage(terminalID, "terminal.started"))
		terminalCollectorMustHandle(t, collector, terminalCollectorOutputMessage(terminalID, "utf-8", 0, payload))
		terminalCollectorMustHandle(t, collector, terminalCollectorLifecycleMessage(terminalID, "terminal.exited"))

		ids := make(map[string]uuid.UUID)
		for _, message := range client.events {
			slot := ""
			switch message.EventType {
			case "content.delta":
				slot = "preview"
			default:
				if terminal, found := terminalCollectorTerminalPayload(message); found {
					slot, _ = terminal["eventType"].(string)
				}
			}
			if slot == "" {
				continue
			}
			if message.EventID == nil {
				t.Fatalf("%s event omitted Event ID", slot)
			}
			if previous, found := ids[slot]; found && previous != *message.EventID {
				t.Fatalf("slot %q produced multiple Event IDs: %s and %s", slot, previous, message.EventID)
			}
			ids[slot] = *message.EventID
		}
		for _, slot := range []string{"terminal.started", "preview", "terminal.output.reference", "terminal.exited"} {
			if _, found := ids[slot]; !found {
				t.Fatalf("stable ID scenario omitted %q event", slot)
			}
		}
		seen := make(map[uuid.UUID]string)
		for slot, eventID := range ids {
			if previous, found := seen[eventID]; found {
				t.Fatalf("slots %q and %q share Event ID %s", previous, slot, eventID)
			}
			seen[eventID] = slot
		}
		return ids
	}

	first := run(t)
	second := run(t)
	if len(first) != len(second) {
		t.Fatalf("Event ID slot count changed: first=%d second=%d", len(first), len(second))
	}
	for slot, firstID := range first {
		if secondID := second[slot]; secondID != firstID {
			t.Fatalf("%s Event ID changed: first=%s second=%s", slot, firstID, secondID)
		}
	}
}

func TestTerminalLogCollectorCloseRemovesTemporaryFiles(t *testing.T) {
	client := &terminalCollectorTestClient{}
	collector := newTerminalLogCollector(client, uuid.New(), executions.Lease{Generation: 15})
	terminalID := "terminal-open"
	payload := []byte("pending output")

	terminalCollectorMustHandle(t, collector, terminalCollectorOutputMessage(terminalID, "utf-8", 0, payload))
	root := collector.tempRoot
	if strings.TrimSpace(root) == "" {
		t.Fatal("collector did not create a temporary root")
	}
	state := collector.states[terminalID]
	if state == nil || state.segmentPath == "" || state.segmentFile == nil {
		t.Fatal("collector did not retain an open temporary segment")
	}
	segmentPath := state.segmentPath
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("stat temp root before Close: %v", err)
	}
	if _, err := os.Stat(segmentPath); err != nil {
		t.Fatalf("stat temp segment before Close: %v", err)
	}

	if err := collector.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if collector.tempRoot != "" {
		t.Fatalf("Close() retained tempRoot %q", collector.tempRoot)
	}
	if state.segmentFile != nil {
		t.Fatal("Close() retained the segment file handle")
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp root still exists after Close: %v", err)
	}
	if _, err := os.Stat(segmentPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp segment still exists after Close: %v", err)
	}
	if err := collector.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestTerminalLogCollectorRedactsSecretAcrossPreviewAndSegmentBoundaries(t *testing.T) {
	secret := "terminal-secret-123456"
	for _, testCase := range []struct {
		name         string
		prefixLength int
	}{
		{name: "preview", prefixLength: int(terminalLogPreviewBytes) - 4},
		{name: "segment", prefixLength: int(terminalLogSegmentBytes) - 4},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			client, collector := newGuardedTerminalCollectorForTest(t, secret)
			terminalID := "terminal-secret-" + testCase.name
			prefix := bytes.Repeat([]byte("a"), testCase.prefixLength)
			source := append(append(append([]byte(nil), prefix...), secret...), []byte("-suffix")...)
			want := append(append(append([]byte(nil), prefix...), secretguard.RedactionMarker...), []byte("-suffix")...)
			split := len(prefix) + len(secret)/2

			terminalCollectorMustHandle(t, collector, terminalCollectorLifecycleMessage(terminalID, "terminal.started"))
			terminalCollectorMustHandle(t, collector, terminalCollectorOutputMessage(
				terminalID, "utf-8", 0, source[:split],
			))
			terminalCollectorMustHandle(t, collector, terminalCollectorOutputMessage(
				terminalID, "utf-8", int64(split), source[split:],
			))
			terminalCollectorMustHandle(t, collector, terminalCollectorLifecycleMessage(terminalID, "terminal.exited"))

			preview := terminalCollectorPreviewBytes(terminalCollectorPreviewEvents(client.events, terminalID))
			if bytes.Contains(preview, []byte(secret)) {
				t.Fatal("Secret leaked into terminal preview")
			}
			var artifactBody []byte
			for _, upload := range client.uploads {
				if bytes.Contains(upload.body, []byte(secret)) {
					t.Fatal("Secret leaked into terminal Artifact")
				}
				artifactBody = append(artifactBody, upload.body...)
			}
			if !bytes.Equal(artifactBody, want) {
				t.Fatalf("terminal Artifact body mismatch: got %d bytes, want %d", len(artifactBody), len(want))
			}
			completion := terminalCollectorSingleTerminalEvent(t, client.events, terminalID, "terminal.exited")
			terminal := terminalCollectorTerminalPayloadRequired(t, completion)
			terminalCollectorRequireInt64(t, terminal, "totalBytes", int64(len(want)))
			terminalCollectorRequireInt64(t, terminal, "segmentCount", int64(len(client.uploads)))
		})
	}
}

func TestTerminalLogCollectorBlocksBinarySecretAcrossChunksAndDeletesSegment(t *testing.T) {
	secret := "terminal-binary-secret-123456"
	client, collector := newGuardedTerminalCollectorForTest(t, secret)
	terminalID := "terminal-binary-secret"
	first := append(bytes.Repeat([]byte{0x01}, 1<<10), secret[:len(secret)/2]...)
	second := append([]byte(secret[len(secret)/2:]), 0x00, 0xff)

	terminalCollectorMustHandle(t, collector, terminalCollectorLifecycleMessage(terminalID, "terminal.started"))
	terminalCollectorMustHandle(t, collector, terminalCollectorOutputMessage(terminalID, "binary", 0, first))
	state := collector.states[terminalID]
	if state == nil || state.segmentPath == "" {
		t.Fatal("binary stream did not create a guarded temporary segment")
	}
	segmentPath := state.segmentPath
	err := collector.Handle(context.Background(), terminalCollectorOutputMessage(
		terminalID, "binary", int64(len(first)), second,
	))
	if !secretguard.IsExposure(err) {
		t.Fatalf("binary secret error = %T %v", err, err)
	}
	if len(client.uploads) != 0 {
		t.Fatalf("binary secret uploaded %d Artifacts", len(client.uploads))
	}
	if state.segmentPath != "" || state.segmentFile != nil {
		t.Fatal("blocked binary state retained its temporary segment")
	}
	if _, statErr := os.Stat(segmentPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("blocked binary segment still exists: %v", statErr)
	}
	if _, finalizeErr := collector.FinalizeOpen(context.Background(), "provider_error"); !secretguard.IsExposure(finalizeErr) {
		t.Fatalf("FinalizeOpen() error = %T %v", finalizeErr, finalizeErr)
	}
}

func TestTerminalLogCollectorSanitizesNonTerminalEventPayload(t *testing.T) {
	secret := "event-payload-secret-123456"
	client, collector := newGuardedTerminalCollectorForTest(t, secret)
	err := collector.Handle(context.Background(), RunnerMessage{
		Type: "event", EventVersion: executions.RuntimeEventVersionV2, EventType: "assistant.message",
		Payload: map[string]any{"nested": map[string]any{"text": "value=" + secret}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(client.events) != 1 {
		t.Fatalf("persisted event count = %d, want 1", len(client.events))
	}
	encoded := mustJSONForSecretGuardTest(t, client.events[0].Payload)
	if strings.Contains(encoded, secret) || !strings.Contains(encoded, secretguard.RedactionMarker) {
		t.Fatalf("non-terminal event was not sanitized: %s", encoded)
	}
}

func TestTerminalLogCollectorRedactsSecretAcrossAssistantTextEvents(t *testing.T) {
	secret := "assistant-delta-secret-123456"
	client, collector := newGuardedTerminalCollectorForTest(t, secret)
	first := "prefix " + secret[:len(secret)/2]
	second := secret[len(secret)/2:] + " suffix"
	for index, delta := range []string{first, second} {
		err := collector.Handle(context.Background(), RunnerMessage{
			Type: "event", EventVersion: executions.RuntimeEventVersionV2, EventType: "content.delta",
			Payload: map[string]any{
				"streamKind": "assistant_text", "contentIndex": float64(0), "delta": delta,
			},
			OccurredAt: &terminalCollectorTestTime,
		})
		if err != nil {
			t.Fatalf("Handle assistant Delta %d: %v", index, err)
		}
	}
	if !collector.HasOpen() {
		t.Fatal("guarded assistant stream was not retained for final flush")
	}
	hadOpenTerminals, err := collector.FinalizeOpen(context.Background(), "provider_error")
	if err != nil {
		t.Fatal(err)
	}
	if hadOpenTerminals {
		t.Fatal("assistant-only finalization reported an open Terminal")
	}
	var persisted strings.Builder
	for _, event := range client.events {
		if event.EventType != "content.delta" || stringMapField(event.Payload, "streamKind") != "assistant_text" {
			continue
		}
		persisted.WriteString(stringMapField(event.Payload, "delta"))
	}
	want := "prefix " + secretguard.RedactionMarker + " suffix"
	if persisted.String() != want || strings.Contains(persisted.String(), secret) {
		t.Fatalf("persisted assistant stream = %q, want %q", persisted.String(), want)
	}
	if collector.HasOpen() {
		t.Fatal("assistant stream remained open after final flush")
	}
}

func TestTerminalLogCollectorRejectsReportedRawByteMismatch(t *testing.T) {
	client, collector := newTerminalCollectorForTest(t, uuid.New(), executions.Lease{Generation: 22})
	terminalID := "terminal-total-mismatch"
	payload := []byte("three")
	terminalCollectorMustHandle(t, collector, terminalCollectorLifecycleMessage(terminalID, "terminal.started"))
	terminalCollectorMustHandle(t, collector, terminalCollectorOutputMessage(terminalID, "utf-8", 0, payload))
	completion := terminalCollectorLifecycleMessage(terminalID, "terminal.exited")
	terminal := terminalCollectorTerminalPayloadRequired(t, completion)
	terminal["totalBytes"] = int64(len(payload) + 1)
	err := collector.Handle(context.Background(), completion)
	if err == nil || !strings.Contains(err.Error(), "totalBytes") {
		t.Fatalf("raw totalBytes mismatch error = %v", err)
	}
	if len(client.uploads) != 0 {
		t.Fatalf("raw totalBytes mismatch uploaded %d Artifacts", len(client.uploads))
	}
}

func newTerminalCollectorForTest(
	t *testing.T,
	executionID uuid.UUID,
	lease executions.Lease,
) (*terminalCollectorTestClient, *terminalLogCollector) {
	t.Helper()
	client := &terminalCollectorTestClient{}
	collector := newTerminalLogCollector(client, executionID, lease)
	t.Cleanup(func() {
		if err := collector.Close(); err != nil {
			t.Errorf("close terminal log collector: %v", err)
		}
	})
	return client, collector
}

func newGuardedTerminalCollectorForTest(
	t *testing.T,
	secret string,
) (*terminalCollectorTestClient, *terminalLogCollector) {
	t.Helper()
	guard := executionGuardForSecretTest(t, secret)
	client := &terminalCollectorTestClient{}
	collector := newTerminalLogCollector(
		client,
		uuid.New(),
		executions.Lease{Generation: 21},
		guard,
	)
	t.Cleanup(func() {
		if err := collector.Close(); err != nil {
			t.Errorf("close guarded terminal log collector: %v", err)
		}
	})
	return client, collector
}

func terminalCollectorMustHandle(t *testing.T, collector *terminalLogCollector, message RunnerMessage) {
	t.Helper()
	if err := collector.Handle(context.Background(), message); err != nil {
		t.Fatalf("Handle(%s) error = %v", message.EventType, err)
	}
}

func terminalCollectorOutputMessage(
	terminalID string,
	encoding string,
	byteOffset int64,
	payload []byte,
) RunnerMessage {
	delta := string(payload)
	if encoding == "binary" {
		delta = base64.StdEncoding.EncodeToString(payload)
	}
	return RunnerMessage{
		Type:         "event",
		EventVersion: executions.RuntimeEventVersionV2,
		EventType:    "content.delta",
		Payload: map[string]any{
			"streamKind": "command_output",
			"terminalId": terminalID,
			"encoding":   encoding,
			"delta":      delta,
			"byteOffset": byteOffset,
			"byteLength": int64(len(payload)),
			"truncated":  false,
		},
		OccurredAt: &terminalCollectorTestTime,
	}
}

func terminalCollectorLifecycleMessage(terminalID, terminalEventType string) RunnerMessage {
	eventType := "item.started"
	status := "inProgress"
	if terminalEventType == "terminal.exited" || terminalEventType == "terminal.failed" {
		eventType = "item.completed"
		status = "completed"
	}
	return RunnerMessage{
		Type:         "event",
		EventVersion: executions.RuntimeEventVersionV2,
		EventType:    eventType,
		Payload: map[string]any{
			"itemType": "command_execution",
			"status":   status,
			"title":    "Terminal",
			"data": map[string]any{
				"provider": "codex",
				"terminal": map[string]any{
					"terminalId":     terminalID,
					"eventType":      terminalEventType,
					"commandSummary": "printf output",
					"cwdLabel":       ".",
				},
			},
		},
		OccurredAt: &terminalCollectorTestTime,
	}
}

func terminalCollectorPreviewEvents(events []RunnerMessage, terminalID string) []RunnerMessage {
	result := make([]RunnerMessage, 0)
	for _, message := range events {
		if message.EventType != "content.delta" {
			continue
		}
		if got, _ := message.Payload["terminalId"].(string); got == terminalID {
			result = append(result, message)
		}
	}
	return result
}

func terminalCollectorPreviewBytes(events []RunnerMessage) []byte {
	var result []byte
	for _, message := range events {
		delta, _ := message.Payload["delta"].(string)
		result = append(result, []byte(delta)...)
	}
	return result
}

func terminalCollectorTerminalEvents(
	events []RunnerMessage,
	terminalID string,
	terminalEventType string,
) []RunnerMessage {
	result := make([]RunnerMessage, 0)
	for _, message := range events {
		terminal, found := terminalCollectorTerminalPayload(message)
		if !found {
			continue
		}
		if got, _ := terminal["terminalId"].(string); got != terminalID {
			continue
		}
		if got, _ := terminal["eventType"].(string); got == terminalEventType {
			result = append(result, message)
		}
	}
	return result
}

func terminalCollectorSingleTerminalEvent(
	t *testing.T,
	events []RunnerMessage,
	terminalID string,
	terminalEventType string,
) RunnerMessage {
	t.Helper()
	matches := terminalCollectorTerminalEvents(events, terminalID, terminalEventType)
	if len(matches) != 1 {
		t.Fatalf(
			"terminal %q event %q count = %d, want 1",
			terminalID,
			terminalEventType,
			len(matches),
		)
	}
	return matches[0]
}

func terminalCollectorTerminalPayload(message RunnerMessage) (map[string]any, bool) {
	data, ok := message.Payload["data"].(map[string]any)
	if !ok {
		return nil, false
	}
	terminal, ok := data["terminal"].(map[string]any)
	return terminal, ok
}

func terminalCollectorTerminalPayloadRequired(t *testing.T, message RunnerMessage) map[string]any {
	t.Helper()
	terminal, found := terminalCollectorTerminalPayload(message)
	if !found {
		t.Fatalf("%s event omitted terminal payload", message.EventType)
	}
	return terminal
}

func terminalCollectorInt64(t *testing.T, payload map[string]any, key string) int64 {
	t.Helper()
	value, found := nonNegativeInt64MapField(payload, key)
	if !found {
		t.Fatalf("field %q is not a non-negative integer: %#v", key, payload[key])
	}
	return value
}

func terminalCollectorRequireInt64(t *testing.T, payload map[string]any, key string, want int64) {
	t.Helper()
	if got := terminalCollectorInt64(t, payload, key); got != want {
		t.Fatalf("field %q = %d, want %d", key, got, want)
	}
}
