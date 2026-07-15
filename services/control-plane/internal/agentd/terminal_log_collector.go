package agentd

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/artifacts"
	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/secretguard"
)

const (
	terminalLogPreviewBytes = int64(32 << 10)
	terminalLogSegmentBytes = int64(1 << 20)
)

var terminalLogEventNamespace = uuid.NewSHA1(
	uuid.NameSpaceURL,
	[]byte("https://synara.dev/runtime-events/terminal-log-v1"),
)

type terminalLogClient interface {
	AppendEvent(context.Context, uuid.UUID, executions.Lease, RunnerMessage) error
	UploadArtifact(context.Context, uuid.UUID, executions.Lease, RunnerArtifact, string) (artifacts.Artifact, error)
}

type terminalLogCollector struct {
	client      terminalLogClient
	executionID uuid.UUID
	lease       executions.Lease
	guard       *executionSecretGuard
	tempRoot    string
	states      map[string]*terminalLogState
	content     map[string]*guardedContentStreamState
}

type guardedContentStreamState struct {
	stream   *secretguard.Stream
	template RunnerMessage
	pending  *RunnerMessage
}

type terminalLogState struct {
	terminalID        string
	provider          string
	commandSummary    string
	cwdLabel          string
	totalBytes        int64
	sourceBytes       int64
	previewBytes      int64
	previewClosed     bool
	artifactRequired  bool
	segmentIndex      int64
	segmentCount      int64
	segmentStart      int64
	segmentBytes      int64
	segmentEncoding   string
	segmentFile       *os.File
	segmentPath       string
	artifactID        *uuid.UUID
	pendingCompletion *RunnerMessage
	secretStream      *secretguard.Stream
	secretStreamMode  secretguard.StreamMode
	blockedErr        error
}

func newTerminalLogCollector(
	client terminalLogClient,
	executionID uuid.UUID,
	lease executions.Lease,
	guards ...*executionSecretGuard,
) *terminalLogCollector {
	var guard *executionSecretGuard
	if len(guards) > 0 {
		guard = guards[0]
	}
	return &terminalLogCollector{
		client: client, executionID: executionID, lease: lease, guard: guard,
		states: make(map[string]*terminalLogState), content: make(map[string]*guardedContentStreamState),
	}
}

func (c *terminalLogCollector) Handle(ctx context.Context, message RunnerMessage) error {
	if message.Type != "event" || message.EventVersion != executions.RuntimeEventVersionV2 {
		if c.guard != nil {
			var err error
			message, err = c.guard.SanitizeRunnerMessage(message)
			if err != nil {
				return err
			}
		}
		return c.client.AppendEvent(ctx, c.executionID, c.lease, message)
	}
	if message.EventType == "content.delta" && stringMapField(message.Payload, "streamKind") == "command_output" {
		return c.handleOutput(ctx, message)
	}
	if message.EventType == "content.delta" && c.guard != nil {
		return c.handleGuardedContentDelta(ctx, message)
	}
	if c.guard != nil {
		var err error
		message, err = c.guard.SanitizeRunnerMessage(message)
		if err != nil {
			return err
		}
	}
	if message.EventType == "item.started" || message.EventType == "item.updated" || message.EventType == "item.completed" {
		terminal, found := terminalLifecycleFromMessage(message)
		if found {
			return c.handleLifecycle(ctx, message, terminal)
		}
	}
	return c.client.AppendEvent(ctx, c.executionID, c.lease, message)
}

