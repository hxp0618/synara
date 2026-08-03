package kmsworker

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maximumRequestBytes = 128 << 10

type Authorizer struct {
	identities map[string]map[Role]struct{}
}

func NewAuthorizer(input map[string][]Role) (*Authorizer, error) {
	if len(input) == 0 {
		return nil, errors.New("at least one KMS mTLS identity is required")
	}
	result := &Authorizer{identities: make(map[string]map[Role]struct{}, len(input))}
	for rawIdentity, roles := range input {
		identity := strings.TrimSpace(rawIdentity)
		if identity == "" || len(identity) > 500 || len(roles) == 0 {
			return nil, errors.New("KMS mTLS identity configuration is invalid")
		}
		roleSet := map[Role]struct{}{}
		for _, role := range roles {
			switch role {
			case RoleControlPlaneCryptor, RoleKeyManager, RoleKeyDisabler, RoleDeletionApprover, RoleAuditor:
				roleSet[role] = struct{}{}
			default:
				return nil, fmt.Errorf("unsupported KMS role %q", role)
			}
		}
		result.identities[identity] = roleSet
	}
	return result, nil
}

func (a *Authorizer) Authenticate(r *http.Request, allowed ...Role) (Actor, error) {
	identity, err := verifiedPeerIdentity(r.TLS)
	if err != nil {
		return Actor{}, serviceError(http.StatusUnauthorized, "key_policy_denied", err)
	}
	roles, exists := a.identities[identity]
	if !exists {
		return Actor{}, serviceError(http.StatusForbidden, "key_policy_denied", errors.New("mTLS identity is not authorized"))
	}
	for _, role := range allowed {
		if _, exists := roles[role]; exists {
			return Actor{Identity: identity, Role: role}, nil
		}
	}
	return Actor{}, serviceError(http.StatusForbidden, "key_policy_denied", errors.New("mTLS identity lacks the required role"))
}

func verifiedPeerIdentity(state *tls.ConnectionState) (string, error) {
	if state == nil || len(state.PeerCertificates) == 0 || len(state.VerifiedChains) == 0 {
		return "", errors.New("a verified client certificate is required")
	}
	certificate := state.PeerCertificates[0]
	if len(certificate.URIs) == 1 {
		identity := strings.TrimSpace(certificate.URIs[0].String())
		if identity != "" {
			return identity, nil
		}
	}
	if len(certificate.URIs) > 1 {
		return "", errors.New("client certificate must contain at most one URI identity")
	}
	identity := strings.TrimSpace(certificate.Subject.CommonName)
	if identity == "" {
		return "", errors.New("client certificate identity is missing")
	}
	return identity, nil
}

type HTTPServer struct {
	service    *Service
	authorizer *Authorizer
	metrics    *requestMetrics
}

func NewHTTPServer(service *Service, authorizer *Authorizer) (*HTTPServer, error) {
	if service == nil || authorizer == nil {
		return nil, errors.New("KMS service and authorizer are required")
	}
	return &HTTPServer{service: service, authorizer: authorizer, metrics: newRequestMetrics()}, nil
}

func (s *HTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	captured := &statusCapturingWriter{ResponseWriter: w, status: http.StatusOK}
	s.serveHTTP(captured, r)
	s.metrics.record(metricOperation(r), captured.status, time.Since(started))
}

func (s *HTTPServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/health/live":
		if r.Method != http.MethodGet {
			s.writeMethodNotAllowed(w)
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]string{"status": "live"})
		return
	case "/health/ready":
		if r.Method != http.MethodGet {
			s.writeMethodNotAllowed(w)
			return
		}
		if err := s.service.Ready(r.Context()); err != nil {
			s.writeError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
		return
	case "/v1/audit-events":
		s.handleAuditEvents(w, r)
		return
	case "/v1/reseal-receipts":
		s.handleResealReceipts(w, r)
		return
	case "/v1/destruction-receipts":
		s.handleDestructionReceipts(w, r)
		return
	case "/metrics":
		s.handleMetrics(w, r)
		return
	case "/v1/keys":
		s.handleKeyCollection(w, r)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/v1/keys/") {
		s.writeError(w, serviceError(http.StatusNotFound, "key_not_found", errors.New("route not found")))
		return
	}
	relative := strings.TrimPrefix(r.URL.Path, "/v1/keys/")
	if strings.Contains(relative, "/versions/") {
		s.handleVersion(w, r, relative)
		return
	}
	s.handleLogicalKey(w, r, relative)
}

