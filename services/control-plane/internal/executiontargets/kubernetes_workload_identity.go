package executiontargets

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	kubernetesWorkloadIdentityAudiencePrefix = "synara.execution-target."
	kubernetesPodNameExtraKey                = "authentication.kubernetes.io/pod-name"
	kubernetesPodUIDExtraKey                 = "authentication.kubernetes.io/pod-uid"
	kubernetesWorkerModeLabel                = "synara.io/worker-mode"
	kubernetesWorkerPoolIDLabel              = "synara.io/worker-pool-id"
	kubernetesWorkerPoolVersionLabel         = "synara.io/worker-pool-version"
	kubernetesCapacityClassLabel             = "synara.io/capacity-class"
	kubernetesWorkerModeExecutionPinned      = "execution-pinned"
	kubernetesWorkerModeWarmPool             = "warm-pool"
	kubernetesWorkerModeGeneralPool          = "general-pool"
)

type KubernetesWorkloadIdentityClaims struct {
	Namespace string `json:"namespace"`
	PodName   string `json:"podName"`
	PodUID    string `json:"podUid"`
}

type VerifiedKubernetesWorkloadIdentity struct {
	TargetID                       uuid.UUID  `json:"targetId"`
	Audience                       string     `json:"audience"`
	ClusterID                      string     `json:"clusterId"`
	Namespace                      string     `json:"namespace"`
	PodName                        string     `json:"podName"`
	PodUID                         string     `json:"podUid"`
	WorkerMode                     string     `json:"workerMode"`
	AssignedExecutionID            *uuid.UUID `json:"assignedExecutionId,omitempty"`
	WorkerPoolID                   *uuid.UUID `json:"workerPoolId,omitempty"`
	WorkerPoolVersion              *int64     `json:"workerPoolVersion,omitempty"`
	CapacityClass                  *string    `json:"capacityClass,omitempty"`
	RequestedCPUMillicores         *int64     `json:"requestedCpuMillicores,omitempty"`
	RequestedMemoryBytes           *int64     `json:"requestedMemoryBytes,omitempty"`
	RequestedEphemeralStorageBytes *int64     `json:"requestedEphemeralStorageBytes,omitempty"`
	ServiceAccountName             string     `json:"serviceAccountName"`
	ServiceAccountUsername         string     `json:"serviceAccountUsername"`
}

func KubernetesWorkloadIdentityAudience(targetID uuid.UUID) string {
	return kubernetesWorkloadIdentityAudiencePrefix + targetID.String()
}

func KubernetesWorkerRegistrationAudience(targetID uuid.UUID) string {
	return KubernetesWorkloadIdentityAudience(targetID)
}

func (s *Service) VerifyWorkerRegistration(
	ctx context.Context,
	targetID uuid.UUID,
	namespace, podName, podUID, bearerToken string,
) error {
	_, err := s.VerifyKubernetesWorkerRegistration(ctx, targetID, namespace, podName, podUID, bearerToken)
	return err
}

func (s *Service) VerifyKubernetesWorkerRegistration(
	ctx context.Context,
	targetID uuid.UUID,
	namespace, podName, podUID, bearerToken string,
) (VerifiedKubernetesWorkloadIdentity, error) {
	return s.VerifyKubernetesWorkloadIdentity(ctx, targetID, KubernetesWorkloadIdentityClaims{
		Namespace: namespace,
		PodName:   podName,
		PodUID:    podUID,
	}, bearerToken)
}