func (c *terminalLogCollector) HandleArtifactSource(
	ctx context.Context,
	artifact RunnerArtifact,
	source *artifactUploadSource,
	occurredAt *time.Time,
) error {
	if c.guard != nil {
		for _, value := range []string{
			artifact.Path, artifact.Kind, artifact.OriginalName, artifact.ContentType,
			artifact.SourceRoot, artifact.TerminalID, artifact.Encoding,
		} {
			if err := c.guard.RequireSafeStructuralString(value); err != nil {
				return err
			}
		}
	}
	if artifact.SourceRoot != "runtime-output" || artifact.Kind != "terminal_log" {
		return protocolFailure("Provider Host Runtime Output ArtifactCandidate is invalid")
	}
	terminalID := strings.TrimSpace(artifact.TerminalID)
	if terminalID == "" {
		return protocolFailure("Provider Host Runtime Output omitted terminalId")
	}
	state, found := c.states[terminalID]
	if !found {
		return protocolFailure("Provider Host Runtime Output arrived before terminal.started")
	}
	if state.sourceBytes != 0 || state.totalBytes != 0 || state.segmentBytes != 0 || state.previewBytes != 0 {
		return protocolFailure("Provider Host Runtime Output cannot follow inline command output")
	}
	info, err := source.rewind()
	if err != nil {
		return fmt.Errorf("rewind Runtime Output source: %w", err)
	}
	if artifact.ReportedSize != nil && *artifact.ReportedSize != info.Size() {
		return protocolFailure("Provider Host Runtime Output reportedSize does not match the bound file")
	}
	encoding := artifact.Encoding
	if encoding == "utf-8" {
		encoding, err = classifyTerminalOutputSource(ctx, source)
		if err != nil {
			return err
		}
	} else if encoding != "binary" {
		return protocolFailure("Provider Host Runtime Output encoding is unsupported")
	}
	state.artifactRequired = true
	if encoding == "binary" {
		state.previewClosed = true
	}
	if err := c.copyTerminalOutputSource(ctx, state, source, encoding, occurredAt); err != nil {
		return err
	}
	if state.sourceBytes != info.Size() {
		return errors.New("Runtime Output source changed while it was collected")
	}
	return nil
}

func (c *terminalLogCollector) FinalizeOpen(
	ctx context.Context,
	failureKind string,
) (bool, error) {
	var result error
	if err := c.finalizeGuardedContent(ctx); err != nil {
		result = errors.Join(result, err)
	}
	hadOpenTerminals := len(c.states) > 0
	if !hadOpenTerminals {
		return false, result
	}
	terminalIDs := make([]string, 0, len(c.states))
	for terminalID := range c.states {
		terminalIDs = append(terminalIDs, terminalID)
	}
	sort.Strings(terminalIDs)
	for _, terminalID := range terminalIDs {
		state := c.states[terminalID]
		if err := c.finishState(ctx, state); err != nil {
			result = errors.Join(result, err)
			continue
		}
		message := c.completionMessage(state, "terminal.failed", failureKind, time.Now().UTC())
		if state.pendingCompletion != nil {
			message = *state.pendingCompletion
		}
		if err := c.client.AppendEvent(ctx, c.executionID, c.lease, message); err != nil {
			result = errors.Join(result, err)
			continue
		}
		delete(c.states, terminalID)
	}
	return true, result
}

func (c *terminalLogCollector) HasOpen() bool {
	return len(c.states) > 0 || len(c.content) > 0
}

func (c *terminalLogCollector) Close() error {
	for _, state := range c.content {
		if state.stream != nil {
			_ = state.stream.Close()
			state.stream = nil
		}
		state.pending = nil
	}
	clear(c.content)
	for _, state := range c.states {
		if state.secretStream != nil {
			_ = state.secretStream.Close()
			state.secretStream = nil
		}
		if state.segmentFile != nil {
			_ = state.segmentFile.Close()
			state.segmentFile = nil
		}
	}
	if strings.TrimSpace(c.tempRoot) == "" {
		return nil
	}
	err := os.RemoveAll(c.tempRoot)
	c.tempRoot = ""
	return err
}