func (s *HTTPServer) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeMethodNotAllowed(w)
		return
	}
	if _, err := s.authorizer.Authenticate(r, RoleAuditor); err != nil {
		s.writeError(w, err)
		return
	}
	snapshot, err := s.service.Metrics(r.Context())
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.WriteHeader(http.StatusOK)
	for _, state := range []string{StateActive, StateDecryptOnly, StateDisabled, StatePendingDeletion, StateDestroyed} {
		_, _ = fmt.Fprintf(w, "synara_kms_key_versions{state=%q} %d\n", state, snapshot.VersionStates[state])
	}
	_, _ = fmt.Fprintf(w, "synara_kms_seconds_until_primary_expiry %g\n", snapshot.SecondsUntilPrimaryExpiry)
	_, _ = fmt.Fprintf(w, "synara_kms_pending_deletions %d\n", snapshot.PendingDeletions)
	_, _ = fmt.Fprintf(w, "synara_kms_seconds_until_next_deletion %g\n", snapshot.SecondsUntilNextDeletion)
	_, _ = fmt.Fprintf(w, "synara_kms_seal_key_info{key_id=%q} 1\n", snapshot.SealKeyID)
	_, _ = fmt.Fprintln(w, "synara_kms_audit_outbox_depth 0")
	s.metrics.writePrometheus(w)
}

func (s *HTTPServer) handleKeyCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeMethodNotAllowed(w)
		return
	}
	actor, err := s.authorizer.Authenticate(r, RoleKeyManager)
	if err != nil {
		s.writeError(w, err)
		return
	}
	var input CreateKeyInput
	if err := decodeStrictJSON(w, r, &input); err != nil {
		s.writeError(w, err)
		return
	}
	input.IdempotencyKey = r.Header.Get("Idempotency-Key")
	created, err := s.service.CreateKey(r.Context(), actor, input)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, created)
}

func (s *HTTPServer) handleLogicalKey(w http.ResponseWriter, r *http.Request, relative string) {
	if strings.HasSuffix(relative, ":rotate") {
		if r.Method != http.MethodPost {
			s.writeMethodNotAllowed(w)
			return
		}
		keyID := strings.TrimSuffix(relative, ":rotate")
		actor, err := s.authorizer.Authenticate(r, RoleKeyManager)
		if err != nil {
			s.writeError(w, err)
			return
		}
		var input RotateKeyInput
		if err := decodeStrictJSON(w, r, &input); err != nil {
			s.writeError(w, err)
			return
		}
		input.IdempotencyKey = r.Header.Get("Idempotency-Key")
		result, err := s.service.RotateKey(r.Context(), actor, keyID, input)
		if err != nil {
			s.writeError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, result)
		return
	}
	if strings.Contains(relative, "/") || relative == "" {
		s.writeError(w, serviceError(http.StatusNotFound, "key_not_found", errors.New("route not found")))
		return
	}
	switch r.Method {
	case http.MethodGet:
		actor, err := s.authorizer.Authenticate(r, RoleControlPlaneCryptor, RoleKeyManager, RoleKeyDisabler, RoleDeletionApprover, RoleAuditor)
		if err != nil {
			s.writeError(w, err)
			return
		}
		result, err := s.service.DescribeKey(r.Context(), actor, relative)
		if err != nil {
			s.writeError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, result)
	case http.MethodPatch:
		actor, err := s.authorizer.Authenticate(r, RoleKeyManager)
		if err != nil {
			s.writeError(w, err)
			return
		}
		input, err := decodeUpdateKey(w, r)
		if err != nil {
			s.writeError(w, err)
			return
		}
		input.IdempotencyKey = r.Header.Get("Idempotency-Key")
		result, err := s.service.UpdateKey(r.Context(), actor, relative, input)
		if err != nil {
			s.writeError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, result)
	default:
		s.writeMethodNotAllowed(w)
	}
}

