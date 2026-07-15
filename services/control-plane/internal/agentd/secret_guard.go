package agentd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/synara-ai/synara/services/control-plane/internal/secretguard"
)

type executionSecretGuard struct {
	mu      sync.RWMutex
	secrets []secretguard.Secret
	guard   *secretguard.Guard
	retired []*secretguard.Guard
	closed  bool
}

type executionSecretGuardContextKey struct{}

func newExecutionSecretGuard(resumeCursor *string) (*executionSecretGuard, error) {
	guard, err := secretguard.New(nil)
	if err != nil {
		return nil, err
	}
	result := &executionSecretGuard{guard: guard}
	if resumeCursor != nil {
		if err := result.addValues(secretguard.Secret{Value: []byte(*resumeCursor)}); err != nil {
			_ = result.Close()
			return nil, err
		}
	}
	return result, nil
}

func (g *executionSecretGuard) AddGitCredential(credential *GitHTTPSCredential) error {
	if credential == nil {
		return nil
	}
	return g.addValues(secretguard.Secret{
		Value: []byte(credential.Token), BasicAuthUsername: []byte(credential.Username),
	})
}

func (g *executionSecretGuard) AddWorkspaceGitCredential(credential *WorkspaceGitCredential) error {
	if credential == nil {
		return nil
	}
	values := make([]secretguard.Secret, 0, 3)
	if credential.HTTPS != nil {
		values = append(values, secretguard.Secret{
			Value: []byte(credential.HTTPS.Token), BasicAuthUsername: []byte(credential.HTTPS.Username),
		})
	}
	if credential.SSH != nil {
		values = append(values, secretguard.Secret{Value: []byte(credential.SSH.PrivateKey)})
		if credential.SSH.PrivateKeyPassphrase != "" {
			values = append(values, secretguard.Secret{Value: []byte(credential.SSH.PrivateKeyPassphrase)})
		}
	}
	return g.addValues(values...)
}

func (g *executionSecretGuard) AddRegistryCredential(credential *RegistryCredential) error {
	if credential == nil {
		return nil
	}
	if credential.CredentialType == "basic" {
		return g.addValues(secretguard.Secret{
			Value: []byte(credential.Password), BasicAuthUsername: []byte(credential.Username),
		})
	}
	return g.addValues(secretguard.Secret{Value: []byte(credential.Token)})
}

func (g *executionSecretGuard) AddPackageCredential(credential *PackageCredential) error {
	if credential == nil {
		return nil
	}
	if credential.Provider == "pypi" {
		return g.addValues(secretguard.Secret{
			Value: []byte(credential.Token), BasicAuthUsername: []byte(credential.Username),
		})
	}
	return g.addValues(secretguard.Secret{Value: []byte(credential.Token)})
}

// AddProviderCredential intentionally recognizes only the credential fields
// accepted by the Provider Host allowlists. Unknown payload strings are not
// treated as secrets merely because they arrived in the same object.
func (g *executionSecretGuard) AddProviderCredential(credential *RunnerCredential) error {
	if credential == nil {
		return nil
	}
	values := make([]secretguard.Secret, 0, 2)
	for _, key := range []string{"apiKey", "authToken"} {
		raw, found := credential.Payload[key]
		if !found {
			continue
		}
		value, ok := raw.(string)
		if !ok || value == "" {
			return &secretguard.ExposureError{Code: secretguard.ErrorCode, Reason: secretguard.ReasonUnsupportedValue}
		}
		values = append(values, secretguard.Secret{Value: []byte(value)})
	}
	return g.addValues(values...)
}

func (g *executionSecretGuard) AddResumeCursor(cursor *string) error {
	if cursor == nil {
		return nil
	}
	return g.addValues(secretguard.Secret{Value: []byte(*cursor)})
}