func (c *terminalLogCollector) handleGuardedContentDelta(
	ctx context.Context,
	message RunnerMessage,
) error {
	delta, ok := message.Payload["delta"].(string)
	if !ok {
		return protocolFailure("Provider Host text content Delta omitted delta")
	}
	streamKind := strings.TrimSpace(stringMapField(message.Payload, "streamKind"))
	if streamKind == "" {
		return protocolFailure("Provider Host text content Delta omitted streamKind")
	}
	if err := c.guard.RequireSafeStructuralString(streamKind); err != nil {
		return err
	}
	identity, err := guardedContentStreamIdentity(message.Payload, streamKind)
	if err != nil {
		return err
	}
	metadata := cloneMap(message.Payload)
	delete(metadata, "delta")
	template := message
	template.Payload = metadata
	template, err = c.guard.SanitizeRunnerMessage(template)
	if err != nil {
		return err
	}
	state := c.content[identity]
	if state == nil {
		stream, streamErr := c.guard.NewStream(secretguard.StreamText)
		if streamErr != nil {
			return streamErr
		}
		state = &guardedContentStreamState{stream: stream}
		c.content[identity] = state
	}
	state.template = template
	raw := []byte(delta)
	defer zeroBytes(raw)
	output, err := state.stream.Transform(raw)
	if err != nil {
		return err
	}
	defer zeroBytes(output)
	if len(output) == 0 {
		return nil
	}
	safe := template
	safe.Payload = cloneMap(template.Payload)
	safe.Payload["delta"] = string(output)
	return c.client.AppendEvent(ctx, c.executionID, c.lease, safe)
}

func guardedContentStreamIdentity(payload map[string]any, streamKind string) (string, error) {
	identity := streamKind
	for _, key := range []string{"contentIndex", "summaryIndex"} {
		value, found, err := optionalInt64MapField(payload, key)
		if err != nil {
			return "", protocolFailure("Provider Host text content Delta index is invalid")
		}
		if found {
			identity += fmt.Sprintf("\x00%s=%d", key, value)
		} else {
			identity += "\x00" + key + "=-"
		}
	}
	return identity, nil
}

func optionalInt64MapField(value map[string]any, key string) (int64, bool, error) {
	raw, found := value[key]
	if !found {
		return 0, false, nil
	}
	switch candidate := raw.(type) {
	case int:
		return int64(candidate), true, nil
	case int64:
		return candidate, true, nil
	case float64:
		if candidate != float64(int64(candidate)) {
			return 0, false, errors.New("content index is not an integer")
		}
		return int64(candidate), true, nil
	default:
		return 0, false, errors.New("content index has an unsupported type")
	}
}

func (c *terminalLogCollector) finalizeGuardedContent(ctx context.Context) error {
	if len(c.content) == 0 {
		return nil
	}
	identities := make([]string, 0, len(c.content))
	for identity := range c.content {
		identities = append(identities, identity)
	}
	sort.Strings(identities)
	var result error
	for _, identity := range identities {
		state := c.content[identity]
		if err := c.finishGuardedContentState(ctx, identity, state); err != nil {
			result = errors.Join(result, err)
			continue
		}
		delete(c.content, identity)
	}
	return result
}

func (c *terminalLogCollector) finishGuardedContentState(
	ctx context.Context,
	identity string,
	state *guardedContentStreamState,
) error {
	if state.pending == nil && state.stream != nil {
		final, err := state.stream.Finish()
		if err != nil {
			_ = state.stream.Close()
			state.stream = nil
			return err
		}
		closeErr := state.stream.Close()
		state.stream = nil
		if closeErr != nil {
			zeroBytes(final)
			return closeErr
		}
		if len(final) > 0 {
			message := state.template
			message.Payload = cloneMap(state.template.Payload)
			message.Payload["delta"] = string(final)
			message.EventID = c.eventID("content:"+identity, "guard-final")
			state.pending = &message
		}
		zeroBytes(final)
	}
	if state.pending == nil {
		return nil
	}
	if err := c.client.AppendEvent(ctx, c.executionID, c.lease, *state.pending); err != nil {
		return err
	}
	state.pending = nil
	return nil
}