func (s *HTTPServer) handleVersion(w http.ResponseWriter, r *http.Request, relative string) {
	keyID, rest, found := strings.Cut(relative, "/versions/")
	if !found || keyID == "" {
		s.writeError(w, serviceError(http.StatusNotFound, "key_version_not_found", errors.New("route not found")))
		return
	}
	encodedVersion, operation, found := strings.Cut(rest, ":")
	version, err := strconv.ParseInt(encodedVersion, 10, 64)
	if !found || version <= 0 || err != nil || strings.Contains(operation, "/") {
		s.writeError(w, serviceError(http.StatusNotFound, "key_version_not_found", errors.New("route not found")))
		return
	}
	if r.Method != http.MethodPost {
		s.writeMethodNotAllowed(w)
		return
	}
	switch operation {
	case "wrap":
		actor, authErr := s.authorizer.Authenticate(r, RoleControlPlaneCryptor)
		if authErr != nil {
			s.writeError(w, authErr)
			return
		}
		var input struct {
			DataKey []byte `json:"dataKey"`
			AAD     []byte `json:"aad"`
		}
		if err := decodeStrictJSON(w, r, &input); err != nil {
			s.writeError(w, err)
			return
		}
		result, err := s.service.Wrap(r.Context(), actor, keyID, version, input.DataKey, input.AAD)
		zeroBytes(input.DataKey)
		if err != nil {
			s.writeError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, result)
	case "unwrap":
		actor, authErr := s.authorizer.Authenticate(r, RoleControlPlaneCryptor)
		if authErr != nil {
			s.writeError(w, authErr)
			return
		}
		var input struct {
			WrappedDataKey []byte `json:"wrappedDataKey"`
			AAD            []byte `json:"aad"`
		}
		if err := decodeStrictJSON(w, r, &input); err != nil {
			s.writeError(w, err)
			return
		}
		plaintext, err := s.service.Unwrap(r.Context(), actor, keyID, version, input.WrappedDataKey, input.AAD)
		if err != nil {
			s.writeError(w, err)
			return
		}
		defer zeroBytes(plaintext)
		s.writeJSON(w, http.StatusOK, map[string]any{"keyId": VersionedKeyID(keyID, version), "dataKey": plaintext})
	case "disable":
		actor, input, ok := s.decodeLifecycleRequest(w, r, RoleKeyDisabler, &DisableKeyInput{})
		if !ok {
			return
		}
		disable := input.(*DisableKeyInput)
		disable.IdempotencyKey = r.Header.Get("Idempotency-Key")
		result, err := s.service.DisableKey(r.Context(), actor, keyID, version, *disable)
		if err != nil {
			s.writeError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, result)
	case "scheduleDeletion":
		actor, input, ok := s.decodeLifecycleRequest(w, r, RoleKeyDisabler, &ScheduleDeletionInput{})
		if !ok {
			return
		}
		schedule := input.(*ScheduleDeletionInput)
		schedule.IdempotencyKey = r.Header.Get("Idempotency-Key")
		result, err := s.service.ScheduleDeletion(r.Context(), actor, keyID, version, *schedule)
		if err != nil {
			s.writeError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, result)
	case "cancelDeletion":
		actor, input, ok := s.decodeLifecycleRequest(w, r, RoleKeyDisabler, &CancelDeletionInput{})
		if !ok {
			return
		}
		cancel := input.(*CancelDeletionInput)
		cancel.IdempotencyKey = r.Header.Get("Idempotency-Key")
		result, err := s.service.CancelDeletion(r.Context(), actor, keyID, version, *cancel)
		if err != nil {
			s.writeError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, result)
	case "destroy":
		actor, input, ok := s.decodeLifecycleRequest(w, r, RoleDeletionApprover, &DestroyKeyInput{})
		if !ok {
			return
		}
		destroy := input.(*DestroyKeyInput)
		destroy.IdempotencyKey = r.Header.Get("Idempotency-Key")
		result, err := s.service.DestroyKey(r.Context(), actor, keyID, version, *destroy)
		if err != nil {
			s.writeError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, result)
	default:
		s.writeError(w, serviceError(http.StatusNotFound, "key_version_not_found", errors.New("route not found")))
	}
}