func (g *executionSecretGuard) addValues(values ...secretguard.Secret) error {
	if len(values) == 0 {
		return nil
	}
	defer clearSecrets(values)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return secretguard.ErrClosed
	}
	additional := make([]secretguard.Secret, 0, len(values))
	for _, value := range values {
		if len(value.Value) == 0 || g.hasValueLocked(value) || containsSecret(additional, value) {
			continue
		}
		additional = append(additional, cloneSecret(value))
	}
	if len(additional) == 0 {
		return nil
	}
	candidate := make([]secretguard.Secret, 0, len(g.secrets)+len(additional))
	candidate = append(candidate, g.secrets...)
	candidate = append(candidate, additional...)
	compiled, err := secretguard.New(candidate)
	if err != nil {
		clearSecrets(additional)
		return err
	}
	previous := g.guard
	g.secrets = candidate
	g.guard = compiled
	if previous != nil {
		// Existing streams keep their immutable matcher until they finish.
		// New sanitization and streams use the freshly compiled superset.
		g.retired = append(g.retired, previous)
	}
	return nil
}

func (g *executionSecretGuard) hasValueLocked(candidate secretguard.Secret) bool {
	return containsSecret(g.secrets, candidate)
}

func (g *executionSecretGuard) SanitizeRunnerMessage(message RunnerMessage) (RunnerMessage, error) {
	for _, value := range []string{message.Type, message.EventType} {
		if err := g.RequireSafeStructuralString(value); err != nil {
			return RunnerMessage{}, err
		}
	}
	result := message
	if message.Payload != nil {
		payload, err := g.SanitizeMap(message.Payload)
		if err != nil {
			return RunnerMessage{}, err
		}
		result.Payload = payload
	}
	if message.Output != nil {
		output, err := g.SanitizeMap(message.Output)
		if err != nil {
			return RunnerMessage{}, err
		}
		result.Output = output
	}
	if message.Artifact != nil {
		artifact := *message.Artifact
		for _, value := range []string{
			artifact.Path, artifact.Kind, artifact.OriginalName, artifact.ContentType,
			artifact.SourceRoot, artifact.TerminalID, artifact.Encoding,
		} {
			if err := g.RequireSafeStructuralString(value); err != nil {
				return RunnerMessage{}, err
			}
		}
		result.Artifact = &artifact
	}
	return result, nil
}

func (g *executionSecretGuard) SanitizeControlResult(result map[string]any) (map[string]any, error) {
	if result == nil {
		return nil, nil
	}
	candidate := cloneMap(result)
	var cursor *string
	if raw, found := candidate["providerResumeCursor"]; found {
		value, ok := raw.(string)
		if !ok || strings.TrimSpace(value) == "" {
			return nil, &secretguard.ExposureError{
				Code: secretguard.ErrorCode, Reason: secretguard.ReasonUnsupportedValue,
			}
		}
		cursor = &value
		delete(candidate, "providerResumeCursor")
		if err := g.AddResumeCursor(cursor); err != nil {
			return nil, err
		}
	}
	sanitized, err := g.SanitizeMap(candidate)
	if err != nil {
		return nil, err
	}
	if cursor != nil {
		sanitized["providerResumeCursor"] = *cursor
	}
	return sanitized, nil
}