func (c *terminalLogCollector) handleOutput(ctx context.Context, message RunnerMessage) error {
	if c.guard != nil {
		payload := cloneMap(message.Payload)
		delete(payload, "delta")
		sanitized, err := c.guard.SanitizeMap(payload)
		if err != nil {
			return err
		}
		sanitized["delta"] = message.Payload["delta"]
		message.Payload = sanitized
	}
	terminalID := strings.TrimSpace(stringMapField(message.Payload, "terminalId"))
	if terminalID == "" {
		return protocolFailure("Provider Host command output omitted terminalId")
	}
	encoding := stringMapField(message.Payload, "encoding")
	delta, ok := message.Payload["delta"].(string)
	if !ok {
		return protocolFailure("Provider Host command output omitted delta")
	}
	byteOffset, ok := nonNegativeInt64MapField(message.Payload, "byteOffset")
	if !ok {
		return protocolFailure("Provider Host command output omitted byteOffset")
	}
	byteLength, ok := nonNegativeInt64MapField(message.Payload, "byteLength")
	if !ok {
		return protocolFailure("Provider Host command output omitted byteLength")
	}

	var payload []byte
	switch encoding {
	case "utf-8":
		if !utf8.ValidString(delta) {
			return protocolFailure("Provider Host command output contains invalid UTF-8")
		}
		payload = []byte(delta)
	case "binary":
		decoded, err := base64.StdEncoding.Strict().DecodeString(delta)
		if err != nil || base64.StdEncoding.EncodeToString(decoded) != delta {
			return protocolFailure("Provider Host binary command output is not canonical base64")
		}
		payload = decoded
	default:
		return protocolFailure("Provider Host command output encoding is unsupported")
	}
	defer zeroBytes(payload)
	if int64(len(payload)) != byteLength {
		return protocolFailure("Provider Host command output byteLength is invalid")
	}

	state := c.state(terminalID)
	if byteOffset != state.sourceBytes {
		return protocolFailure("Provider Host command output byteOffset is not contiguous")
	}
	return c.ingestOutput(ctx, state, payload, encoding, message.OccurredAt)
}

func (c *terminalLogCollector) ingestOutput(
	ctx context.Context,
	state *terminalLogState,
	payload []byte,
	encoding string,
	occurredAt *time.Time,
) error {
	safePayload, err := c.guardTerminalOutput(state, payload, encoding)
	defer zeroBytes(safePayload)
	state.sourceBytes += int64(len(payload))
	if err != nil {
		state.blockedErr = err
		if discardErr := c.discardSegment(state); discardErr != nil {
			return errors.Join(err, discardErr)
		}
		return err
	}
	return c.ingestSafeOutput(ctx, state, safePayload, encoding, occurredAt)
}

func (c *terminalLogCollector) ingestSafeOutput(
	ctx context.Context,
	state *terminalLogState,
	payload []byte,
	encoding string,
	occurredAt *time.Time,
) error {
	totalAfter := state.totalBytes + int64(len(payload))
	if encoding == "binary" || !safeTerminalPreview(payload) {
		state.artifactRequired = true
		state.previewClosed = true
	}
	if totalAfter > terminalLogPreviewBytes {
		state.artifactRequired = true
	}
	if !state.previewClosed && encoding == "utf-8" {
		remaining := terminalLogPreviewBytes - state.previewBytes
		if remaining <= 0 {
			state.previewClosed = true
		} else {
			prefix := utf8Prefix(payload, remaining)
			if len(prefix) == 0 {
				state.previewClosed = true
			} else {
				truncated := int64(len(prefix)) < int64(len(payload)) || totalAfter > terminalLogPreviewBytes
				preview := RunnerMessage{
					Type: "event", EventVersion: executions.RuntimeEventVersionV2, EventType: "content.delta",
					Payload: map[string]any{
						"streamKind": "command_output", "delta": string(prefix), "terminalId": state.terminalID,
						"encoding": "utf-8", "byteOffset": state.previewBytes, "byteLength": len(prefix),
						"truncated": truncated,
					},
					OccurredAt: occurredAt,
				}
				preview.EventID = c.eventID(state.terminalID, fmt.Sprintf(
					"preview:%d:%d:%s", state.previewBytes, len(prefix), sha256Hex(prefix),
				))
				if err := c.client.AppendEvent(ctx, c.executionID, c.lease, preview); err != nil {
					return err
				}
				state.previewBytes += int64(len(prefix))
				if truncated || state.previewBytes >= terminalLogPreviewBytes {
					state.previewClosed = true
				}
			}
		}
	}
	return c.writeSegment(ctx, state, payload, encoding)
}