func (s *Service) VerifyKubernetesWorkloadIdentity(
	ctx context.Context,
	targetID uuid.UUID,
	claims KubernetesWorkloadIdentityClaims,
	bearerToken string,
) (VerifiedKubernetesWorkloadIdentity, error) {
	claims, bearerToken, err := normalizeKubernetesWorkloadIdentityInput(targetID, claims, bearerToken)
	if err != nil {
		return VerifiedKubernetesWorkloadIdentity{}, err
	}
	target, err := s.loadKubernetesWorkloadIdentityTarget(ctx, targetID)
	if err != nil {
		return VerifiedKubernetesWorkloadIdentity{}, err
	}
	configuration, err := s.loadKubernetesWorkloadIdentityConfiguration(target)
	if err != nil {
		return VerifiedKubernetesWorkloadIdentity{}, err
	}
	if claims.Namespace != configuration.Namespace {
		return VerifiedKubernetesWorkloadIdentity{}, problem.New(
			401,
			"kubernetes_workload_identity_pod_claim_mismatch",
			"Kubernetes workload Pod claims do not match this execution target.",
		)
	}
	client, err := newKubernetesHTTPClient(configuration)
	if err != nil {
		return VerifiedKubernetesWorkloadIdentity{}, problem.Wrap(
			503,
			"kubernetes_api_unavailable",
			"Kubernetes API configuration is unavailable.",
			err,
		)
	}
	audience := KubernetesWorkloadIdentityAudience(targetID)
	review, err := client.ReviewServiceAccountToken(ctx, bearerToken, audience)
	if err != nil {
		return VerifiedKubernetesWorkloadIdentity{}, problem.Wrap(
			502,
			"kubernetes_workload_identity_review_failed",
			"Kubernetes workload identity could not be reviewed.",
			err,
		)
	}
	if !review.Authenticated {
		return VerifiedKubernetesWorkloadIdentity{}, problem.New(
			401,
			"kubernetes_workload_identity_unauthenticated",
			"Kubernetes workload identity could not be authenticated.",
		)
	}
	if !kubernetesAudienceAccepted(review.Audiences, audience) {
		return VerifiedKubernetesWorkloadIdentity{}, problem.New(
			401,
			"kubernetes_workload_identity_audience_mismatch",
			"Kubernetes workload identity audience does not match this execution target.",
		)
	}
	expectedUsername := "system:serviceaccount:" + configuration.Namespace + ":" + configuration.ServiceAccountName
	if strings.TrimSpace(review.Username) != expectedUsername {
		return VerifiedKubernetesWorkloadIdentity{}, problem.New(
			401,
			"kubernetes_workload_identity_service_account_mismatch",
			"Kubernetes workload ServiceAccount does not match this execution target.",
		)
	}
	if !kubernetesExtraClaimMatches(review.Extra, kubernetesPodNameExtraKey, claims.PodName) ||
		!kubernetesExtraClaimMatches(review.Extra, kubernetesPodUIDExtraKey, claims.PodUID) {
		return VerifiedKubernetesWorkloadIdentity{}, problem.New(
			401,
			"kubernetes_workload_identity_pod_claim_mismatch",
			"Kubernetes workload Pod claims do not match the presented token.",
		)
	}
	pod, err := client.GetPod(ctx, configuration.Namespace, claims.PodName)
	if err != nil {
		var statusErr *kubernetesAPIStatusError
		if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound {
			return VerifiedKubernetesWorkloadIdentity{}, problem.New(
				401,
				"kubernetes_workload_identity_pod_missing",
				"Kubernetes workload Pod no longer exists.",
			)
		}
		return VerifiedKubernetesWorkloadIdentity{}, problem.Wrap(
			502,
			"kubernetes_workload_identity_pod_lookup_failed",
			"Kubernetes workload Pod could not be loaded.",
			err,
		)
	}
	if strings.TrimSpace(pod.UID) != claims.PodUID {
		return VerifiedKubernetesWorkloadIdentity{}, problem.New(
			401,
			"kubernetes_workload_identity_pod_uid_mismatch",
			"Kubernetes workload Pod UID does not match the presented token.",
		)
	}
	if pod.DeletionTimestamp != nil {
		return VerifiedKubernetesWorkloadIdentity{}, problem.New(
			409,
			"kubernetes_workload_identity_pod_terminating",
			"Kubernetes workload Pod is terminating and cannot register.",
		)
	}
	if pod.Labels[kubernetesManagedLabel] != "true" || pod.Labels[kubernetesTargetLabel] != targetID.String() {
		return VerifiedKubernetesWorkloadIdentity{}, problem.New(
			401,
			"kubernetes_workload_identity_target_mismatch",
			"Kubernetes workload Pod is not owned by this execution target.",
		)
	}
	if strings.TrimSpace(pod.ServiceAccountName) != configuration.ServiceAccountName {
		return VerifiedKubernetesWorkloadIdentity{}, problem.New(
			401,
			"kubernetes_workload_identity_service_account_mismatch",
			"Kubernetes workload ServiceAccount does not match this execution target.",
		)
	}
	runtimeCapabilities, runtimeObservationErr := client.RuntimeIsolationCapabilities(ctx, configuration, time.Now().UTC())
	gvisorCompatibilityErr := gvisorCompatibilityAccepted(ctx, s.db, target, configuration.RuntimeIsolation)
	runtimeDecision, runtimeDecisionErr := resolveKubernetesRuntimeIsolation(
		configuration,
		filterGVisorCapability(runtimeCapabilities, gvisorCompatibilityErr),
	)
	runtimeDecisionErr = preferGVisorCompatibilityError(
		&runtimeDecision, runtimeDecisionErr, gvisorCompatibilityErr,
	)
	if runtimeDecisionErr != nil {
		if runtimeObservationErr != nil {
			return VerifiedKubernetesWorkloadIdentity{}, runtimeObservationErr
		}
		return VerifiedKubernetesWorkloadIdentity{}, runtimeDecisionErr
	}
	configuration.RuntimeIsolationDecision = &runtimeDecision
	identity, err := kubernetesWorkerIdentityFromVerifiedPod(pod)
	if err != nil {
		return VerifiedKubernetesWorkloadIdentity{}, err
	}
	if err := s.verifyKubernetesRuntimeIsolationDecisionBinding(ctx, targetID, pod, identity, runtimeDecision); err != nil {
		return VerifiedKubernetesWorkloadIdentity{}, err
	}
	resourceRequests := pod.AgentdResourceRequests
	if configuration.AllocationBackend == string(kubernetesAllocationBackendSandboxOperatorCocoon) {
		if err := verifyKubernetesCocoonOuterSandbox(pod); err != nil {
			return VerifiedKubernetesWorkloadIdentity{}, err
		}
		node, err := client.GetNode(ctx, pod.Spec.NodeName)
		if err != nil {
			return VerifiedKubernetesWorkloadIdentity{}, problem.Wrap(
				401, "kubernetes_workload_identity_cocoon_node_invalid",
				"The Cocoon workload node attestation could not be loaded.", err,
			)
		}
		if !node.Ready || !kubernetesCocoonNodeMatchesSchedulingFence(node.Labels) ||
			!kubernetesCocoonSupervisorHeartbeatReady(node.Annotations, time.Now().UTC()) {
			return VerifiedKubernetesWorkloadIdentity{}, problem.New(
				401, "kubernetes_workload_identity_cocoon_node_invalid",
				"The Cocoon workload node does not have a live supervisor isolation attestation.",
			)
		}
		if err := attestCocoonHostPIDsLimit(pod.Spec.NodeName, node.Annotations, configuration.PIDsLimit); err != nil {
			return VerifiedKubernetesWorkloadIdentity{}, problem.Wrap(
				401, "kubernetes_workload_identity_pids_limit_invalid",
				"The Cocoon host agentd does not have the required finite PID confinement.", err,
			)
		}
		resourceRequests = pod.CocoonGuestResourceRequests
	} else {
		if err := verifyKubernetesPodOuterSandbox(targetID, pod, configuration.PIDsLimit, runtimeDecision); err != nil {
			return VerifiedKubernetesWorkloadIdentity{}, err
		}
		if err := client.attestNodePodPIDsLimit(ctx, pod.Spec.NodeName, configuration.PIDsLimit); err != nil {
			return VerifiedKubernetesWorkloadIdentity{}, problem.Wrap(
				401,
				"kubernetes_workload_identity_pids_limit_invalid",
				"The Kubernetes workload Pod is scheduled on a node without the required finite PID confinement.",
				err,
			)
		}
	}
	if err := s.verifyKubernetesSandboxAllocationIdentity(ctx, targetID, configuration, pod, identity); err != nil {
		return VerifiedKubernetesWorkloadIdentity{}, err
	}
	resources, err := kubernetesRequestedResourceSnapshot(resourceRequests)
	if err != nil {
		return VerifiedKubernetesWorkloadIdentity{}, problem.New(
			401,
			"kubernetes_workload_identity_resource_requests_invalid",
			"Kubernetes workload Pod resource requests are invalid.",
		)
	}
	return VerifiedKubernetesWorkloadIdentity{
		TargetID:                       targetID,
		Audience:                       audience,
		ClusterID:                      kubernetesLocalClusterID,
		Namespace:                      configuration.Namespace,
		PodName:                        claims.PodName,
		PodUID:                         claims.PodUID,
		WorkerMode:                     identity.WorkerMode,
		AssignedExecutionID:            identity.AssignedExecutionID,
		WorkerPoolID:                   identity.WorkerPoolID,
		WorkerPoolVersion:              identity.WorkerPoolVersion,
		CapacityClass:                  identity.CapacityClass,
		RequestedCPUMillicores:         resources.CPUMillicores,
		RequestedMemoryBytes:           resources.MemoryBytes,
		RequestedEphemeralStorageBytes: resources.EphemeralStorageBytes,
		ServiceAccountName:             configuration.ServiceAccountName,
		ServiceAccountUsername:         expectedUsername,
	}, nil
}