func (g *executionSecretGuard) SanitizeMap(value map[string]any) (map[string]any, error) {
	if value == nil {
		return nil, nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.closed || g.guard == nil {
		return nil, secretguard.ErrClosed
	}
	sanitized, _, err := g.guard.Sanitize(value)
	if err != nil {
		return nil, err
	}
	result, ok := sanitized.(map[string]any)
	if !ok {
		return nil, &secretguard.ExposureError{Code: secretguard.ErrorCode, Reason: secretguard.ReasonUnsupportedValue}
	}
	return result, nil
}

func (g *executionSecretGuard) RequireSafeStructuralString(value string) error {
	if value == "" {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.closed || g.guard == nil {
		return secretguard.ErrClosed
	}
	_, changed, err := g.guard.SanitizeString(value)
	if err != nil {
		return err
	}
	if changed {
		return &secretguard.ExposureError{Code: secretguard.ErrorCode, Reason: secretguard.ReasonMapKeyMatch}
	}
	return nil
}

func (g *executionSecretGuard) SanitizeError(value error) error {
	if value == nil {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.closed || g.guard == nil {
		return secretguard.ErrClosed
	}
	sanitized, changed, err := g.guard.SanitizeString(value.Error())
	if err != nil {
		return err
	}
	if !changed {
		return value
	}
	var problem *controlPlaneProblem
	if errors.As(value, &problem) {
		cloned := *problem
		message, _, sanitizeErr := g.guard.SanitizeString(problem.Message)
		if sanitizeErr != nil {
			return sanitizeErr
		}
		cloned.Message = strings.TrimSpace(message)
		return &cloned
	}
	var failure *runnerFailure
	if errors.As(value, &failure) {
		cloned := *failure
		cloned.message = safeRunnerMessage(sanitized)
		return &cloned
	}
	return errors.New(strings.TrimSpace(sanitized))
}

func (g *executionSecretGuard) SanitizeResult(result RunnerResult) (RunnerResult, error) {
	if err := g.AddResumeCursor(result.ProviderResumeCursor); err != nil {
		return RunnerResult{}, err
	}
	var err error
	if result.Output != nil {
		result.Output, err = g.SanitizeMap(result.Output)
		if err != nil {
			return RunnerResult{}, err
		}
	}
	if result.PrimaryOperationResult != nil {
		primaryResult := cloneMap(result.PrimaryOperationResult)
		delete(primaryResult, "providerResumeCursor")
		result.PrimaryOperationResult, err = g.SanitizeMap(primaryResult)
		if err != nil {
			return RunnerResult{}, err
		}
		// The cursor travels only through the dedicated encrypted persistence
		// path. Keep it available long enough for Client to extract it while
		// every duplicate occurrence has already been redacted above.
		if result.ProviderResumeCursor != nil {
			result.PrimaryOperationResult["providerResumeCursor"] = *result.ProviderResumeCursor
		}
	}
	return result, nil
}

func (g *executionSecretGuard) NewStream(mode secretguard.StreamMode) (*secretguard.Stream, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.closed || g.guard == nil {
		return nil, secretguard.ErrClosed
	}
	return g.guard.NewStream(mode)
}

func (g *executionSecretGuard) Close() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil
	}
	g.closed = true
	compiled := make([]*secretguard.Guard, 0, len(g.retired)+1)
	if g.guard != nil {
		compiled = append(compiled, g.guard)
	}
	compiled = append(compiled, g.retired...)
	g.guard = nil
	g.retired = nil
	clearSecrets(g.secrets)
	g.secrets = nil
	g.mu.Unlock()
	var result error
	for _, guard := range compiled {
		result = errors.Join(result, guard.Close())
	}
	return result
}

func withExecutionSecretGuard(ctx context.Context, guard *executionSecretGuard) context.Context {
	if guard == nil {
		return ctx
	}
	return context.WithValue(ctx, executionSecretGuardContextKey{}, guard)
}

func executionSecretGuardFromContext(ctx context.Context) *executionSecretGuard {
	if ctx == nil {
		return nil
	}
	guard, _ := ctx.Value(executionSecretGuardContextKey{}).(*executionSecretGuard)
	return guard
}

func sanitizeExecutionContextError(ctx context.Context, value error) error {
	if guard := executionSecretGuardFromContext(ctx); guard != nil {
		return guard.SanitizeError(value)
	}
	return value
}

func cloneSecret(value secretguard.Secret) secretguard.Secret {
	return secretguard.Secret{
		Value:             append([]byte(nil), value.Value...),
		BasicAuthUsername: append([]byte(nil), value.BasicAuthUsername...),
	}
}

func containsSecret(values []secretguard.Secret, candidate secretguard.Secret) bool {
	for _, existing := range values {
		if bytes.Equal(existing.Value, candidate.Value) &&
			bytes.Equal(existing.BasicAuthUsername, candidate.BasicAuthUsername) {
			return true
		}
	}
	return false
}

func clearSecrets(values []secretguard.Secret) {
	for index := range values {
		zeroBytes(values[index].Value)
		zeroBytes(values[index].BasicAuthUsername)
		values[index].Value = nil
		values[index].BasicAuthUsername = nil
	}
}

func clearRunnerCredential(credential *RunnerCredential) {
	if credential == nil {
		return
	}
	for key := range credential.Payload {
		delete(credential.Payload, key)
	}
	credential.Payload = nil
}