func (c *terminalLogCollector) guardTerminalOutput(
	state *terminalLogState,
	payload []byte,
	encoding string,
) ([]byte, error) {
	if c.guard == nil {
		return append([]byte(nil), payload...), nil
	}
	mode := secretguard.StreamText
	if encoding == "binary" {
		mode = secretguard.StreamBinaryDetectOnly
	}
	if state.secretStream == nil {
		stream, err := c.guard.NewStream(mode)
		if err != nil {
			return nil, err
		}
		state.secretStream = stream
		state.secretStreamMode = mode
	} else if state.secretStreamMode != mode {
		return nil, protocolFailure("Provider Host command output changed encoding")
	}
	return state.secretStream.Transform(payload)
}

func classifyTerminalOutputSource(
	ctx context.Context,
	source *artifactUploadSource,
) (string, error) {
	if _, err := source.rewind(); err != nil {
		return "", fmt.Errorf("rewind Runtime Output source: %w", err)
	}
	reader := bufio.NewReaderSize(contextReader{ctx: ctx, reader: source.file}, 64<<10)
	for {
		r, size, err := reader.ReadRune()
		if errors.Is(err, io.EOF) {
			return "utf-8", nil
		}
		if err != nil {
			return "", fmt.Errorf("inspect Runtime Output source: %w", err)
		}
		if (r == utf8.RuneError && size == 1) || !safeTerminalRune(r) {
			return "binary", nil
		}
	}
}

func (c *terminalLogCollector) copyTerminalOutputSource(
	ctx context.Context,
	state *terminalLogState,
	source *artifactUploadSource,
	encoding string,
	occurredAt *time.Time,
) error {
	if _, err := source.rewind(); err != nil {
		return fmt.Errorf("rewind Runtime Output source: %w", err)
	}
	reader := contextReader{ctx: ctx, reader: source.file}
	buffer := make([]byte, 64<<10)
	defer zeroBytes(buffer)
	if encoding == "binary" {
		for {
			read, err := reader.Read(buffer)
			if read > 0 {
				if ingestErr := c.ingestOutput(ctx, state, buffer[:read], encoding, occurredAt); ingestErr != nil {
					return ingestErr
				}
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("read Runtime Output source: %w", err)
			}
		}
	}

	var pending []byte
	defer func() { zeroBytes(pending) }()
	for {
		read, err := reader.Read(buffer)
		combined := make([]byte, 0, len(pending)+read)
		combined = append(combined, pending...)
		combined = append(combined, buffer[:read]...)
		zeroBytes(pending)
		pending = nil
		if errors.Is(err, io.EOF) {
			if len(combined) > 0 {
				if !utf8.Valid(combined) {
					zeroBytes(combined)
					return errors.New("Runtime Output source changed UTF-8 validity while it was collected")
				}
				if ingestErr := c.ingestOutput(ctx, state, combined, encoding, occurredAt); ingestErr != nil {
					zeroBytes(combined)
					return ingestErr
				}
			}
			zeroBytes(combined)
			return nil
		}
		if err != nil {
			zeroBytes(combined)
			return fmt.Errorf("read Runtime Output source: %w", err)
		}
		cutoff := completeUTF8Prefix(combined)
		if cutoff > 0 {
			if !utf8.Valid(combined[:cutoff]) {
				zeroBytes(combined)
				return errors.New("Runtime Output source changed UTF-8 validity while it was collected")
			}
			if ingestErr := c.ingestOutput(ctx, state, combined[:cutoff], encoding, occurredAt); ingestErr != nil {
				zeroBytes(combined)
				return ingestErr
			}
		}
		if cutoff < len(combined) {
			pending = append(pending, combined[cutoff:]...)
		}
		zeroBytes(combined)
	}
}

func completeUTF8Prefix(value []byte) int {
	if utf8.Valid(value) {
		return len(value)
	}
	start := len(value) - 1
	for start >= 0 && !utf8.RuneStart(value[start]) {
		start--
	}
	if start < 0 || utf8.FullRune(value[start:]) {
		return len(value)
	}
	return start
}