func (s *Service) verifyKubernetesRuntimeIsolationDecisionBinding(
	ctx context.Context,
	targetID uuid.UUID,
	pod kubernetesVerifiedPod,
	identity kubernetesVerifiedWorkerIdentity,
	current runtimeIsolationDecision,
) error {
	if current.EffectiveRuntime != runtimeIsolationGVisor || identity.AssignedExecutionID == nil {
		return nil
	}
	generation, err := strconv.ParseInt(strings.TrimSpace(pod.Labels[kubernetesGenerationLabel]), 10, 64)
	if err != nil || generation <= 0 {
		return problem.New(401, "gvisor_live_pod_runtime_invalid", "The gVisor workload Pod Generation identity is invalid.")
	}
	var persisted persistence.ExecutionRuntimeIsolationDecision
	err = s.db.WithContext(ctx).
		Where("execution_id = ? AND generation = ? AND execution_target_id = ?", *identity.AssignedExecutionID, generation, targetID).
		Take(&persisted).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return problem.New(401, "gvisor_live_pod_runtime_invalid", "The gVisor workload Pod has no immutable Generation runtime decision.")
	}
	if err != nil {
		return problem.Wrap(500, "runtime_isolation_decision_load_failed", "The immutable Generation runtime decision could not be loaded.", err)
	}
	if persisted.EffectiveRuntime == nil || *persisted.EffectiveRuntime != current.EffectiveRuntime ||
		persisted.EffectiveProfile == nil || *persisted.EffectiveProfile != string(current.EffectiveProfile) ||
		persisted.RuntimeClassName == nil || *persisted.RuntimeClassName != current.RuntimeClassName ||
		persisted.AttestationDigest == nil || *persisted.AttestationDigest != current.AttestationDigest ||
		persisted.Decision == "rejected" {
		return problem.New(401, "gvisor_live_pod_runtime_invalid", "The live gVisor runtime no longer matches the immutable Generation decision.")
	}
	return nil
}

func (s *Service) verifyKubernetesSandboxAllocationIdentity(
	ctx context.Context,
	targetID uuid.UUID,
	configuration kubernetesTargetConfiguration,
	pod kubernetesVerifiedPod,
	identity kubernetesVerifiedWorkerIdentity,
) error {
	if configuration.AllocationBackend == string(kubernetesAllocationBackendNativePod) {
		return nil
	}
	if identity.WorkerMode != kubernetesWorkerModeExecutionPinned || identity.AssignedExecutionID == nil {
		return problem.New(401, "kubernetes_sandbox_allocation_identity_missing", "Sandbox-backed workers must be pinned to an Execution allocation.")
	}
	generation, err := strconv.ParseInt(strings.TrimSpace(pod.Labels[kubernetesGenerationLabel]), 10, 64)
	if err != nil || generation <= 0 {
		return problem.New(401, "kubernetes_sandbox_allocation_generation_invalid", "Sandbox-backed worker Generation identity is missing or invalid.")
	}
	var latestGeneration int64
	if err := s.db.WithContext(ctx).Model(&persistence.ExecutionGenerationFact{}).
		Where("tenant_id = (SELECT tenant_id FROM agent_executions WHERE id = ?) AND execution_id = ?", *identity.AssignedExecutionID, *identity.AssignedExecutionID).
		Select("COALESCE(MAX(generation), 0)").Scan(&latestGeneration).Error; err != nil {
		return problem.Wrap(500, "kubernetes_sandbox_allocation_generation_load_failed", "Sandbox allocation Generation authority could not be loaded.", err)
	}
	if latestGeneration != generation {
		return problem.New(409, "kubernetes_sandbox_allocation_generation_stale", "Sandbox-backed worker belongs to a stale Execution Generation.")
	}
	var allocation persistence.ExecutionKubernetesAllocation
	if err := s.db.WithContext(ctx).Where(
		"execution_target_id = ? AND execution_id = ? AND generation = ? AND status = ?",
		targetID, *identity.AssignedExecutionID, generation, "bound",
	).Take(&allocation).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(409, "kubernetes_sandbox_allocation_not_bound", "Sandbox-backed worker allocation identity is not bound.")
		}
		return problem.Wrap(500, "kubernetes_sandbox_allocation_load_failed", "Sandbox allocation identity could not be loaded.", err)
	}
	if allocation.Namespace != configuration.Namespace ||
		!kubernetesOptionalIdentityMatches(allocation.PodName, pod.Name) ||
		!kubernetesOptionalIdentityMatches(allocation.PodUID, pod.UID) {
		return problem.New(401, "kubernetes_sandbox_allocation_pod_mismatch", "Sandbox-backed worker Pod does not match its bound allocation identity.")
	}
	return nil
}

type kubernetesVerifiedWorkerIdentity struct {
	WorkerMode          string
	AssignedExecutionID *uuid.UUID
	WorkerPoolID        *uuid.UUID
	WorkerPoolVersion   *int64
	CapacityClass       *string
}