func (s *HTTPServer) decodeLifecycleRequest(w http.ResponseWriter, r *http.Request, role Role, input any) (Actor, any, bool) {
	actor, err := s.authorizer.Authenticate(r, role)
	if err != nil {
		s.writeError(w, err)
		return Actor{}, nil, false
	}
	if err := decodeStrictJSON(w, r, input); err != nil {
		s.writeError(w, err)
		return Actor{}, nil, false
	}
	return actor, input, true
}

func (s *HTTPServer) handleAuditEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeMethodNotAllowed(w)
		return
	}
	actor, err := s.authorizer.Authenticate(r, RoleAuditor)
	if err != nil {
		s.writeError(w, err)
		return
	}
	events, err := s.service.ListAuditEvents(r.Context(), actor, queryLimit(r))
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func (s *HTTPServer) handleResealReceipts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeMethodNotAllowed(w)
		return
	}
	actor, err := s.authorizer.Authenticate(r, RoleAuditor)
	if err != nil {
		s.writeError(w, err)
		return
	}
	receipts, err := s.service.ListResealReceipts(r.Context(), actor, queryLimit(r))
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"receipts": receipts})
}

func (s *HTTPServer) handleDestructionReceipts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeMethodNotAllowed(w)
		return
	}
	actor, err := s.authorizer.Authenticate(r, RoleAuditor)
	if err != nil {
		s.writeError(w, err)
		return
	}
	receipts, err := s.service.ListDestructionReceipts(r.Context(), actor, queryLimit(r))
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"receipts": receipts})
}

func decodeStrictJSON(w http.ResponseWriter, r *http.Request, output any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maximumRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return serviceError(http.StatusBadRequest, "key_policy_denied", errors.New("request JSON is invalid"))
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return serviceError(http.StatusBadRequest, "key_policy_denied", errors.New("request must contain exactly one JSON value"))
	}
	return nil
}

func decodeUpdateKey(w http.ResponseWriter, r *http.Request) (UpdateKeyInput, error) {
	var raw map[string]json.RawMessage
	if err := decodeStrictJSON(w, r, &raw); err != nil {
		return UpdateKeyInput{}, err
	}
	allowed := map[string]bool{
		"name": true, "description": true, "policyReference": true, "labels": true,
		"encryptNotAfter": true, "decryptNotAfter": true, "expectedRevision": true,
	}
	for key := range raw {
		if !allowed[key] {
			return UpdateKeyInput{}, serviceError(http.StatusBadRequest, "key_policy_denied", fmt.Errorf("unknown update field %q", key))
		}
	}
	var input UpdateKeyInput
	if value, exists := raw["name"]; exists {
		var decoded string
		if err := json.Unmarshal(value, &decoded); err != nil {
			return UpdateKeyInput{}, serviceError(http.StatusBadRequest, "key_policy_denied", errors.New("name must be a string"))
		}
		input.Name = &decoded
	}
	if value, exists := raw["description"]; exists {
		var decoded string
		if err := json.Unmarshal(value, &decoded); err != nil {
			return UpdateKeyInput{}, serviceError(http.StatusBadRequest, "key_policy_denied", errors.New("description must be a string"))
		}
		input.Description = &decoded
	}
	if value, exists := raw["policyReference"]; exists {
		var decoded string
		if err := json.Unmarshal(value, &decoded); err != nil {
			return UpdateKeyInput{}, serviceError(http.StatusBadRequest, "key_policy_denied", errors.New("policyReference must be a string"))
		}
		input.PolicyReference = &decoded
	}
	if value, exists := raw["labels"]; exists {
		var decoded map[string]string
		if err := json.Unmarshal(value, &decoded); err != nil {
			return UpdateKeyInput{}, serviceError(http.StatusBadRequest, "key_policy_denied", errors.New("labels must be a string map"))
		}
		input.Labels = &decoded
	}
	if value, exists := raw["expectedRevision"]; exists {
		if err := json.Unmarshal(value, &input.ExpectedRevision); err != nil {
			return UpdateKeyInput{}, serviceError(http.StatusBadRequest, "key_version_conflict", errors.New("expectedRevision must be an integer"))
		}
	}
	var err error
	if value, exists := raw["encryptNotAfter"]; exists {
		input.EncryptNotAfter.Present = true
		input.EncryptNotAfter.Value, err = decodeOptionalTime(value)
		if err != nil {
			return UpdateKeyInput{}, err
		}
	}
	if value, exists := raw["decryptNotAfter"]; exists {
		input.DecryptNotAfter.Present = true
		input.DecryptNotAfter.Value, err = decodeOptionalTime(value)
		if err != nil {
			return UpdateKeyInput{}, err
		}
	}
	return input, nil
}