func (c *terminalLogCollector) handleLifecycle(
	ctx context.Context,
	message RunnerMessage,
	terminal map[string]any,
) error {
	terminalID := strings.TrimSpace(stringMapField(terminal, "terminalId"))
	if terminalID == "" {
		return protocolFailure("Provider Host terminal lifecycle omitted terminalId")
	}
	eventType := stringMapField(terminal, "eventType")
	state := c.state(terminalID)
	data, _ := message.Payload["data"].(map[string]any)
	if data != nil {
		state.provider = strings.TrimSpace(stringMapField(data, "provider"))
	}
	if value := strings.TrimSpace(stringMapField(terminal, "commandSummary")); value != "" {
		state.commandSummary = value
	}
	if value := strings.TrimSpace(stringMapField(terminal, "cwdLabel")); value != "" {
		state.cwdLabel = value
	}

	switch eventType {
	case "terminal.started":
		message.EventID = c.eventID(terminalID, "started")
		return c.client.AppendEvent(ctx, c.executionID, c.lease, message)
	case "terminal.output.reference":
		return protocolFailure("Provider Host cannot emit terminal Artifact references")
	case "terminal.exited", "terminal.failed":
		if _, found := terminal["totalBytes"]; found {
			providerTotal, ok := nonNegativeInt64MapField(terminal, "totalBytes")
			if !ok || providerTotal != state.sourceBytes {
				return protocolFailure("Provider Host terminal totalBytes does not match collected output")
			}
		}
		if err := c.finishState(ctx, state); err != nil {
			return err
		}
		completion := cloneRunnerMessage(message)
		completion.EventID = c.eventID(terminalID, "completion")
		completion.Payload = cloneMap(message.Payload)
		completionData := cloneMap(data)
		completionTerminal := cloneMap(terminal)
		completionTerminal["totalBytes"] = state.totalBytes
		completionTerminal["previewBytes"] = state.previewBytes
		completionTerminal["segmentCount"] = state.segmentCount
		completionTerminal["truncated"] = terminal["truncated"] == true || state.previewBytes < state.totalBytes
		completionData["terminal"] = completionTerminal
		completion.Payload["data"] = completionData
		state.pendingCompletion = &completion
		if err := c.client.AppendEvent(ctx, c.executionID, c.lease, completion); err != nil {
			return err
		}
		state.pendingCompletion = nil
		delete(c.states, terminalID)
		return nil
	default:
		return protocolFailure("Provider Host terminal lifecycle type is unsupported")
	}
}

func (c *terminalLogCollector) finishState(ctx context.Context, state *terminalLogState) error {
	if state.blockedErr != nil {
		return errors.Join(state.blockedErr, c.discardSegment(state))
	}
	if state.secretStream != nil {
		encoding := "utf-8"
		if state.secretStreamMode == secretguard.StreamBinaryDetectOnly {
			encoding = "binary"
		}
		final, err := state.secretStream.Finish()
		if err != nil {
			state.blockedErr = err
			_ = state.secretStream.Close()
			state.secretStream = nil
			return errors.Join(err, c.discardSegment(state))
		}
		if err := state.secretStream.Close(); err != nil {
			zeroBytes(final)
			state.secretStream = nil
			return err
		}
		state.secretStream = nil
		if err := c.ingestSafeOutput(ctx, state, final, encoding, nil); err != nil {
			zeroBytes(final)
			return err
		}
		zeroBytes(final)
	}
	if state.artifactRequired {
		if state.segmentBytes > 0 {
			return c.flushSegment(ctx, state)
		}
		return nil
	}
	return c.discardSegment(state)
}