func kubernetesWorkerIdentityFromVerifiedPod(pod kubernetesVerifiedPod) (kubernetesVerifiedWorkerIdentity, error) {
	mode := strings.TrimSpace(pod.Labels[kubernetesWorkerModeLabel])
	if mode == "" {
		executionID := strings.TrimSpace(pod.Labels[kubernetesExecutionLabel])
		if executionID == "" {
			return kubernetesVerifiedWorkerIdentity{}, problem.New(
				401,
				"kubernetes_workload_identity_worker_mode_missing",
				"Kubernetes workload Pod does not advertise an authoritative Worker mode.",
			)
		}
		parsedExecutionID, err := uuid.Parse(executionID)
		if err != nil {
			return kubernetesVerifiedWorkerIdentity{}, problem.New(
				401,
				"kubernetes_workload_identity_execution_label_invalid",
				"Kubernetes workload Pod execution label is invalid.",
			)
		}
		return kubernetesVerifiedWorkerIdentity{
			WorkerMode:          kubernetesWorkerModeExecutionPinned,
			AssignedExecutionID: &parsedExecutionID,
		}, nil
	}
	switch mode {
	case kubernetesWorkerModeExecutionPinned:
		executionID := strings.TrimSpace(pod.Labels[kubernetesExecutionLabel])
		if executionID == "" {
			return kubernetesVerifiedWorkerIdentity{}, problem.New(
				401,
				"kubernetes_workload_identity_execution_label_missing",
				"Kubernetes execution-pinned Pod omitted its assigned execution label.",
			)
		}
		parsedExecutionID, err := uuid.Parse(executionID)
		if err != nil {
			return kubernetesVerifiedWorkerIdentity{}, problem.New(
				401,
				"kubernetes_workload_identity_execution_label_invalid",
				"Kubernetes workload Pod execution label is invalid.",
			)
		}
		return kubernetesVerifiedWorkerIdentity{
			WorkerMode:          mode,
			AssignedExecutionID: &parsedExecutionID,
		}, nil
	case kubernetesWorkerModeWarmPool:
		poolID := strings.TrimSpace(pod.Labels[kubernetesWorkerPoolIDLabel])
		if poolID == "" {
			return kubernetesVerifiedWorkerIdentity{}, problem.New(
				401,
				"kubernetes_workload_identity_worker_pool_missing",
				"Kubernetes warm-pool Pod omitted its authoritative Worker pool identity.",
			)
		}
		parsedPoolID, err := uuid.Parse(poolID)
		if err != nil {
			return kubernetesVerifiedWorkerIdentity{}, problem.New(
				401,
				"kubernetes_workload_identity_worker_pool_invalid",
				"Kubernetes warm-pool Pod Worker pool ID is invalid.",
			)
		}
		versionText := strings.TrimSpace(pod.Labels[kubernetesWorkerPoolVersionLabel])
		version, err := strconv.ParseInt(versionText, 10, 64)
		if err != nil || version <= 0 {
			return kubernetesVerifiedWorkerIdentity{}, problem.New(
				401,
				"kubernetes_workload_identity_worker_pool_version_invalid",
				"Kubernetes warm-pool Pod Worker pool version is invalid.",
			)
		}
		capacityClass := strings.ToLower(strings.TrimSpace(pod.Labels[kubernetesCapacityClassLabel]))
		switch capacityClass {
		case "standard", "interactive":
		default:
			return kubernetesVerifiedWorkerIdentity{}, problem.New(
				401,
				"kubernetes_workload_identity_capacity_class_invalid",
				"Kubernetes warm-pool Pod capacity class is invalid.",
			)
		}
		return kubernetesVerifiedWorkerIdentity{
			WorkerMode:        mode,
			WorkerPoolID:      &parsedPoolID,
			WorkerPoolVersion: &version,
			CapacityClass:     &capacityClass,
		}, nil
	case kubernetesWorkerModeGeneralPool:
		return kubernetesVerifiedWorkerIdentity{WorkerMode: mode}, nil
	default:
		return kubernetesVerifiedWorkerIdentity{}, problem.New(
			401,
			"kubernetes_workload_identity_worker_mode_invalid",
			"Kubernetes workload Pod Worker mode label is invalid.",
		)
	}
}

func normalizeKubernetesWorkloadIdentityInput(
	targetID uuid.UUID,
	claims KubernetesWorkloadIdentityClaims,
	bearerToken string,
) (KubernetesWorkloadIdentityClaims, string, error) {
	claims.Namespace = strings.TrimSpace(claims.Namespace)
	claims.PodName = strings.TrimSpace(claims.PodName)
	claims.PodUID = strings.TrimSpace(claims.PodUID)
	bearerToken = strings.TrimSpace(bearerToken)
	if targetID == uuid.Nil || claims.Namespace == "" || claims.PodName == "" || claims.PodUID == "" || bearerToken == "" {
		return KubernetesWorkloadIdentityClaims{}, "", problem.New(
			400,
			"invalid_kubernetes_workload_identity",
			"Kubernetes workload identity claims are incomplete.",
		)
	}
	return claims, bearerToken, nil
}

func (s *Service) loadKubernetesWorkloadIdentityTarget(
	ctx context.Context,
	targetID uuid.UUID,
) (persistence.ExecutionTarget, error) {
	var model persistence.ExecutionTarget
	err := s.db.WithContext(ctx).Where("id = ?", targetID).Take(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.ExecutionTarget{}, problem.New(404, "execution_target_not_found", "Execution target not found.")
	}
	if err != nil {
		return persistence.ExecutionTarget{}, problem.Wrap(
			500,
			"execution_target_lookup_failed",
			"Failed to load the execution target.",
			err,
		)
	}
	if model.Kind != "kubernetes" || model.Status == "disabled" {
		return persistence.ExecutionTarget{}, problem.New(404, "execution_target_not_found", "Execution target not found.")
	}
	return model, nil
}