func decodeOptionalTime(raw json.RawMessage) (*time.Time, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	var decoded time.Time
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, serviceError(http.StatusBadRequest, "key_policy_denied", errors.New("lifecycle timestamp must be RFC3339 or null"))
	}
	return normalizeTime(&decoded), nil
}

func queryLimit(r *http.Request) int {
	value, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	return value
}

func (s *HTTPServer) writeMethodNotAllowed(w http.ResponseWriter) {
	s.writeError(w, serviceError(http.StatusMethodNotAllowed, "key_policy_denied", errors.New("method not allowed")))
}

func (s *HTTPServer) writeError(w http.ResponseWriter, err error) {
	status, code := errorStatus(err)
	s.writeJSON(w, status, map[string]any{"error": map[string]string{"code": code}})
}

func (s *HTTPServer) writeJSON(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

type statusCapturingWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusCapturingWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

type requestMetric struct {
	Count           uint64
	DurationSeconds float64
}

type requestMetrics struct {
	mu     sync.Mutex
	values map[string]requestMetric
}

func newRequestMetrics() *requestMetrics {
	return &requestMetrics{values: map[string]requestMetric{}}
}

func (m *requestMetrics) record(operation string, status int, duration time.Duration) {
	key := operation + "\x00" + strconv.Itoa(status)
	m.mu.Lock()
	value := m.values[key]
	value.Count++
	value.DurationSeconds += duration.Seconds()
	m.values[key] = value
	m.mu.Unlock()
}

func (m *requestMetrics) writePrometheus(w io.Writer) {
	m.mu.Lock()
	copyValues := make(map[string]requestMetric, len(m.values))
	for key, value := range m.values {
		copyValues[key] = value
	}
	m.mu.Unlock()
	keys := make([]string, 0, len(copyValues))
	for key := range copyValues {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		operation, status, _ := strings.Cut(key, "\x00")
		value := copyValues[key]
		_, _ = fmt.Fprintf(w, "synara_kms_requests_total{operation=%q,status=%q} %d\n", operation, status, value.Count)
		_, _ = fmt.Fprintf(w, "synara_kms_request_duration_seconds_sum{operation=%q,status=%q} %g\n", operation, status, value.DurationSeconds)
		_, _ = fmt.Fprintf(w, "synara_kms_request_duration_seconds_count{operation=%q,status=%q} %d\n", operation, status, value.Count)
	}
}

func metricOperation(r *http.Request) string {
	path := r.URL.Path
	switch {
	case path == "/health/live":
		return "health-live"
	case path == "/health/ready":
		return "health-ready"
	case path == "/metrics":
		return "metrics"
	case path == "/v1/keys" && r.Method == http.MethodPost:
		return "create-key"
	case path == "/v1/audit-events":
		return "list-audit"
	case path == "/v1/reseal-receipts":
		return "list-reseal-receipts"
	case path == "/v1/destruction-receipts":
		return "list-destruction-receipts"
	case strings.HasSuffix(path, ":rotate"):
		return "rotate-key"
	case strings.HasSuffix(path, ":wrap"):
		return "wrap"
	case strings.HasSuffix(path, ":unwrap"):
		return "unwrap"
	case strings.HasSuffix(path, ":disable"):
		return "disable-key"
	case strings.HasSuffix(path, ":scheduleDeletion"):
		return "schedule-deletion"
	case strings.HasSuffix(path, ":cancelDeletion"):
		return "cancel-deletion"
	case strings.HasSuffix(path, ":destroy"):
		return "destroy-key"
	case strings.HasPrefix(path, "/v1/keys/") && r.Method == http.MethodPatch:
		return "update-key"
	case strings.HasPrefix(path, "/v1/keys/") && r.Method == http.MethodGet:
		return "describe-key"
	default:
		return "unknown"
	}
}