func (c *terminalLogCollector) writeSegment(
	ctx context.Context,
	state *terminalLogState,
	payload []byte,
	encoding string,
) error {
	for len(payload) > 0 {
		if err := c.ensureSegment(state); err != nil {
			return err
		}
		if encoding == "binary" {
			state.segmentEncoding = "binary"
		}
		remaining := terminalLogSegmentBytes - state.segmentBytes
		writeBytes := min(int64(len(payload)), remaining)
		written, err := state.segmentFile.Write(payload[:writeBytes])
		if err != nil {
			return fmt.Errorf("write terminal log segment: %w", err)
		}
		if int64(written) != writeBytes {
			return errors.New("write terminal log segment: short write")
		}
		state.segmentBytes += writeBytes
		state.totalBytes += writeBytes
		payload = payload[writeBytes:]
		if state.segmentBytes == terminalLogSegmentBytes {
			state.artifactRequired = true
			if err := c.flushSegment(ctx, state); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *terminalLogCollector) ensureSegment(state *terminalLogState) error {
	if state.segmentPath != "" {
		if state.segmentFile == nil {
			return errors.New("terminal log segment is closed while awaiting persistence")
		}
		return nil
	}
	if c.tempRoot == "" {
		root, err := os.MkdirTemp("", "synara-terminal-logs-")
		if err != nil {
			return fmt.Errorf("create terminal log temp root: %w", err)
		}
		c.tempRoot = root
	}
	file, err := os.CreateTemp(c.tempRoot, "segment-*.log")
	if err != nil {
		return fmt.Errorf("create terminal log segment: %w", err)
	}
	state.segmentFile = file
	state.segmentPath = file.Name()
	state.segmentStart = state.totalBytes
	state.segmentEncoding = "utf-8"
	return nil
}

func (c *terminalLogCollector) flushSegment(ctx context.Context, state *terminalLogState) error {
	if state.segmentBytes == 0 || state.segmentPath == "" {
		return nil
	}
	if state.segmentFile != nil {
		if err := state.segmentFile.Sync(); err != nil {
			return fmt.Errorf("sync terminal log segment: %w", err)
		}
		if err := state.segmentFile.Close(); err != nil {
			return fmt.Errorf("close terminal log segment: %w", err)
		}
		state.segmentFile = nil
	}
	if state.artifactID == nil {
		extension := ".log"
		contentType := "text/plain; charset=utf-8"
		if state.segmentEncoding == "binary" {
			extension = ".bin"
			contentType = "application/octet-stream"
		}
		terminalDigest := sha256.Sum256([]byte(state.terminalID))
		logicalPath := fmt.Sprintf(
			"terminal/%s/segment-%06d%s",
			hex.EncodeToString(terminalDigest[:16]), state.segmentIndex+1, extension,
		)
		artifact, err := c.client.UploadArtifact(ctx, c.executionID, c.lease, RunnerArtifact{
			Path: logicalPath, Kind: "terminal_log",
			OriginalName: fmt.Sprintf("terminal-log-%06d%s", state.segmentIndex+1, extension),
			ContentType:  contentType,
		}, state.segmentPath)
		if err != nil {
			return fmt.Errorf("upload terminal log segment: %w", err)
		}
		state.artifactID = &artifact.ID
	}
	reference := RunnerMessage{
		Type: "event", EventVersion: executions.RuntimeEventVersionV2, EventType: "item.updated",
		Payload: map[string]any{
			"itemType": "command_execution", "status": "inProgress", "title": "Terminal log",
			"data": map[string]any{
				"provider": state.provider,
				"terminal": map[string]any{
					"terminalId": state.terminalID, "eventType": "terminal.output.reference",
					"artifactId": state.artifactID.String(), "offset": state.segmentStart,
					"length": state.segmentBytes, "segmentIndex": state.segmentIndex,
					"encoding": state.segmentEncoding,
				},
			},
		},
	}
	reference.EventID = c.eventID(state.terminalID, fmt.Sprintf("reference:%d", state.segmentIndex))
	now := time.Now().UTC()
	reference.OccurredAt = &now
	if err := c.client.AppendEvent(ctx, c.executionID, c.lease, reference); err != nil {
		return fmt.Errorf("append terminal log reference: %w", err)
	}
	if err := os.Remove(state.segmentPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove terminal log segment: %w", err)
	}
	state.segmentIndex++
	state.segmentCount++
	state.segmentStart = state.totalBytes
	state.segmentBytes = 0
	state.segmentEncoding = ""
	state.segmentPath = ""
	state.artifactID = nil
	return nil
}

func (c *terminalLogCollector) discardSegment(state *terminalLogState) error {
	if state.segmentFile != nil {
		if err := state.segmentFile.Close(); err != nil {
			return fmt.Errorf("close terminal log preview segment: %w", err)
		}
		state.segmentFile = nil
	}
	if state.segmentPath != "" {
		if err := os.Remove(state.segmentPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove terminal log preview segment: %w", err)
		}
	}
	state.segmentPath = ""
	state.segmentBytes = 0
	state.segmentEncoding = ""
	state.artifactID = nil
	return nil
}

func (c *terminalLogCollector) state(terminalID string) *terminalLogState {
	if state, found := c.states[terminalID]; found {
		return state
	}
	state := &terminalLogState{terminalID: terminalID}
	c.states[terminalID] = state
	return state
}

func (c *terminalLogCollector) completionMessage(
	state *terminalLogState,
	eventType string,
	failureKind string,
	occurredAt time.Time,
) RunnerMessage {
	terminal := map[string]any{
		"terminalId": state.terminalID, "eventType": eventType,
		"totalBytes": state.totalBytes, "previewBytes": state.previewBytes,
		"segmentCount": state.segmentCount, "truncated": state.previewBytes < state.totalBytes,
	}
	if state.commandSummary != "" {
		terminal["commandSummary"] = state.commandSummary
	}
	if state.cwdLabel != "" {
		terminal["cwdLabel"] = state.cwdLabel
	}
	if failureKind != "" {
		terminal["failureKind"] = failureKind
	}
	message := RunnerMessage{
		Type: "event", EventVersion: executions.RuntimeEventVersionV2, EventType: "item.completed",
		Payload: map[string]any{
			"itemType": "command_execution", "status": "failed", "title": "Terminal",
			"data": map[string]any{"provider": state.provider, "terminal": terminal},
		},
		OccurredAt: &occurredAt,
	}
	message.EventID = c.eventID(state.terminalID, "completion")
	return message
}

func (c *terminalLogCollector) eventID(terminalID, slot string) *uuid.UUID {
	seed := strings.Join([]string{
		c.executionID.String(), fmt.Sprintf("%d", c.lease.Generation), terminalID, slot,
	}, "\x00")
	eventID := uuid.NewSHA1(terminalLogEventNamespace, []byte(seed))
	return &eventID
}

func terminalLifecycleFromMessage(message RunnerMessage) (map[string]any, bool) {
	data, ok := message.Payload["data"].(map[string]any)
	if !ok || data == nil {
		return nil, false
	}
	terminal, ok := data["terminal"].(map[string]any)
	return terminal, ok && terminal != nil
}

func cloneRunnerMessage(message RunnerMessage) RunnerMessage {
	cloned := message
	cloned.Payload = cloneMap(message.Payload)
	return cloned
}

func cloneMap(value map[string]any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	cloned := make(map[string]any, len(value))
	for key, item := range value {
		cloned[key] = item
	}
	return cloned
}

func stringMapField(value map[string]any, key string) string {
	text, _ := value[key].(string)
	return text
}

func nonNegativeInt64MapField(value map[string]any, key string) (int64, bool) {
	switch candidate := value[key].(type) {
	case int:
		return int64(candidate), candidate >= 0
	case int64:
		return candidate, candidate >= 0
	case float64:
		if candidate < 0 || candidate != float64(int64(candidate)) {
			return 0, false
		}
		return int64(candidate), true
	default:
		return 0, false
	}
}

func safeTerminalPreview(value []byte) bool {
	if !utf8.Valid(value) {
		return false
	}
	for len(value) > 0 {
		r, size := utf8.DecodeRune(value)
		value = value[size:]
		if !safeTerminalRune(r) {
			return false
		}
	}
	return true
}

func safeTerminalRune(r rune) bool {
	if r == '\n' || r == '\r' || r == '\t' {
		return true
	}
	return r >= 0x20 && (r < 0x7f || r > 0x9f) &&
		(r < 0x202a || r > 0x202e) && (r < 0x2066 || r > 0x2069)
}

func utf8Prefix(value []byte, maximum int64) []byte {
	if int64(len(value)) <= maximum {
		return value
	}
	cutoff := int(maximum)
	for cutoff > 0 && !utf8.RuneStart(value[cutoff]) {
		cutoff--
	}
	return value[:cutoff]
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

var _ terminalLogClient = (*Client)(nil)