func (s *Service) loadKubernetesWorkloadIdentityConfiguration(
	target persistence.ExecutionTarget,
) (kubernetesTargetConfiguration, error) {
	if target.TenantID == nil {
		return kubernetesTargetConfiguration{}, problem.New(
			409,
			"kubernetes_target_tenant_required",
			"Managed Kubernetes targets must belong to a Tenant.",
		)
	}
	if len(target.ConfigurationEncrypted) == 0 || s.cipher == nil {
		return kubernetesTargetConfiguration{}, problem.New(
			409,
			"kubernetes_configuration_missing",
			"Kubernetes execution target configuration is missing.",
		)
	}
	decoded, err := s.cipher.Decrypt(target.ConfigurationEncrypted)
	if err != nil {
		return kubernetesTargetConfiguration{}, problem.Wrap(
			503,
			"kubernetes_configuration_unavailable",
			"Kubernetes execution target configuration could not be decrypted.",
			err,
		)
	}
	var configuration kubernetesTargetConfiguration
	decoder := json.NewDecoder(strings.NewReader(decoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&configuration); err != nil {
		return kubernetesTargetConfiguration{}, problem.New(
			400,
			"invalid_kubernetes_configuration",
			"Kubernetes execution target configuration is invalid.",
		)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return kubernetesTargetConfiguration{}, problem.New(
			400,
			"invalid_kubernetes_configuration",
			"Kubernetes execution target configuration is invalid.",
		)
	}
	return normalizeKubernetesWorkloadIdentityConfiguration(target, configuration)
}

func normalizeKubernetesWorkloadIdentityConfiguration(
	target persistence.ExecutionTarget,
	configuration kubernetesTargetConfiguration,
) (kubernetesTargetConfiguration, error) {
	configuration.APIServer = strings.TrimRight(strings.TrimSpace(configuration.APIServer), "/")
	if configuration.APIServer == "" {
		configuration.APIServer = "https://kubernetes.default.svc"
	}
	configuration.BearerToken = strings.TrimSpace(configuration.BearerToken)
	configuration.BearerTokenFile = strings.TrimSpace(configuration.BearerTokenFile)
	if configuration.BearerToken == "" && configuration.BearerTokenFile == "" {
		configuration.BearerTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	}
	configuration.CACertificate = strings.TrimSpace(configuration.CACertificate)
	configuration.CAFile = strings.TrimSpace(configuration.CAFile)
	if configuration.CACertificate == "" && configuration.CAFile == "" {
		configuration.CAFile = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	}
	for _, path := range []string{configuration.BearerTokenFile, configuration.CAFile} {
		if path != "" && !strings.HasPrefix(path, "/var/run/secrets/kubernetes.io/serviceaccount/") {
			return kubernetesTargetConfiguration{}, problem.New(
				400,
				"invalid_kubernetes_configuration",
				"Kubernetes token and CA files must use the in-cluster ServiceAccount path; use inline encrypted values for external clusters.",
			)
		}
	}
	configuration.Namespace = strings.TrimSpace(configuration.Namespace)
	if configuration.Namespace == "" {
		configuration.Namespace = "synara-" + strings.ReplaceAll(target.ID.String(), "-", "")[:12]
	}
	configuration.ServiceAccountName = strings.TrimSpace(configuration.ServiceAccountName)
	if configuration.ServiceAccountName == "" {
		configuration.ServiceAccountName = kubernetesSecretName(target.ID)
	}
	if !kubernetesNamePattern.MatchString(configuration.Namespace) || len(configuration.Namespace) > 63 ||
		!kubernetesNamePattern.MatchString(configuration.ServiceAccountName) || len(configuration.ServiceAccountName) > 63 {
		return kubernetesTargetConfiguration{}, problem.New(
			400,
			"invalid_kubernetes_configuration",
			"Kubernetes namespace or serviceAccountName is invalid.",
		)
	}
	apiURL, err := url.Parse(configuration.APIServer)
	if err != nil || apiURL.Scheme != "https" || apiURL.Host == "" {
		return kubernetesTargetConfiguration{}, problem.New(
			400,
			"invalid_kubernetes_configuration",
			"Kubernetes apiServer must be an HTTPS origin.",
		)
	}
	if err := normalizeKubernetesAllocationConfiguration(&configuration); err != nil {
		return kubernetesTargetConfiguration{}, err
	}
	if err := normalizeKubernetesRuntimeIsolationConfiguration(&configuration); err != nil {
		return kubernetesTargetConfiguration{}, err
	}
	return configuration, nil
}

func newKubernetesHTTPClient(configuration kubernetesTargetConfiguration) (*kubernetesHTTPClient, error) {
	token := strings.TrimSpace(configuration.BearerToken)
	if token == "" {
		encoded, err := os.ReadFile(configuration.BearerTokenFile)
		if err != nil {
			return nil, err
		}
		token = strings.TrimSpace(string(encoded))
	}
	if token == "" {
		return nil, errors.New("Kubernetes bearer token is empty")
	}
	rootCAs, err := x509.SystemCertPool()
	if err != nil || rootCAs == nil {
		rootCAs = x509.NewCertPool()
	}
	caCertificate := []byte(strings.TrimSpace(configuration.CACertificate))
	if len(caCertificate) == 0 && configuration.CAFile != "" {
		caCertificate, err = os.ReadFile(configuration.CAFile)
		if err != nil {
			return nil, err
		}
	}
	if len(caCertificate) > 0 && !rootCAs.AppendCertsFromPEM(caCertificate) {
		return nil, errors.New("Kubernetes CA certificate is invalid")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: rootCAs}}
	return &kubernetesHTTPClient{
		baseURL: configuration.APIServer,
		token:   token,
		client:  &http.Client{Transport: transport, Timeout: 30 * time.Second},
	}, nil
}

type kubernetesTokenReviewStatus struct {
	Authenticated bool
	Username      string
	Extra         map[string][]string
	Audiences     []string
}

type kubernetesVerifiedPod struct {
	Name                        string
	UID                         string
	DeletionTimestamp           *time.Time
	Labels                      map[string]string
	Annotations                 map[string]string
	ServiceAccountName          string
	AgentdResourceRequests      map[string]string
	CocoonGuestResourceRequests map[string]string
	Spec                        kubernetesVerifiedPodSpec
}

type kubernetesVerifiedPodSpec struct {
	NodeName                     string                               `json:"nodeName"`
	RuntimeClassName             string                               `json:"runtimeClassName"`
	AutomountServiceAccountToken *bool                                `json:"automountServiceAccountToken"`
	HostNetwork                  bool                                 `json:"hostNetwork"`
	HostPID                      bool                                 `json:"hostPID"`
	HostIPC                      bool                                 `json:"hostIPC"`
	ShareProcessNamespace        *bool                                `json:"shareProcessNamespace"`
	ServiceAccountName           string                               `json:"serviceAccountName"`
	SecurityContext              kubernetesVerifiedPodSecurityContext `json:"securityContext"`
	Containers                   []kubernetesVerifiedContainer        `json:"containers"`
	InitContainers               []kubernetesVerifiedContainer        `json:"initContainers"`
	EphemeralContainers          []kubernetesVerifiedContainer        `json:"ephemeralContainers"`
	Volumes                      []map[string]json.RawMessage         `json:"volumes"`
}

type kubernetesVerifiedPodSecurityContext struct {
	RunAsNonRoot   *bool                            `json:"runAsNonRoot"`
	FSGroup        *int64                           `json:"fsGroup"`
	SeccompProfile kubernetesVerifiedSeccompProfile `json:"seccompProfile"`
	Sysctls        []map[string]any                 `json:"sysctls"`
}

type kubernetesVerifiedSeccompProfile struct {
	Type string `json:"type"`
}

type kubernetesVerifiedContainer struct {
	Name            string   `json:"name"`
	Image           string   `json:"image"`
	ImagePullPolicy string   `json:"imagePullPolicy"`
	Command         []string `json:"command"`
	SecurityContext struct {
		AllowPrivilegeEscalation *bool                            `json:"allowPrivilegeEscalation"`
		Privileged               *bool                            `json:"privileged"`
		ReadOnlyRootFilesystem   *bool                            `json:"readOnlyRootFilesystem"`
		RunAsNonRoot             *bool                            `json:"runAsNonRoot"`
		RunAsUser                *int64                           `json:"runAsUser"`
		RunAsGroup               *int64                           `json:"runAsGroup"`
		Capabilities             kubernetesVerifiedCapabilities   `json:"capabilities"`
		SeccompProfile           kubernetesVerifiedSeccompProfile `json:"seccompProfile"`
	} `json:"securityContext"`
	Resources struct {
		Requests map[string]string `json:"requests"`
		Limits   map[string]string `json:"limits"`
	} `json:"resources"`
	Environment  []kubernetesVerifiedEnvironmentVariable `json:"env"`
	VolumeMounts []struct {
		Name      string `json:"name"`
		MountPath string `json:"mountPath"`
		ReadOnly  bool   `json:"readOnly"`
		SubPath   string `json:"subPath"`
	} `json:"volumeMounts"`
	Ports []struct {
		HostPort int32 `json:"hostPort"`
	} `json:"ports"`
	VolumeDevices []map[string]any `json:"volumeDevices"`
}

type kubernetesVerifiedEnvironmentVariable struct {
	Name      string `json:"name"`
	Value     string `json:"value"`
	ValueFrom struct {
		ConfigMapKeyRef *struct {
			Name     string `json:"name"`
			Key      string `json:"key"`
			Optional *bool  `json:"optional"`
		} `json:"configMapKeyRef"`
	} `json:"valueFrom"`
}

type kubernetesVerifiedCapabilities struct {
	Add  []string `json:"add"`
	Drop []string `json:"drop"`
}

func (c *kubernetesHTTPClient) ReviewServiceAccountToken(
	ctx context.Context,
	bearerToken, audience string,
) (kubernetesTokenReviewStatus, error) {
	var response struct {
		Status struct {
			Authenticated bool `json:"authenticated"`
			User          struct {
				Username string              `json:"username"`
				Extra    map[string][]string `json:"extra"`
			} `json:"user"`
			Audiences []string `json:"audiences"`
		} `json:"status"`
	}
	err := c.requestJSON(ctx, http.MethodPost, "/apis/authentication.k8s.io/v1/tokenreviews", map[string]any{
		"apiVersion": "authentication.k8s.io/v1",
		"kind":       "TokenReview",
		"spec": map[string]any{
			"token":     bearerToken,
			"audiences": []string{audience},
		},
	}, &response, http.StatusOK, http.StatusCreated)
	if err != nil {
		return kubernetesTokenReviewStatus{}, err
	}
	return kubernetesTokenReviewStatus{
		Authenticated: response.Status.Authenticated,
		Username:      response.Status.User.Username,
		Extra:         response.Status.User.Extra,
		Audiences:     response.Status.Audiences,
	}, nil
}

func (c *kubernetesHTTPClient) GetPod(
	ctx context.Context,
	namespace, podName string,
) (kubernetesVerifiedPod, error) {
	var response struct {
		Metadata struct {
			UID               string            `json:"uid"`
			DeletionTimestamp *time.Time        `json:"deletionTimestamp"`
			Labels            map[string]string `json:"labels"`
			Annotations       map[string]string `json:"annotations"`
		} `json:"metadata"`
		Spec kubernetesVerifiedPodSpec `json:"spec"`
	}
	err := c.requestJSON(
		ctx,
		http.MethodGet,
		kubernetesNamespacedPath(namespace, "pods", podName),
		nil,
		&response,
		http.StatusOK,
	)
	if err != nil {
		return kubernetesVerifiedPod{}, err
	}
	var agentdRequests map[string]string
	var cocoonGuestRequests map[string]string
	for _, container := range response.Spec.Containers {
		if strings.TrimSpace(container.Name) == "agentd" {
			agentdRequests = container.Resources.Requests
			break
		}
		if strings.TrimSpace(container.Name) == kubernetesCocoonGuestContainerName {
			cocoonGuestRequests = container.Resources.Requests
		}
	}
	return kubernetesVerifiedPod{
		Name:                        strings.TrimSpace(podName),
		UID:                         strings.TrimSpace(response.Metadata.UID),
		DeletionTimestamp:           response.Metadata.DeletionTimestamp,
		Labels:                      response.Metadata.Labels,
		Annotations:                 response.Metadata.Annotations,
		ServiceAccountName:          strings.TrimSpace(response.Spec.ServiceAccountName),
		AgentdResourceRequests:      agentdRequests,
		CocoonGuestResourceRequests: cocoonGuestRequests,
		Spec:                        response.Spec,
	}, nil
}

type kubernetesVerifiedNode struct {
	Labels      map[string]string
	Annotations map[string]string
	Ready       bool
}

func (c *kubernetesHTTPClient) GetNode(ctx context.Context, name string) (kubernetesVerifiedNode, error) {
	var response struct {
		Metadata struct {
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := c.requestJSON(ctx, http.MethodGet, "/api/v1/nodes/"+url.PathEscape(strings.TrimSpace(name)), nil, &response, http.StatusOK); err != nil {
		return kubernetesVerifiedNode{}, err
	}
	result := kubernetesVerifiedNode{Labels: response.Metadata.Labels, Annotations: response.Metadata.Annotations}
	for _, condition := range response.Status.Conditions {
		if condition.Type == "Ready" && condition.Status == "True" {
			result.Ready = true
			break
		}
	}
	return result, nil
}

func verifyKubernetesCocoonOuterSandbox(pod kubernetesVerifiedPod) error {
	invalid := func() error {
		return problem.New(
			401, "kubernetes_workload_identity_outer_sandbox_invalid",
			"Kubernetes workload Pod does not satisfy the required Cocoon microVM outer sandbox profile.",
		)
	}
	spec := pod.Spec
	if strings.TrimSpace(spec.NodeName) == "" || spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken ||
		spec.HostNetwork || spec.HostPID || spec.HostIPC || (spec.ShareProcessNamespace != nil && *spec.ShareProcessNamespace) ||
		len(spec.InitContainers) != 0 || len(spec.EphemeralContainers) != 0 || len(spec.Containers) != 1 {
		return invalid()
	}
	container := spec.Containers[0]
	if strings.TrimSpace(container.Name) != kubernetesCocoonGuestContainerName ||
		!strings.Contains(strings.TrimSpace(container.Image), "@sha256:") ||
		!strings.EqualFold(strings.TrimSpace(pod.Annotations[kubernetesCocoonSharedMemoryAnnotation]), "true") ||
		strings.TrimSpace(pod.Annotations[kubernetesCocoonSnapshotPolicyAnnotation]) != kubernetesCocoonEphemeralPolicy ||
		strings.TrimSpace(pod.Annotations["vm.cocoonstack.io/id"]) == "" {
		return invalid()
	}
	return nil
}

func verifyKubernetesPodOuterSandbox(
	targetID uuid.UUID,
	pod kubernetesVerifiedPod,
	expectedPIDsLimit uint64,
	runtimeDecision runtimeIsolationDecision,
) error {
	invalid := func() error {
		return problem.New(
			401,
			"kubernetes_workload_identity_outer_sandbox_invalid",
			"Kubernetes workload Pod does not satisfy the required Provider outer sandbox profile.",
		)
	}
	spec := pod.Spec
	expectedRuntimeClassName := ""
	if runtimeDecision.EffectiveRuntime == runtimeIsolationGVisor {
		expectedRuntimeClassName = runtimeDecision.RuntimeClassName
	}
	if strings.TrimSpace(spec.NodeName) == "" ||
		strings.TrimSpace(spec.RuntimeClassName) != expectedRuntimeClassName ||
		(runtimeDecision.EffectiveRuntime == runtimeIsolationGVisor &&
			!slices.Contains(runtimeDecision.EligibleNodeNames, strings.TrimSpace(spec.NodeName))) ||
		spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken ||
		spec.HostNetwork || spec.HostPID || spec.HostIPC ||
		(spec.ShareProcessNamespace != nil && *spec.ShareProcessNamespace) ||
		len(spec.InitContainers) != 2 || len(spec.EphemeralContainers) != 0 || len(spec.Containers) != 1 {
		return invalid()
	}
	podSecurity := spec.SecurityContext
	if podSecurity.RunAsNonRoot == nil || !*podSecurity.RunAsNonRoot ||
		podSecurity.FSGroup == nil || *podSecurity.FSGroup != 10001 ||
		strings.TrimSpace(podSecurity.SeccompProfile.Type) != "RuntimeDefault" || len(podSecurity.Sysctls) != 0 {
		return invalid()
	}
	container := spec.Containers[0]
	networkBoundaryInit := spec.InitContainers[0]
	registrationTokenInit := spec.InitContainers[1]
	if strings.TrimSpace(container.Name) != "agentd" ||
		!kubernetesRestrictedContainerValid(container) ||
		strings.TrimSpace(networkBoundaryInit.Name) != kubernetesNetworkBoundaryInitName ||
		!kubernetesRestrictedContainerValid(networkBoundaryInit) ||
		strings.TrimSpace(registrationTokenInit.Name) != kubernetesRegistrationTokenInitName ||
		!kubernetesRestrictedContainerValid(registrationTokenInit) ||
		strings.TrimSpace(container.Image) == "" ||
		container.Image != networkBoundaryInit.Image || container.Image != registrationTokenInit.Image ||
		container.ImagePullPolicy != networkBoundaryInit.ImagePullPolicy ||
		container.ImagePullPolicy != registrationTokenInit.ImagePullPolicy ||
		len(networkBoundaryInit.Command) != 2 || networkBoundaryInit.Command[0] != "/usr/local/bin/synara-agentd" ||
		networkBoundaryInit.Command[1] != platform.KubernetesNetworkBoundaryVerifyArgument ||
		len(networkBoundaryInit.Environment) != 0 || len(networkBoundaryInit.VolumeMounts) != 0 ||
		len(registrationTokenInit.Command) != 2 || registrationTokenInit.Command[0] != "/usr/local/bin/synara-agentd" ||
		registrationTokenInit.Command[1] != platform.KubernetesRegistrationTokenStageArgument ||
		len(registrationTokenInit.Environment) != 0 {
		return invalid()
	}
	limits, err := kubernetesRequestedResourceSnapshot(container.Resources.Limits)
	if err != nil || limits.CPUMillicores == nil || limits.MemoryBytes == nil || limits.EphemeralStorageBytes == nil {
		return invalid()
	}
	networkBoundaryLimits, err := kubernetesRequestedResourceSnapshot(networkBoundaryInit.Resources.Limits)
	if err != nil || networkBoundaryLimits.CPUMillicores == nil || networkBoundaryLimits.MemoryBytes == nil || networkBoundaryLimits.EphemeralStorageBytes == nil {
		return invalid()
	}
	registrationTokenLimits, err := kubernetesRequestedResourceSnapshot(registrationTokenInit.Resources.Limits)
	if err != nil || registrationTokenLimits.CPUMillicores == nil || registrationTokenLimits.MemoryBytes == nil || registrationTokenLimits.EphemeralStorageBytes == nil {
		return invalid()
	}
	if !kubernetesOuterSandboxEnvironmentValid(
		container.Environment,
		targetID,
		expectedPIDsLimit,
		runtimeDecision.EffectiveProfile,
	) ||
		!kubernetesOuterSandboxVolumesValid(
			targetID,
			spec.Volumes,
			container.VolumeMounts,
			registrationTokenInit.VolumeMounts,
		) {
		return invalid()
	}
	return nil
}

func kubernetesRestrictedContainerValid(container kubernetesVerifiedContainer) bool {
	security := container.SecurityContext
	if security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation ||
		(security.Privileged != nil && *security.Privileged) ||
		security.ReadOnlyRootFilesystem == nil || !*security.ReadOnlyRootFilesystem ||
		security.RunAsNonRoot == nil || !*security.RunAsNonRoot ||
		security.RunAsUser == nil || *security.RunAsUser != 10001 ||
		security.RunAsGroup == nil || *security.RunAsGroup != 10001 ||
		strings.TrimSpace(security.SeccompProfile.Type) != "RuntimeDefault" ||
		len(security.Capabilities.Add) != 0 || !containsFold(security.Capabilities.Drop, "ALL") ||
		len(container.VolumeDevices) != 0 {
		return false
	}
	for _, port := range container.Ports {
		if port.HostPort != 0 {
			return false
		}
	}
	return true
}

func containsFold(values []string, expected string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), expected) {
			return true
		}
	}
	return false
}

func kubernetesOuterSandboxEnvironmentValid(
	environment []kubernetesVerifiedEnvironmentVariable,
	targetID uuid.UUID,
	expectedPIDsLimit uint64,
	expectedProfile platform.ExecutionTargetIsolationProfile,
) bool {
	want := map[string]string{
		"SYNARA_EXECUTION_TARGET_KIND":                        "kubernetes",
		"SYNARA_WORKER_REGISTRATION_TOKEN_FILE":               kubernetesStagedRegistrationTokenPath,
		"SYNARA_AGENTD_PROVIDER_HOST_PROTOCOL":                "v2",
		"SYNARA_AGENTD_PRIVATE_TMP_ROOT":                      "/tmp",
		platform.KubernetesPIDsLimitEnvironment:               strconv.FormatUint(expectedPIDsLimit, 10),
		platform.KubernetesRuntimeIsolationProfileEnvironment: string(expectedProfile),
	}
	wantConfigMap := map[string]struct{}{
		"OTEL_EXPORTER_OTLP_ENDPOINT":      {},
		"OTEL_EXPORTER_OTLP_PROTOCOL":      {},
		"OTEL_EXPORTER_OTLP_CERTIFICATE":   {},
		"SYNARA_OTEL_TRACE_SAMPLE_RATIO":   {},
		"SYNARA_OTEL_COLLECTOR_REGION":     {},
		"SYNARA_OTEL_TRACE_RETENTION_DAYS": {},
	}
	seen := make(map[string]struct{}, len(want))
	seenConfigMap := make(map[string]struct{}, len(wantConfigMap))
	for _, item := range environment {
		name := strings.TrimSpace(item.Name)
		if _, forbidden := kubernetesForbiddenWorkerObservabilityEnvironment[name]; forbidden {
			return false
		}
		if expected, required := want[name]; required {
			if _, duplicate := seen[name]; duplicate || strings.TrimSpace(item.Value) != expected ||
				item.ValueFrom.ConfigMapKeyRef != nil {
				return false
			}
			seen[name] = struct{}{}
			continue
		}
		if _, required := wantConfigMap[name]; required {
			ref := item.ValueFrom.ConfigMapKeyRef
			if _, duplicate := seenConfigMap[name]; duplicate || strings.TrimSpace(item.Value) != "" || ref == nil ||
				strings.TrimSpace(ref.Name) != kubernetesObservabilityConfigMapName(targetID) || strings.TrimSpace(ref.Key) != name ||
				ref.Optional == nil || !*ref.Optional {
				return false
			}
			seenConfigMap[name] = struct{}{}
		}
	}
	return expectedPIDsLimit > 0 && expectedPIDsLimit <= platform.MaximumKubernetesPIDsLimit &&
		len(seen) == len(want) && len(seenConfigMap) == len(wantConfigMap)
}

func kubernetesOuterSandboxVolumesValid(
	targetID uuid.UUID,
	volumes []map[string]json.RawMessage,
	mainMounts []struct {
		Name      string `json:"name"`
		MountPath string `json:"mountPath"`
		ReadOnly  bool   `json:"readOnly"`
		SubPath   string `json:"subPath"`
	},
	initMounts []struct {
		Name      string `json:"name"`
		MountPath string `json:"mountPath"`
		ReadOnly  bool   `json:"readOnly"`
		SubPath   string `json:"subPath"`
	},
) bool {
	requiredMainMounts := map[string]struct {
		path     string
		readOnly bool
	}{
		"workspace":                       {path: "/data"},
		"tmp":                             {path: "/tmp"},
		"home":                            {path: "/home/synara"},
		kubernetesRegistrationTokenVolume: {path: "/var/run/secrets/synara.io/registration"},
	}
	requiredInitMounts := map[string]struct {
		path     string
		readOnly bool
	}{
		kubernetesWorkloadIdentityVolume: {
			path: "/var/run/secrets/synara.io/workload-identity", readOnly: true,
		},
		kubernetesRegistrationTokenVolume: {path: "/var/run/secrets/synara.io/registration"},
	}
	seenVolumes := make(map[string]struct{}, len(volumes))
	for _, volume := range volumes {
		var name string
		if err := json.Unmarshal(volume["name"], &name); err != nil || strings.TrimSpace(name) == "" {
			return false
		}
		name = strings.TrimSpace(name)
		if _, duplicate := seenVolumes[name]; duplicate {
			return false
		}
		seenVolumes[name] = struct{}{}
		sources := make([]string, 0, len(volume)-1)
		for key := range volume {
			if key != "name" {
				sources = append(sources, key)
			}
		}
		if len(sources) != 1 {
			return false
		}
		switch name {
		case "workspace", "tmp", "home", kubernetesRegistrationTokenVolume:
			if sources[0] != "emptyDir" {
				return false
			}
		case kubernetesWorkloadIdentityVolume:
			if sources[0] != "projected" || !kubernetesWorkloadIdentityProjectionValid(volume["projected"], targetID) {
				return false
			}
		case "git-cache":
			if sources[0] != "persistentVolumeClaim" {
				return false
			}
			requiredMainMounts[name] = struct {
				path     string
				readOnly bool
			}{path: "/git-cache"}
		default:
			return false
		}
	}
	for _, required := range []string{
		"workspace",
		"tmp",
		"home",
		kubernetesWorkloadIdentityVolume,
		kubernetesRegistrationTokenVolume,
	} {
		if _, found := seenVolumes[required]; !found {
			return false
		}
	}
	seenMounts := make(map[string]struct{}, len(mainMounts))
	for _, mount := range mainMounts {
		name := strings.TrimSpace(mount.Name)
		expected, allowed := requiredMainMounts[name]
		if !allowed || strings.TrimSpace(mount.MountPath) != expected.path || mount.ReadOnly != expected.readOnly ||
			strings.TrimSpace(mount.SubPath) != "" {
			return false
		}
		if _, duplicate := seenMounts[name]; duplicate {
			return false
		}
		seenMounts[name] = struct{}{}
	}
	if len(seenMounts) != len(requiredMainMounts) {
		return false
	}
	seenInitMounts := make(map[string]struct{}, len(initMounts))
	for _, mount := range initMounts {
		name := strings.TrimSpace(mount.Name)
		expected, allowed := requiredInitMounts[name]
		if !allowed || strings.TrimSpace(mount.MountPath) != expected.path || mount.ReadOnly != expected.readOnly ||
			strings.TrimSpace(mount.SubPath) != "" {
			return false
		}
		if _, duplicate := seenInitMounts[name]; duplicate {
			return false
		}
		seenInitMounts[name] = struct{}{}
	}
	return len(seenInitMounts) == len(requiredInitMounts)
}

func kubernetesWorkloadIdentityProjectionValid(raw json.RawMessage, targetID uuid.UUID) bool {
	var projected struct {
		DefaultMode int32 `json:"defaultMode"`
		Sources     []struct {
			ServiceAccountToken *struct {
				Audience          string `json:"audience"`
				ExpirationSeconds int64  `json:"expirationSeconds"`
				Path              string `json:"path"`
			} `json:"serviceAccountToken"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(raw, &projected); err != nil || projected.DefaultMode != 0o440 || len(projected.Sources) != 1 ||
		projected.Sources[0].ServiceAccountToken == nil {
		return false
	}
	token := projected.Sources[0].ServiceAccountToken
	return strings.TrimSpace(token.Audience) == KubernetesWorkerRegistrationAudience(targetID) &&
		token.ExpirationSeconds == 600 && strings.TrimSpace(token.Path) == "token"
}

type kubernetesAPIStatusError struct {
	StatusCode int
	Detail     string
}

func (e *kubernetesAPIStatusError) Error() string {
	return fmt.Sprintf("Kubernetes API returned status %d: %s", e.StatusCode, e.Detail)
}

func (c *kubernetesHTTPClient) requestJSON(
	ctx context.Context,
	method, path string,
	input, output any,
	accepted ...int,
) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	acceptedStatus := false
	for _, status := range accepted {
		if response.StatusCode == status {
			acceptedStatus = true
			break
		}
	}
	if !acceptedStatus {
		var status struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		}
		_ = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&status)
		detail := strings.TrimSpace(status.Reason)
		if message := strings.TrimSpace(status.Message); message != "" {
			if detail != "" {
				detail += ": "
			}
			detail += message
		}
		if detail == "" {
			detail = http.StatusText(response.StatusCode)
		}
		return &kubernetesAPIStatusError{StatusCode: response.StatusCode, Detail: detail}
	}
	if output == nil {
		_, _ = io.Copy(io.Discard, response.Body)
		return nil
	}
	return json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(output)
}

func kubernetesAudienceAccepted(audiences []string, expected string) bool {
	for _, audience := range audiences {
		if strings.TrimSpace(audience) == expected {
			return true
		}
	}
	return false
}

func kubernetesExtraClaimMatches(extra map[string][]string, key, expected string) bool {
	values := extra[key]
	if len(values) != 1 {
		return false
	}
	return strings.TrimSpace(values[0]) == expected
}
