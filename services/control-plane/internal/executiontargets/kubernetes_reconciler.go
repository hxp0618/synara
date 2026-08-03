package executiontargets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/executionqueue"
	"github.com/synara-ai/synara/services/control-plane/internal/fairqueue"
	"github.com/synara-ai/synara/services/control-plane/internal/gitpolicy"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/placement"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/podlifecycle"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/providerproxy"
	"github.com/synara-ai/synara/services/control-plane/internal/routing"
)

const (
	kubernetesManagedLabel                 = "synara.io/managed"
	kubernetesTargetLabel                  = "synara.io/execution-target-id"
	kubernetesExecutionLabel               = "synara.io/execution-id"
	kubernetesGenerationLabel              = "synara.io/generation"
	kubernetesReleaseLabel                 = "synara.io/worker-release-revision-id"
	kubernetesChannelLabel                 = "synara.io/worker-release-channel"
	kubernetesWarmSlotLabel                = "synara.io/warm-slot"
	kubernetesConfigAnnotation             = "synara.io/config-sha256"
	kubernetesWorkloadIdentityVolume       = "workload-identity"
	kubernetesRegistrationTokenVolume      = "registration-token"
	kubernetesObservabilityConfigMapPrefix = "synara-agentd-observability-config-"
	kubernetesWorkloadIdentityTokenPath    = platform.KubernetesWorkloadIdentityTokenPath
	kubernetesStagedRegistrationTokenPath  = platform.KubernetesStagedRegistrationTokenPath
	kubernetesNetworkBoundaryInitName      = "network-boundary-init"
	kubernetesRegistrationTokenInitName    = "registration-token-init"
	kubernetesLocalClusterID               = "kubernetes"
	kubernetesWorkerProtocolVersion        = 2
	kubernetesPodBoundRegistrationTrust    = "kubernetes-pod-bound-v1"

	KubernetesPodFailureApplyFailed    = "pod-apply-failed"
	KubernetesPodFailurePendingTimeout = "pending-timeout"
	KubernetesPodFailureUnschedulable  = "unschedulable"
	KubernetesPodFailureImagePull      = "image-pull"
	KubernetesPodFailureContainerStart = "container-start"
	KubernetesPodFailureEvicted        = "evicted"
	KubernetesPodFailureOOMKilled      = "oom-killed"
	KubernetesPodFailureGeneric        = "pod-failed"
)

var errKubernetesPodUIDPreconditionFailed = errors.New("Kubernetes Pod UID precondition failed")

type KubernetesWorkerPodObservation struct {
	ExecutionTargetID uuid.UUID
	Namespace         string
	PodName           string
	PodUID            string
	Phase             string
	Reason            string
	ObservedAt        time.Time
}

type KubernetesWorkerPodObserver func(context.Context, KubernetesWorkerPodObservation) error

type KubernetesExecutionPodObservation struct {
	TenantID                uuid.UUID
	ExecutionTargetID       uuid.UUID
	ExecutionID             uuid.UUID
	Generation              int64
	Namespace               string
	PodName                 string
	PodUID                  string
	Phase                   string
	FailureClass            string
	FailureReasonCode       string
	PendingFailureThreshold time.Duration
	PodCreatedAt            time.Time
	ObservedAt              time.Time
}

type KubernetesExecutionPodObserver func(context.Context, KubernetesExecutionPodObservation) error

type KubernetesReconcilerConfig struct {
	PublicControlPlaneURL              string
	WorkerLeaseTTL                     time.Duration
	WorkerHeartbeatTimeout             time.Duration
	Interval                           time.Duration
	RecoverExpired                     func(context.Context, int) error
	ReconcileEphemeralWorkspaceCleanup func(context.Context, uuid.UUID, []string, time.Time) (int, error)
	FinalizeResourceSuspend            func(context.Context, KubernetesPodTerminalObservation) (bool, error)
	ObserveWorkerPod                   KubernetesWorkerPodObserver
	ObserveExecutionPod                KubernetesExecutionPodObserver
	PodPendingFailureThreshold         time.Duration
	PublishRoutingHealth               ManagedKubernetesRoutingHealthObserver
	PublishWarmCapacity                ManagedKubernetesWarmCapacityObserver
	PublishTargetCapacity              ManagedKubernetesTargetCapacityObserver
	Observer                           BackgroundObserver
	ResolveImagePull                   ImagePullCredentialResolver
}

type kubernetesTargetConfiguration struct {
	AllocationBackend               string                         `json:"allocationBackend"`
	RuntimeIsolation                *runtimeIsolationConfiguration `json:"runtimeIsolation"`
	RuntimeIsolationDecision        *runtimeIsolationDecision      `json:"-"`
	SandboxTemplateName             string                         `json:"sandboxTemplateName"`
	SandboxWarmPoolName             string                         `json:"sandboxWarmPoolName"`
	SandboxClaimReadyTimeoutSeconds int                            `json:"sandboxClaimReadyTimeoutSeconds"`
	SandboxAllowedTenantIDs         []uuid.UUID                    `json:"sandboxAllowedTenantIds"`
	APIServer                       string                         `json:"apiServer"`
	BearerToken                     string                         `json:"bearerToken"`
	BearerTokenFile                 string                         `json:"bearerTokenFile"`
	CACertificate                   string                         `json:"caCertificate"`
	CAFile                          string                         `json:"caFile"`
	Namespace                       string                         `json:"namespace"`
	ManageNamespace                 *bool                          `json:"manageNamespace"`
	ServiceAccountName              string                         `json:"serviceAccountName"`
	Image                           string                         `json:"image"`
	ImagePullPolicy                 string                         `json:"imagePullPolicy"`
	ImagePullSecrets                []string                       `json:"imagePullSecrets"`
	ControlPlaneURL                 string                         `json:"controlPlaneUrl"`
	AllowInsecureControlPlane       bool                           `json:"allowInsecureControlPlane"`
	RunnerCommand                   []string                       `json:"runnerCommand"`
	MaxActivePods                   int                            `json:"maxActivePods"`
	EgressCIDRs                     []string                       `json:"egressCidrs"`
	EgressTCPPorts                  []int                          `json:"egressTcpPorts"`
	PrivateNetworkCIDRs             []string                       `json:"privateNetworkCidrs"`
	ProviderHTTPProxy               string                         `json:"providerHttpProxy"`
	ProviderHTTPSProxy              string                         `json:"providerHttpsProxy"`
	ProviderAllProxy                string                         `json:"providerAllProxy"`
	ProviderNoProxy                 []string                       `json:"providerNoProxy"`
	CPURequest                      string                         `json:"cpuRequest"`
	CPULimit                        string                         `json:"cpuLimit"`
	PIDsLimit                       uint64                         `json:"pidsLimit"`
	MemoryRequest                   string                         `json:"memoryRequest"`
	MemoryLimit                     string                         `json:"memoryLimit"`
	EphemeralStorageRequest         string                         `json:"ephemeralStorageRequest"`
	EphemeralStorageLimit           string                         `json:"ephemeralStorageLimit"`
	WorkspaceSizeLimit              string                         `json:"workspaceSizeLimit"`
	GitCachePersistentVolumeClaim   string                         `json:"gitCachePersistentVolumeClaim"`
	QuotaCPURequests                string                         `json:"quotaCpuRequests"`
	QuotaCPULimits                  string                         `json:"quotaCpuLimits"`
	QuotaMemoryRequests             string                         `json:"quotaMemoryRequests"`
	QuotaMemoryLimits               string                         `json:"quotaMemoryLimits"`
	QuotaEphemeralStorage           string                         `json:"quotaEphemeralStorage"`
	GPUResourceName                 string                         `json:"gpuResourceName"`
	GPURequest                      string                         `json:"gpuRequest"`
	QuotaGPURequests                string                         `json:"quotaGpuRequests"`
	NodeSelector                    map[string]string              `json:"nodeSelector"`
	Tolerations                     []map[string]any               `json:"tolerations"`
	RequireNodeSpread               bool                           `json:"requireNodeSpread"`
}

type kubernetesPod struct {
	Name                string
	UID                 string
	AgentdImage         string
	SandboxRuntimeImage string
	Phase               string
	Reason              string
	CreatedAt           time.Time
	Labels              map[string]string
	Annotations         map[string]string
	ControllerOwnerKind string
	ControllerOwnerUID  string
	Conditions          []kubernetesPodCondition
	Containers          []kubernetesContainerStatus
	ResourceRequests    map[string]string
}

type kubernetesResourceQuota struct {
	Hard map[string]string
	Used map[string]string
}

type kubernetesPodCondition struct {
	Type   string
	Status string
	Reason string
}

type kubernetesContainerStatus struct {
	Name                 string
	WaitingReason        string
	Terminated           bool
	TerminatedReason     string
	LastTerminatedReason string
	ExitCode             int
}

type KubernetesPodTerminalObservation struct {
	ExecutionTargetID uuid.UUID
	ExecutionID       uuid.UUID
	Generation        int64
	Namespace         string
	PodName           string
	PodUID            string
	Phase             string
	ObservedAt        time.Time
}

type kubernetesClient interface {
	Apply(context.Context, string, map[string]any) error
	AttestPodPIDsLimit(context.Context, map[string]string, uint64, string) error
	RuntimeIsolationCapabilities(context.Context, kubernetesTargetConfiguration, time.Time) ([]runtimeIsolationCapability, error)
	EnsureRuntimeIsolationCanary(context.Context, persistence.ExecutionTarget, kubernetesTargetConfiguration, *ImagePullCredential) error
	GetPriorityClass(context.Context, string) (kubernetesPriorityClass, error)
	GetResourceQuota(context.Context, string, string) (kubernetesResourceQuota, error)
	ListPods(context.Context, string, uuid.UUID) ([]kubernetesPod, error)
	ListPodUIDs(context.Context, string) ([]string, error)
	DeletePod(context.Context, string, string, string) error
}

type kubernetesClientFactory interface {
	Open(kubernetesTargetConfiguration) (kubernetesClient, error)
}

type kubernetesFoundationState struct {
	hash      string
	appliedAt time.Time
}

type KubernetesReconciler struct {
	targets *Service
	config  KubernetesReconcilerConfig
	factory kubernetesClientFactory
	logger  *slog.Logger
	// foundationMu guards foundation. ReconcileOnce is exported and today runs from a
	// single leader-runner goroutine, but that is an implicit contract; a concurrent
	// caller (for example sharding targets to raise throughput) would hit Go's fatal
	// concurrent map write rather than a recoverable error.
	foundationMu sync.Mutex
	foundation   map[uuid.UUID]kubernetesFoundationState
	now          func() time.Time
}

func NewKubernetesReconciler(
	targets *Service,
	config KubernetesReconcilerConfig,
	logger *slog.Logger,
) *KubernetesReconciler {
	return &KubernetesReconciler{
		targets: targets, config: config, factory: kubernetesHTTPFactory{}, logger: logger,
		foundation: map[uuid.UUID]kubernetesFoundationState{}, now: func() time.Time { return time.Now().UTC() },
	}
}

func (r *KubernetesReconciler) Run(ctx context.Context) {
	interval := r.config.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		started := time.Now()
		err := r.ReconcileOnce(ctx)
		if r.config.Observer != nil {
			r.config.Observer.ObserveBackground("kubernetes", started, err)
		}
		if err != nil && ctx.Err() == nil {
			r.logger.Error("kubernetes execution reconciliation failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *KubernetesReconciler) ReconcileOnce(ctx context.Context) error {
	release, acquired, err := r.targets.tryKubernetesReconcilerLock(ctx)
	if err != nil {
		return problem.Wrap(500, "kubernetes_reconciler_lock_failed", "Kubernetes reconciler coordination failed.", err)
	}
	if !acquired {
		return nil
	}
	defer release()
	if r.config.RecoverExpired != nil {
		if err := r.config.RecoverExpired(ctx, 200); err != nil {
			return err
		}
	}
	var targets []persistence.ExecutionTarget
	if err := r.targets.db.WithContext(ctx).
		Where("kind = ? AND tenant_id IS NOT NULL AND status <> ?", "kubernetes", "disabled").Order("id").Find(&targets).Error; err != nil {
		return problem.Wrap(500, "kubernetes_targets_load_failed", "Kubernetes execution targets could not be loaded.", err)
	}
	var failures []error
	for _, target := range targets {
		if err := r.reconcileTarget(ctx, target); err != nil {
			failures = append(failures, fmt.Errorf("target %s: %w", target.ID, err))
		}
	}
	return errors.Join(failures...)
}

type kubernetesExecution struct {
	ID                       uuid.UUID  `gorm:"column:id"`
	TenantID                 uuid.UUID  `gorm:"column:tenant_id"`
	OrganizationID           uuid.UUID  `gorm:"column:organization_id"`
	ProjectID                uuid.UUID  `gorm:"column:project_id"`
	SessionID                uuid.UUID  `gorm:"column:session_id"`
	QueueClass               string     `gorm:"column:queue_class"`
	QueuePriority            int        `gorm:"column:queue_priority"`
	AbsoluteExpiresAt        *time.Time `gorm:"column:absolute_expires_at"`
	Status                   string     `gorm:"column:status"`
	Generation               int64      `gorm:"column:generation"`
	QueuedAt                 time.Time  `gorm:"column:queued_at"`
	WorkerReleaseRevisionID  *uuid.UUID `gorm:"column:worker_release_revision_id"`
	WorkerReleaseChannel     *string    `gorm:"column:worker_release_channel"`
	WorkerReleaseImageDigest *string    `gorm:"column:worker_release_image_digest"`
	WarmPoolModeSnapshot     string     `gorm:"column:warm_pool_mode_snapshot"`
	WorkerPool               *kubernetesExecutionPoolSnapshot
}

type kubernetesExecutionPoolSnapshot struct {
	ID                 uuid.UUID
	Version            int64
	CapacityClass      string
	Mode               string
	ClusterID          string
	Namespace          string
	SchedulingTemplate map[string]any
}

type kubernetesWorkerPodState struct {
	PodName     string `gorm:"column:pod_name"`
	InstanceUID string `gorm:"column:instance_uid"`
}

func (r *KubernetesReconciler) foundationState(targetID uuid.UUID) kubernetesFoundationState {
	r.foundationMu.Lock()
	defer r.foundationMu.Unlock()
	return r.foundation[targetID]
}

func (r *KubernetesReconciler) recordFoundationState(targetID uuid.UUID, hash string, appliedAt time.Time) {
	r.foundationMu.Lock()
	defer r.foundationMu.Unlock()
	r.foundation[targetID] = kubernetesFoundationState{hash: hash, appliedAt: appliedAt}
}

func (r *KubernetesReconciler) reconcileTarget(ctx context.Context, target persistence.ExecutionTarget) (err error) {
	healthObservation := ManagedKubernetesRoutingHealthObservation{
		ExecutionTargetID: target.ID,
		TenantOwned:       target.TenantID != nil,
		Status:            routing.HealthUnknown,
		CapacityStatus:    routing.CapacityUnknown,
		ReservationAuthority: &routing.ReservationAuthorityObservation{
			Mode: routing.ReservationAuthorityExactActiveV1,
		},
	}
	acknowledgedReservations := make(map[routing.ReservationIdentity]struct{})
	var warmCapacityObservations []ManagedKubernetesWarmCapacityObservation
	var targetCapacityObservation *ManagedKubernetesTargetCapacityObservation
	defer func() {
		if ctx.Err() != nil || target.TenantID == nil {
			return
		}
		reconcileSucceeded := err == nil
		observedAt := r.now()
		if r.config.PublishRoutingHealth != nil {
			healthObservation.ObservedAt = observedAt
			if publishErr := r.config.PublishRoutingHealth(ctx, healthObservation); publishErr != nil {
				err = errors.Join(err, publishErr)
			}
		}
		if reconcileSucceeded && r.config.PublishWarmCapacity != nil {
			for _, observation := range warmCapacityObservations {
				observation.ObservedAt = observedAt
				if publishErr := r.config.PublishWarmCapacity(ctx, observation); publishErr != nil {
					err = errors.Join(err, publishErr)
				}
			}
		}
		if reconcileSucceeded && r.config.PublishTargetCapacity != nil && targetCapacityObservation != nil {
			targetCapacityObservation.ObservedAt = observedAt
			if publishErr := r.config.PublishTargetCapacity(ctx, *targetCapacityObservation); publishErr != nil {
				err = errors.Join(err, publishErr)
			}
		}
	}()
	configuration, err := r.loadConfiguration(target)
	if err != nil {
		healthObservation.Reason = managedKubernetesRoutingReasonPointer(
			"Managed Kubernetes target configuration is unavailable.",
		)
		r.setKubernetesStatus(ctx, target, "offline", false, false, 0, 0)
		return err
	}
	client, err := r.factory.Open(configuration)
	if err != nil {
		healthObservation.Status = routing.HealthUnreachable
		healthObservation.Reason = managedKubernetesRoutingReasonPointer(
			"Managed Kubernetes API configuration is unavailable.",
		)
		r.setKubernetesStatus(ctx, target, "offline", false, false, 0, 0)
		return problem.Wrap(503, "kubernetes_api_unavailable", "Kubernetes API configuration is unavailable.", err)
	}
	if err := client.AttestPodPIDsLimit(ctx, configuration.NodeSelector, configuration.PIDsLimit, configuration.AllocationBackend); err != nil {
		healthObservation.Status = routing.HealthUnreachable
		healthObservation.Reason = managedKubernetesRoutingReasonPointer(
			"Managed Kubernetes node PID confinement is unavailable.",
		)
		r.setKubernetesStatus(ctx, target, "offline", false, false, 0, 0)
		return problem.Wrap(
			503,
			"kubernetes_pids_limit_unverified",
			"Every eligible Kubernetes node must expose a finite kubelet podPidsLimit no greater than the Target pidsLimit.",
			err,
		)
	}
	runtimeObservedAt := r.now()
	runtimeCapabilities, runtimeObservationErr := client.RuntimeIsolationCapabilities(ctx, configuration, runtimeObservedAt)
	gvisorCompatibilityErr := gvisorCompatibilityAccepted(
		ctx, r.targets.db, target, configuration.RuntimeIsolation,
	)
	runtimeDecision, runtimeDecisionErr := resolveKubernetesRuntimeIsolation(
		configuration,
		filterGVisorCapability(runtimeCapabilities, gvisorCompatibilityErr),
	)
	runtimeDecisionErr = preferGVisorCompatibilityError(
		&runtimeDecision, runtimeDecisionErr, gvisorCompatibilityErr,
	)
	if observationPersistErr := persistRuntimeIsolationObservation(
		ctx, r.targets.db, target.ID, runtimeCapabilities, &runtimeDecision,
		firstRuntimeIsolationError(runtimeObservationErr, runtimeDecisionErr), runtimeObservedAt,
	); observationPersistErr != nil {
		return observationPersistErr
	}
	if runtimeDecisionErr != nil {
		healthObservation.Status = routing.HealthUnreachable
		healthObservation.Reason = managedKubernetesRoutingReasonPointer(
			"Managed Kubernetes runtime isolation is unavailable.",
		)
		r.setKubernetesStatus(ctx, target, "offline", false, false, 0, 0)
		if runtimeObservationErr != nil {
			return runtimeObservationErr
		}
		return runtimeDecisionErr
	}
	configuration.RuntimeIsolationDecision = &runtimeDecision
	resolution, err := r.resolveImagePullCredential(ctx, target, configuration.Image)
	if err != nil {
		healthObservation.Reason = managedKubernetesRoutingReasonPointer(
			"Managed Kubernetes image pull credentials are unavailable.",
		)
		return r.rejectImagePullCredential(ctx, client, target, configuration, resolution.Authoritative, err)
	}
	credential := resolution.Credential
	if err := validateKubernetesImagePullCredential(configuration.Image, credential); err != nil {
		healthObservation.Reason = managedKubernetesRoutingReasonPointer(
			"Managed Kubernetes image pull credentials are invalid.",
		)
		return r.rejectImagePullCredential(ctx, client, target, configuration, resolution.Authoritative, err)
	}
	podBaseHash, err := r.foundationHash(target, configuration)
	if err != nil {
		healthObservation.Reason = managedKubernetesRoutingReasonPointer(
			"Managed Kubernetes foundation hashing failed.",
		)
		return err
	}
	foundationHash, err := kubernetesFoundationApplyHash(podBaseHash, credential)
	if err != nil {
		healthObservation.Reason = managedKubernetesRoutingReasonPointer(
			"Managed Kubernetes foundation hashing failed.",
		)
		return err
	}
	state := r.foundationState(target.ID)
	foundationChanged := state.hash != foundationHash || r.now().Sub(state.appliedAt) >= 5*time.Minute
	if foundationChanged {
		if err := r.applyFoundation(ctx, client, target, configuration, credential); err != nil {
			healthObservation.Status = routing.HealthUnreachable
			healthObservation.Reason = managedKubernetesRoutingReasonPointer(
				"Managed Kubernetes foundation apply failed.",
			)
			r.setKubernetesStatus(ctx, target, "offline", false, false, 0, 0)
			return err
		}
		r.recordFoundationState(target.ID, foundationHash, r.now())
	}
	if err := client.EnsureRuntimeIsolationCanary(ctx, target, configuration, credential); err != nil {
		if observationErr := persistRuntimeIsolationObservation(
			ctx, r.targets.db, target.ID, runtimeCapabilities, &runtimeDecision, err, r.now(),
		); observationErr != nil {
			return observationErr
		}
		healthObservation.Status = routing.HealthUnreachable
		healthObservation.Reason = managedKubernetesRoutingReasonPointer(
			"Managed Kubernetes runtime isolation canary is not ready.",
		)
		r.setKubernetesStatus(ctx, target, "offline", foundationChanged, false, 0, 0)
		return err
	}
	executions, err := r.loadKubernetesExecutions(ctx, target.ID)
	if err != nil {
		healthObservation.Reason = managedKubernetesRoutingReasonPointer(
			"Managed Kubernetes execution state is unavailable.",
		)
		return err
	}
	if configuration.AllocationBackend != string(kubernetesAllocationBackendNativePod) {
		if err := validateKubernetesSandboxExecutionTenants(configuration, executions); err != nil {
			healthObservation.Reason = managedKubernetesRoutingReasonPointer(
				"Managed Kubernetes Sandbox tenant allowlist rejected an Execution.",
			)
			return err
		}
	}
	if err := r.fenceKubernetesAllocationBackendTransition(ctx, client, target, configuration); err != nil {
		healthObservation.Reason = managedKubernetesRoutingReasonPointer(
			"Managed Kubernetes allocation backend transition is fenced.",
		)
		_ = r.setKubernetesStatus(ctx, target, "offline", foundationChanged, false, 0, 0)
		return err
	}
	if configuration.AllocationBackend != string(kubernetesAllocationBackendNativePod) {
		materialized, materializeErr := r.reconcileSandboxAllocations(ctx, client, target, configuration, executions)
		if materializeErr != nil {
			healthObservation.Reason = managedKubernetesRoutingReasonPointer(
				"Managed Kubernetes Sandbox allocation materialization failed.",
			)
			_ = r.setKubernetesStatus(ctx, target, "offline", foundationChanged, false, materialized.Allocated, materialized.Deleted)
			return materializeErr
		}
		for _, allocation := range materialized.Acknowledgements {
			acknowledgedReservations[routing.ReservationIdentity{
				ExecutionID: allocation.ExecutionID, Generation: allocation.Generation,
			}] = struct{}{}
		}
		if err := r.setKubernetesStatus(
			ctx, target, "active", foundationChanged, materialized.Allocated+materialized.Deleted > 0,
			materialized.Allocated, materialized.Deleted,
		); err != nil {
			return err
		}
		availableCapacity := configuration.MaxActivePods - materialized.Active
		if availableCapacity < 0 {
			availableCapacity = 0
		}
		healthObservation.Status = routing.HealthHealthy
		healthObservation.CapacityStatus = routing.CapacityAvailable
		healthObservation.AvailableCapacityUnits = &availableCapacity
		healthObservation.AllocatedCapacityUnits = materialized.Active
		healthObservation.ReservationAuthority.Acknowledgements = make(
			[]routing.ReservationIdentity, 0, len(acknowledgedReservations),
		)
		for identity := range acknowledgedReservations {
			healthObservation.ReservationAuthority.Acknowledgements = append(
				healthObservation.ReservationAuthority.Acknowledgements, identity,
			)
		}
		if availableCapacity == 0 {
			healthObservation.CapacityStatus = routing.CapacitySaturated
		}
		healthObservation.Reason = nil
		return nil
	}
	warmPools, err := r.loadKubernetesWarmPools(ctx, target.ID)
	if err != nil {
		healthObservation.Reason = managedKubernetesRoutingReasonPointer(
			"Managed Kubernetes Worker pool state is unavailable.",
		)
		return err
	}
	warmWorkerStates, err := r.loadKubernetesWarmWorkerStates(ctx, target.ID)
	if err != nil {
		healthObservation.Reason = managedKubernetesRoutingReasonPointer(
			"Managed Kubernetes Worker pool state is unavailable.",
		)
		return err
	}
	workerPodStates, err := r.loadKubernetesWorkerPodStates(ctx, target.ID, configuration.Namespace)
	if err != nil {
		healthObservation.Reason = managedKubernetesRoutingReasonPointer(
			"Managed Kubernetes Worker Pod state is unavailable.",
		)
		return err
	}
	warmRelease, warmPoolsSupported, err := r.loadKubernetesWarmReleaseSelection(ctx, target.ID, configuration.Image)
	if err != nil {
		healthObservation.Reason = managedKubernetesRoutingReasonPointer(
			"Managed Kubernetes warm Worker release state is unavailable.",
		)
		return err
	}
	pods, err := client.ListPods(ctx, configuration.Namespace, target.ID)
	if err != nil {
		healthObservation.Status = routing.HealthUnreachable
		healthObservation.Reason = managedKubernetesRoutingReasonPointer(
			"Managed Kubernetes Worker Pods could not be listed.",
		)
		return problem.Wrap(502, "kubernetes_pods_load_failed", "Kubernetes Worker Pods could not be listed.", err)
	}
	observedPodUIDs := make(map[string]struct{}, len(pods))
	for _, pod := range pods {
		if uid := strings.TrimSpace(pod.UID); uid != "" {
			observedPodUIDs[uid] = struct{}{}
		}
	}
	for _, state := range workerPodStates {
		uid := strings.TrimSpace(state.InstanceUID)
		if _, found := observedPodUIDs[uid]; found {
			continue
		}
		if err := r.observeWorkerPod(ctx, KubernetesWorkerPodObservation{
			ExecutionTargetID: target.ID,
			Namespace:         configuration.Namespace,
			PodName:           state.PodName,
			PodUID:            uid,
			Phase:             "Missing",
			Reason:            "confirmed-missing:reconcile-list",
			ObservedAt:        r.now(),
		}); err != nil {
			return err
		}
	}
	if r.config.ReconcileEphemeralWorkspaceCleanup != nil {
		activePodUIDs, err := client.ListPodUIDs(ctx, configuration.Namespace)
		if err != nil {
			return problem.Wrap(502, "kubernetes_pod_identities_load_failed", "The complete Kubernetes Pod identity set could not be loaded.", err)
		}
		for index := range activePodUIDs {
			activePodUIDs[index] = strings.TrimSpace(activePodUIDs[index])
			if activePodUIDs[index] == "" {
				return problem.New(502, "kubernetes_pod_identities_invalid", "The Kubernetes Pod identity list contained an empty UID.")
			}
		}
		if _, err := r.config.ReconcileEphemeralWorkspaceCleanup(ctx, target.ID, activePodUIDs, r.now()); err != nil {
			return problem.Wrap(500, "kubernetes_workspace_cleanup_reconcile_failed", "Kubernetes ephemeral Workspace cleanup could not be reconciled.", err)
		}
	}
	active := make(map[uuid.UUID]kubernetesExecution, len(executions))
	for _, execution := range executions {
		active[execution.ID] = execution
	}
	warmWorkerStatesByIdentity := make(map[kubernetesWarmWorkerIdentity]kubernetesWarmWorkerState, len(warmWorkerStates))
	for _, state := range warmWorkerStates {
		warmWorkerStatesByIdentity[kubernetesWarmWorkerIdentityKey(state.PodName, state.InstanceUID)] = state
	}
	existing := make(map[string]kubernetesPod, len(pods))
	warmClaimedCounts := make(map[uuid.UUID]int)
	unleasedWarmPods := make([]kubernetesObservedWarmPod, 0)
	created, deleted := 0, 0
	for _, pod := range pods {
		if strings.TrimSpace(pod.Labels[kubernetesWorkerModeLabel]) == kubernetesWorkerModeWarmPool {
			state, matchedState := kubernetesWarmWorkerStateForPod(warmWorkerStatesByIdentity, pod)
			if matchedState && state.HasLease {
				if state.WorkerPoolID != nil {
					warmClaimedCounts[*state.WorkerPoolID]++
				}
				existing[pod.Name] = pod
				continue
			}
			poolID, poolVersion, capacityClass, slot, parseErr := kubernetesWarmPodIdentity(pod)
			if parseErr != nil {
				deletedPod, retainedWorker, err := r.deleteObservedPodSafely(
					ctx, client, target.ID, configuration.Namespace, pod, "warm-pool-invalid-identity",
				)
				if err != nil {
					return err
				}
				if retainedWorker != nil {
					if retainedWorker.WorkerPoolID != nil {
						warmClaimedCounts[*retainedWorker.WorkerPoolID]++
					}
					existing[pod.Name] = pod
					continue
				}
				if deletedPod {
					deleted++
				} else {
					existing[pod.Name] = pod
				}
				continue
			}
			observed := kubernetesObservedWarmPod{
				Pod: pod, PoolID: poolID, PoolVersion: poolVersion, CapacityClass: capacityClass, Slot: slot,
			}
			if matchedState {
				stateCopy := state
				observed.State = &stateCopy
			}
			unleasedWarmPods = append(unleasedWarmPods, observed)
			continue
		}
		executionID, parseErr := uuid.Parse(pod.Labels[kubernetesExecutionLabel])
		if parseErr != nil {
			deletedPod, _, err := r.deleteObservedPodSafely(
				ctx, client, target.ID, configuration.Namespace, pod, "execution-pod-invalid-identity",
			)
			if err != nil {
				return err
			}
			if deletedPod {
				deleted++
			} else {
				existing[pod.Name] = pod
			}
			continue
		}
		execution, found := active[executionID]
		if found && !kubernetesExecutionWithinAbsoluteLifetime(execution, r.now()) {
			found = false
		}
		expectedName := ""
		expectedHash := ""
		if found {
			expectedName = kubernetesPodName(execution)
			expectedHash, err = kubernetesExecutionPodHash(podBaseHash, configuration.Image, execution)
			if err != nil {
				return err
			}
		}
		terminalPod := pod.Phase == "Succeeded" || pod.Phase == "Failed"
		exactExecutionPod := found && pod.Name == expectedName &&
			pod.Annotations[kubernetesConfigAnnotation] == expectedHash
		generation := int64(0)
		if exactExecutionPod {
			generation, err = kubernetesObservedExecutionPodGeneration(execution, pod)
			if err != nil {
				return err
			}
			failureClass, failureReasonCode := classifyKubernetesExecutionPodFailure(pod)
			if err := r.observeExecutionPod(ctx, KubernetesExecutionPodObservation{
				TenantID: execution.TenantID, ExecutionTargetID: target.ID,
				ExecutionID: executionID, Generation: generation,
				Namespace: configuration.Namespace, PodName: pod.Name, PodUID: pod.UID,
				Phase: pod.Phase, FailureClass: failureClass, FailureReasonCode: failureReasonCode,
				PendingFailureThreshold: r.podPendingFailureThreshold(), PodCreatedAt: pod.CreatedAt,
				ObservedAt: r.now(),
			}); err != nil {
				return err
			}
			if terminalPod {
				if err := r.observeTerminalExecutionPod(ctx, target.ID, configuration.Namespace, pod); err != nil {
					return err
				}
			}
		}
		if exactExecutionPod &&
			pod.Phase == "Succeeded" && kubernetesPodCompletedSuccessfully(pod) {
			if r.config.FinalizeResourceSuspend == nil {
				return problem.New(503, "kubernetes_suspend_finalizer_unavailable", "Kubernetes Pod-terminal suspension finalization is not configured.")
			}
			finalized, err := r.config.FinalizeResourceSuspend(ctx, KubernetesPodTerminalObservation{
				ExecutionTargetID: target.ID, ExecutionID: executionID, Generation: generation,
				Namespace: configuration.Namespace, PodName: pod.Name,
				PodUID: pod.UID, Phase: pod.Phase, ObservedAt: r.now(),
			})
			if err != nil {
				return problem.Wrap(500, "kubernetes_suspend_finalize_failed", "The terminal Kubernetes Worker Pod could not finalize resource suspension.", err)
			}
			if !finalized {
				// Keep the exact terminal Pod until Control Plane state catches up.
				// Deleting here would destroy the only kubelet-authored proof for this
				// Pod UID and can permanently wedge suspension if checkpoint-ready or
				// durable DB visibility lags the terminal observation briefly.
				existing[pod.Name] = pod
				continue
			}
		}
		if !exactExecutionPod || terminalPod {
			reason := "execution-pod-obsolete"
			if terminalPod {
				reason = "execution-pod-terminal"
			}
			deletedPod, _, err := r.deleteObservedPodSafely(
				ctx, client, target.ID, configuration.Namespace, pod, reason,
			)
			if err != nil {
				return err
			}
			if deletedPod {
				deleted++
			} else {
				existing[pod.Name] = pod
			}
			continue
		}
		existing[pod.Name] = pod
	}
	_, _, validationWarmPlansByName, err := kubernetesWarmPodPlans(
		warmPools, warmPoolsSupported, warmClaimedCounts, warmRelease, podBaseHash, configuration.Image,
	)
	if err != nil {
		return err
	}
	readyWarmCapacity := make(map[kubernetesWarmCapacityKey]int)
	priorityClassValidation := make(map[string]error)
	warmDemandEvictionCandidates := make([]kubernetesWarmDemandEvictionCandidate, 0)
	for _, observed := range unleasedWarmPods {
		plan, found := validationWarmPlansByName[observed.Pod.Name]
		terminalPod := observed.Pod.Phase == "Succeeded" || observed.Pod.Phase == "Failed"
		if !found || terminalPod ||
			observed.Pod.Annotations[kubernetesConfigAnnotation] != plan.ConfigHash ||
			observed.PoolID != plan.Pool.ID ||
			observed.PoolVersion != plan.Pool.Version ||
			observed.CapacityClass != plan.Pool.CapacityClass ||
			observed.Slot != plan.Slot ||
			(observed.State != nil && !kubernetesWarmWorkerStateMatchesPlan(*observed.State, plan)) {
			reason := "warm-pool-plan-mismatch"
			if !found {
				reason = "warm-pool-scale-down"
			} else if terminalPod {
				reason = "warm-pool-terminal"
			}
			deletedPod, retainedWorker, err := r.deleteObservedPodSafely(
				ctx, client, target.ID, configuration.Namespace, observed.Pod, reason,
			)
			if err != nil {
				return err
			}
			if retainedWorker != nil {
				if retainedWorker.WorkerPoolID != nil {
					warmClaimedCounts[*retainedWorker.WorkerPoolID]++
				}
				existing[observed.Pod.Name] = observed.Pod
				continue
			}
			if deletedPod {
				deleted++
				continue
			}
			existing[observed.Pod.Name] = observed.Pod
			continue
		}
		if observed.State == nil {
			existing[observed.Pod.Name] = observed.Pod
			if key, ok := kubernetesWarmCapacityKeyForPlan(plan); ok && !kubernetesWarmPodPlanGuaranteed(plan) {
				warmDemandEvictionCandidates = append(warmDemandEvictionCandidates, kubernetesWarmDemandEvictionCandidate{
					Observed: observed,
					Key:      key,
				})
			}
			continue
		}
		readinessObservedAt := r.now()
		if !kubernetesWarmWorkerStateReadyIdle(
			*observed.State,
			observed.Pod,
			plan,
			readinessObservedAt,
			r.config.WorkerHeartbeatTimeout,
		) {
			if kubernetesWarmWorkerStateShouldRecycle(
				*observed.State,
				observed.Pod,
				readinessObservedAt,
				r.config.WorkerHeartbeatTimeout,
			) {
				deletedPod, retainedWorker, err := r.deleteObservedPodSafely(
					ctx, client, target.ID, configuration.Namespace, observed.Pod, "warm-pool-not-ready",
				)
				if err != nil {
					return err
				}
				if retainedWorker != nil {
					if retainedWorker.WorkerPoolID != nil {
						warmClaimedCounts[*retainedWorker.WorkerPoolID]++
					}
					existing[observed.Pod.Name] = observed.Pod
					continue
				}
				if deletedPod {
					deleted++
					continue
				}
			}
			existing[observed.Pod.Name] = observed.Pod
			if key, ok := kubernetesWarmCapacityKeyForPlan(plan); ok && !kubernetesWarmPodPlanGuaranteed(plan) {
				warmDemandEvictionCandidates = append(warmDemandEvictionCandidates, kubernetesWarmDemandEvictionCandidate{
					Observed: observed,
					Key:      key,
				})
			}
			continue
		}
		existing[observed.Pod.Name] = observed.Pod
		if key, ok := kubernetesWarmCapacityKeyForState(*observed.State); ok {
			readyWarmCapacity[key]++
		}
	}
	readyForDemand := make(map[kubernetesWarmCapacityKey]int, len(readyWarmCapacity))
	for key, units := range readyWarmCapacity {
		readyForDemand[key] = units
	}
	coldNeeded := 0
	unmetWarmDemand := make(map[kubernetesWarmCapacityKey]int)
	for _, execution := range executions {
		if execution.Status != "queued" && execution.Status != "recovering" {
			continue
		}
		if !kubernetesExecutionWithinAbsoluteLifetime(execution, r.now()) {
			continue
		}
		if _, found := existing[kubernetesPodName(execution)]; found {
			continue
		}
		if key, ok := kubernetesWarmCapacityKeyForExecution(execution); ok {
			if readyForDemand[key] > 0 {
				readyForDemand[key]--
				continue
			}
			unmetWarmDemand[key]++
		}
		coldNeeded++
	}
	freeSlots := configuration.MaxActivePods - (len(existing) + deleted)
	if freeSlots < 0 {
		freeSlots = 0
	}
	demandEvictionsNeeded := coldNeeded - freeSlots
	if demandEvictionsNeeded > 0 {
		attempted := make(map[int]struct{}, len(warmDemandEvictionCandidates))
		evictCandidate := func(index int) error {
			attempted[index] = struct{}{}
			candidate := warmDemandEvictionCandidates[index]
			deletedPod, retainedWorker, err := r.deleteObservedPodSafely(
				ctx,
				client,
				target.ID,
				configuration.Namespace,
				candidate.Observed.Pod,
				"warm-pool-demand-fallback",
			)
			if err != nil {
				return err
			}
			if retainedWorker != nil {
				if retainedWorker.WorkerPoolID != nil {
					warmClaimedCounts[*retainedWorker.WorkerPoolID]++
				}
				return nil
			}
			if deletedPod {
				delete(existing, candidate.Observed.Pod.Name)
				deleted++
				demandEvictionsNeeded--
				if unmetWarmDemand[candidate.Key] > 0 {
					unmetWarmDemand[candidate.Key]--
				}
			}
			return nil
		}
		// Prefer evicting a not-ready Pod whose exact warm pool/release key
		// cannot satisfy queued demand. If the remaining cold demand is for a
		// general or different-pool Execution, any not-ready warm slot can still
		// be released because maxActivePods is target-wide.
		for index, candidate := range warmDemandEvictionCandidates {
			if demandEvictionsNeeded <= 0 {
				break
			}
			if unmetWarmDemand[candidate.Key] <= 0 {
				continue
			}
			if err := evictCandidate(index); err != nil {
				return err
			}
		}
		for index := range warmDemandEvictionCandidates {
			if demandEvictionsNeeded <= 0 {
				break
			}
			if _, found := attempted[index]; found {
				continue
			}
			if err := evictCandidate(index); err != nil {
				return err
			}
		}
	}
	guaranteedWarmPlans, bestEffortWarmPlans, _, err := kubernetesWarmPodPlans(
		warmPools, warmPoolsSupported, warmClaimedCounts, warmRelease, podBaseHash, configuration.Image,
	)
	if err != nil {
		return err
	}
	// A Pod accepted for deletion still consumes ResourceQuota until Kubernetes
	// finishes its grace period. Count deletion-pending Pods against the target
	// capacity so reconciliation does not create a replacement that the API
	// server must reject with an exceeded-quota error.
	scheduled := len(existing) + deleted
	var executionFailures []error
	applyWarmPlans := func(plans []kubernetesWarmPodPlan) {
		for _, plan := range plans {
			if scheduled >= configuration.MaxActivePods {
				break
			}
			name := kubernetesWarmPodName(plan)
			if _, found := existing[name]; found {
				continue
			}
			pod, err := r.warmPoolPod(target, configuration, plan, credential)
			if err != nil {
				executionFailures = append(executionFailures, fmt.Errorf("warm pod %s: %w", name, err))
				continue
			}
			if err := validateKubernetesWorkerPodPriorityClass(ctx, client, pod, priorityClassValidation); err != nil {
				executionFailures = append(executionFailures, fmt.Errorf("warm pod %s: %w", name, err))
				continue
			}
			path := kubernetesNamespacedPath(configuration.Namespace, "pods", name)
			if err := client.Apply(ctx, path, pod); err != nil {
				executionFailures = append(executionFailures, fmt.Errorf(
					"warm pod %s: %w", name,
					problem.Wrap(502, "kubernetes_pod_apply_failed", "A Kubernetes Worker Pod could not be applied.", err),
				))
				continue
			}
			created++
			scheduled++
		}
	}
	// Guaranteed slots consume the target-wide budget before cold Execution
	// Pods. Budget exhaustion truncates this slice without failing the pass;
	// the published min/ready authority makes the resulting deficit visible.
	applyWarmPlans(guaranteedWarmPlans)
	// One Execution that cannot get a Pod must not starve every other queued
	// Execution on this target for the cycle: a malformed pool scheduling
	// template, a rejected Pod spec or a failed observation write is scoped to
	// its own Execution. Failures are collected and reported after the sweep,
	// so the pass is still reported as failed and warm capacity stays
	// unpublished, but the remaining Executions are still placed.
	for _, execution := range executions {
		if execution.Status != "queued" && execution.Status != "recovering" {
			continue
		}
		if err := r.persistRuntimeIsolationDecision(ctx, target, execution, configuration); err != nil {
			executionFailures = append(executionFailures, fmt.Errorf("execution %s: %w", execution.ID, err))
			continue
		}
		name := kubernetesPodName(execution)
		if _, found := existing[name]; found {
			acknowledgedReservations[routing.ReservationIdentity{
				ExecutionID: execution.ID, Generation: execution.Generation,
			}] = struct{}{}
			continue
		}
		if key, ok := kubernetesWarmCapacityKeyForExecution(execution); ok && readyWarmCapacity[key] > 0 {
			readyWarmCapacity[key]--
			acknowledgedReservations[routing.ReservationIdentity{
				ExecutionID: execution.ID, Generation: execution.Generation,
			}] = struct{}{}
			continue
		}
		if scheduled >= configuration.MaxActivePods {
			continue
		}
		podHash, err := kubernetesExecutionPodHash(podBaseHash, configuration.Image, execution)
		if err != nil {
			executionFailures = append(executionFailures, fmt.Errorf("execution %s: %w", execution.ID, err))
			continue
		}
		pod, err := r.executionPod(target, configuration, podHash, execution, credential)
		if err != nil {
			executionFailures = append(executionFailures, fmt.Errorf("execution %s: %w", execution.ID, err))
			continue
		}
		// The Session can cross its immutable deadline after the initial query.
		// Recheck at the external write boundary so a stale reconciliation
		// snapshot cannot create a post-expiry Pod.
		if !kubernetesExecutionWithinAbsoluteLifetime(execution, r.now()) {
			continue
		}
		if err := validateKubernetesWorkerPodPriorityClass(ctx, client, pod, priorityClassValidation); err != nil {
			executionFailures = append(executionFailures, fmt.Errorf("execution %s: %w", execution.ID, err))
			continue
		}
		path := kubernetesNamespacedPath(configuration.Namespace, "pods", name)
		applyStartedAt := r.now()
		applyErr := client.Apply(ctx, path, pod)
		observation := KubernetesExecutionPodObservation{
			TenantID: execution.TenantID, ExecutionTargetID: target.ID,
			ExecutionID: execution.ID, Generation: execution.Generation + 1,
			Namespace: configuration.Namespace, PodName: name, Phase: "Applied",
			PendingFailureThreshold: r.podPendingFailureThreshold(), ObservedAt: applyStartedAt,
		}
		if applyErr != nil {
			observation.Phase = "ApplyFailed"
			observation.FailureClass = KubernetesPodFailureApplyFailed
			observation.FailureReasonCode = classifyKubernetesPodApplyFailure(applyErr)
		}
		if err := r.observeExecutionPod(ctx, observation); err != nil {
			if applyErr != nil {
				err = errors.Join(
					problem.Wrap(502, "kubernetes_pod_apply_failed", "A Kubernetes Worker Pod could not be applied.", applyErr),
					err,
				)
			}
			executionFailures = append(executionFailures, fmt.Errorf("execution %s: %w", execution.ID, err))
			continue
		}
		if applyErr != nil {
			executionFailures = append(executionFailures, fmt.Errorf(
				"execution %s: %w", execution.ID,
				problem.Wrap(502, "kubernetes_pod_apply_failed", "A Kubernetes Worker Pod could not be applied.", applyErr),
			))
			continue
		}
		acknowledgedReservations[routing.ReservationIdentity{
			ExecutionID: execution.ID, Generation: execution.Generation,
		}] = struct{}{}
		created++
		scheduled++
	}
	applyWarmPlans(bestEffortWarmPlans)
	if err := r.setKubernetesStatus(ctx, target, "active", foundationChanged, created+deleted > 0, created, deleted); err != nil {
		return errors.Join(append(executionFailures, err)...)
	}
	if len(executionFailures) > 0 {
		// The target itself is reachable and its status is current, so the
		// health observation published by the deferred hook stays accurate;
		// only the per-Execution placements failed.
		return errors.Join(executionFailures...)
	}
	if r.config.PublishTargetCapacity != nil {
		quota, quotaErr := client.GetResourceQuota(
			ctx, configuration.Namespace, kubernetesResourceQuotaName(target.ID),
		)
		if quotaErr != nil {
			return problem.Wrap(502, "kubernetes_resource_quota_load_failed", "Kubernetes ResourceQuota capacity could not be loaded.", quotaErr)
		}
		observation, capacityErr := kubernetesTargetCapacityFromQuota(*target.TenantID, target.ID, configuration, quota)
		if capacityErr != nil {
			return capacityErr
		}
		targetCapacityObservation = &observation
	}
	availableCapacity := configuration.MaxActivePods
	healthObservation.Status = routing.HealthHealthy
	healthObservation.CapacityStatus = routing.CapacityAvailable
	healthObservation.AvailableCapacityUnits = &availableCapacity
	healthObservation.AllocatedCapacityUnits = scheduled
	healthObservation.ReservationAuthority.Acknowledgements = make(
		[]routing.ReservationIdentity, 0, len(acknowledgedReservations),
	)
	for identity := range acknowledgedReservations {
		healthObservation.ReservationAuthority.Acknowledgements = append(
			healthObservation.ReservationAuthority.Acknowledgements,
			identity,
		)
	}
	sort.Slice(healthObservation.ReservationAuthority.Acknowledgements, func(left, right int) bool {
		leftIdentity := healthObservation.ReservationAuthority.Acknowledgements[left]
		rightIdentity := healthObservation.ReservationAuthority.Acknowledgements[right]
		if leftIdentity.ExecutionID != rightIdentity.ExecutionID {
			return leftIdentity.ExecutionID.String() < rightIdentity.ExecutionID.String()
		}
		return leftIdentity.Generation < rightIdentity.Generation
	})
	healthObservation.Reason = nil
	if scheduled >= configuration.MaxActivePods {
		healthObservation.CapacityStatus = routing.CapacitySaturated
		healthObservation.Reason = managedKubernetesRoutingReasonPointer(
			"Managed Kubernetes scheduled pod capacity is fully allocated.",
		)
	}
	warmCapacityObservations = managedKubernetesWarmCapacityObservations(
		*target.TenantID,
		target.ID,
		warmPools,
		warmPoolsSupported,
		warmClaimedCounts,
		readyWarmCapacity,
		warmRelease,
	)
	return nil
}

func firstRuntimeIsolationError(errors ...error) error {
	for _, err := range errors {
		if err != nil {
			return err
		}
	}
	return nil
}

func validateKubernetesSandboxExecutionTenants(
	configuration kubernetesTargetConfiguration,
	executions []kubernetesExecution,
) error {
	for _, execution := range executions {
		if !slices.Contains(configuration.SandboxAllowedTenantIDs, execution.TenantID) {
			return problem.New(
				403,
				"kubernetes_sandbox_execution_tenant_not_allowed",
				"An Execution Tenant is not explicitly allowed to use the Sandbox allocation backend.",
			)
		}
	}
	return nil
}

func kubernetesPodCompletedSuccessfully(pod kubernetesPod) bool {
	if pod.Phase != "Succeeded" || len(pod.Containers) != 1 {
		return false
	}
	container := pod.Containers[0]
	return container.Name == "agentd" && container.Terminated && container.ExitCode == 0
}

func kubernetesObservedExecutionPodGeneration(
	execution kubernetesExecution,
	pod kubernetesPod,
) (int64, error) {
	expected := execution.Generation
	if execution.Status == "queued" || execution.Status == "recovering" {
		expected++
	}
	generation, err := strconv.ParseInt(strings.TrimSpace(pod.Labels[kubernetesGenerationLabel]), 10, 64)
	if err != nil || generation <= 0 || generation != expected {
		return 0, problem.New(
			502,
			"kubernetes_pod_generation_invalid",
			"A Kubernetes Worker Pod did not carry the expected canonical Generation label.",
		)
	}
	return generation, nil
}

func classifyKubernetesExecutionPodFailure(pod kubernetesPod) (string, string) {
	if strings.EqualFold(strings.TrimSpace(pod.Reason), "Evicted") {
		return KubernetesPodFailureEvicted, "evicted"
	}
	for _, container := range pod.Containers {
		if strings.EqualFold(strings.TrimSpace(container.TerminatedReason), "OOMKilled") ||
			strings.EqualFold(strings.TrimSpace(container.LastTerminatedReason), "OOMKilled") {
			return KubernetesPodFailureOOMKilled, "oom-killed"
		}
	}
	for _, container := range pod.Containers {
		switch strings.TrimSpace(container.WaitingReason) {
		case "ImagePullBackOff":
			return KubernetesPodFailureImagePull, "image-pull-backoff"
		case "ErrImagePull":
			return KubernetesPodFailureImagePull, "err-image-pull"
		case "ErrImageNeverPull":
			return KubernetesPodFailureImagePull, "err-image-never-pull"
		case "InvalidImageName":
			return KubernetesPodFailureImagePull, "invalid-image-name"
		case "RegistryUnavailable":
			return KubernetesPodFailureImagePull, "registry-unavailable"
		}
	}
	for _, condition := range pod.Conditions {
		if strings.TrimSpace(condition.Type) == "PodScheduled" &&
			strings.EqualFold(strings.TrimSpace(condition.Status), "False") &&
			strings.EqualFold(strings.TrimSpace(condition.Reason), "Unschedulable") {
			return KubernetesPodFailureUnschedulable, "unschedulable"
		}
	}
	for _, container := range pod.Containers {
		switch strings.TrimSpace(container.WaitingReason) {
		case "CreateContainerConfigError":
			return KubernetesPodFailureContainerStart, "create-container-config-error"
		case "CreateContainerError":
			return KubernetesPodFailureContainerStart, "create-container-error"
		case "RunContainerError":
			return KubernetesPodFailureContainerStart, "run-container-error"
		case "StartError":
			return KubernetesPodFailureContainerStart, "start-error"
		case "CrashLoopBackOff":
			return KubernetesPodFailureContainerStart, "crash-loop-backoff"
		}
	}
	if strings.TrimSpace(pod.Phase) != "Failed" {
		return "", ""
	}
	switch strings.TrimSpace(pod.Reason) {
	case "NodeLost":
		return KubernetesPodFailureGeneric, "node-lost"
	case "Shutdown":
		return KubernetesPodFailureGeneric, "node-shutdown"
	case "DeadlineExceeded":
		return KubernetesPodFailureGeneric, "deadline-exceeded"
	case "UnexpectedAdmissionError":
		return KubernetesPodFailureGeneric, "unexpected-admission-error"
	default:
		return KubernetesPodFailureGeneric, "phase-failed"
	}
}

func classifyKubernetesPodApplyFailure(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "api-timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "api-cancelled"
	}
	var statusErr *kubernetesAPIStatusError
	if errors.As(err, &statusErr) && statusErr.StatusCode > 0 {
		return "api-status-" + strconv.Itoa(statusErr.StatusCode)
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) && networkErr.Timeout() {
		return "api-timeout"
	}
	return "api-request-failed"
}

func (r *KubernetesReconciler) loadKubernetesExecutions(ctx context.Context, targetID uuid.UUID) ([]kubernetesExecution, error) {
	type executionRow struct {
		ID                           uuid.UUID  `gorm:"column:id"`
		TenantID                     uuid.UUID  `gorm:"column:tenant_id"`
		OrganizationID               uuid.UUID  `gorm:"column:organization_id"`
		ProjectID                    uuid.UUID  `gorm:"column:project_id"`
		SessionID                    uuid.UUID  `gorm:"column:session_id"`
		QueueClass                   string     `gorm:"column:queue_class"`
		QueuePriority                int        `gorm:"column:queue_priority"`
		AbsoluteExpiresAt            *time.Time `gorm:"column:absolute_expires_at"`
		Status                       string     `gorm:"column:status"`
		Generation                   int64      `gorm:"column:generation"`
		QueuedAt                     time.Time  `gorm:"column:queued_at"`
		WorkerReleaseRevisionID      *uuid.UUID `gorm:"column:worker_release_revision_id"`
		WorkerReleaseChannel         *string    `gorm:"column:worker_release_channel"`
		WorkerReleaseImageDigest     *string    `gorm:"column:worker_release_image_digest"`
		WarmPoolModeSnapshot         string     `gorm:"column:warm_pool_mode_snapshot"`
		WorkerPoolID                 *uuid.UUID `gorm:"column:worker_pool_id"`
		WorkerPoolVersion            *int64     `gorm:"column:worker_pool_version"`
		CapacityClass                *string    `gorm:"column:capacity_class"`
		WorkerPoolSnapshotID         *uuid.UUID `gorm:"column:worker_pool_snapshot_id"`
		WorkerPoolSnapshotMode       *string    `gorm:"column:worker_pool_snapshot_mode"`
		WorkerPoolSnapshotCapacity   *string    `gorm:"column:worker_pool_snapshot_capacity_class"`
		WorkerPoolSnapshotClusterID  *string    `gorm:"column:worker_pool_snapshot_cluster_id"`
		WorkerPoolSnapshotNamespace  *string    `gorm:"column:worker_pool_snapshot_namespace"`
		WorkerPoolSnapshotScheduling *string    `gorm:"column:worker_pool_snapshot_scheduling_template"`
	}
	var rows []executionRow
	err := r.targets.db.WithContext(ctx).Table("agent_executions AS e").
		Select(`e.id, e.tenant_id, s.organization_id, s.project_id, e.session_id, e.queue_class, e.queue_priority,
			s.absolute_expires_at, e.status, e.generation, e.queued_at,
			e.warm_pool_mode_snapshot,
			e.worker_pool_id, e.worker_pool_version, e.capacity_class,
			e.worker_release_revision_id, e.worker_release_channel,
			pool.id AS worker_pool_snapshot_id,
			pool.mode AS worker_pool_snapshot_mode,
			pool.capacity_class AS worker_pool_snapshot_capacity_class,
			pool.cluster_id AS worker_pool_snapshot_cluster_id,
			pool.namespace AS worker_pool_snapshot_namespace,
			pool.scheduling_template AS worker_pool_snapshot_scheduling_template,
			manifest.image_digest AS worker_release_image_digest`).
		Joins("JOIN agent_sessions AS s ON s.tenant_id = e.tenant_id AND s.id = e.session_id").
		Joins("LEFT JOIN worker_pools AS pool ON pool.execution_target_id = e.execution_target_id AND pool.id = e.worker_pool_id AND pool.version = e.worker_pool_version").
		Joins("LEFT JOIN worker_release_revisions AS release ON release.execution_target_id = e.execution_target_id AND release.id = e.worker_release_revision_id").
		Joins("LEFT JOIN worker_manifests AS manifest ON manifest.id = release.worker_manifest_id").
		Where("e.execution_target_id = ? AND e.target_kind = ? AND e.status IN ?", targetID, "kubernetes", []string{"queued", "recovering", "leased", "running", "waiting-for-approval"}).
		Where("s.absolute_expires_at IS NULL OR s.absolute_expires_at > ?", r.now()).
		Order("e.queued_at, e.id").Scan(&rows).Error
	if err != nil {
		return nil, problem.Wrap(500, "kubernetes_executions_load_failed", "Kubernetes executions could not be loaded.", err)
	}
	items := make([]kubernetesExecution, 0, len(rows))
	for _, row := range rows {
		item := kubernetesExecution{
			ID: row.ID, TenantID: row.TenantID, OrganizationID: row.OrganizationID, ProjectID: row.ProjectID,
			SessionID: row.SessionID, QueueClass: row.QueueClass, QueuePriority: row.QueuePriority,
			AbsoluteExpiresAt: row.AbsoluteExpiresAt, Status: row.Status,
			Generation: row.Generation, QueuedAt: row.QueuedAt, WorkerReleaseRevisionID: row.WorkerReleaseRevisionID,
			WorkerReleaseChannel: row.WorkerReleaseChannel, WorkerReleaseImageDigest: row.WorkerReleaseImageDigest,
			WarmPoolModeSnapshot: row.WarmPoolModeSnapshot,
		}
		if row.WorkerPoolID != nil || row.WorkerPoolVersion != nil || row.CapacityClass != nil {
			if row.WorkerPoolID == nil || row.WorkerPoolVersion == nil || row.CapacityClass == nil ||
				*row.WorkerPoolVersion <= 0 || strings.TrimSpace(*row.CapacityClass) == "" {
				return nil, problem.New(409, "worker_pool_assignment_mismatch", "The Execution is assigned to another Worker pool snapshot.")
			}
			if row.WorkerPoolSnapshotID == nil || row.WorkerPoolSnapshotMode == nil || row.WorkerPoolSnapshotCapacity == nil ||
				*row.WorkerPoolSnapshotID != *row.WorkerPoolID ||
				strings.TrimSpace(*row.WorkerPoolSnapshotMode) == "" ||
				strings.TrimSpace(*row.WorkerPoolSnapshotCapacity) != strings.TrimSpace(*row.CapacityClass) {
				return nil, problem.New(409, "worker_pool_assignment_mismatch", "The Execution is assigned to another Worker pool snapshot.")
			}
			template := map[string]any{}
			if row.WorkerPoolSnapshotScheduling != nil && strings.TrimSpace(*row.WorkerPoolSnapshotScheduling) != "" {
				if err := json.Unmarshal([]byte(*row.WorkerPoolSnapshotScheduling), &template); err != nil {
					return nil, problem.Wrap(500, "kubernetes_executions_load_failed", "The selected Worker pool schedulingTemplate could not be decoded.", err)
				}
			}
			item.WorkerPool = &kubernetesExecutionPoolSnapshot{
				ID: *row.WorkerPoolID, Version: *row.WorkerPoolVersion, CapacityClass: strings.TrimSpace(*row.CapacityClass),
				Mode:               strings.TrimSpace(*row.WorkerPoolSnapshotMode),
				ClusterID:          strings.TrimSpace(stringValue(row.WorkerPoolSnapshotClusterID)),
				Namespace:          strings.TrimSpace(stringValue(row.WorkerPoolSnapshotNamespace)),
				SchedulingTemplate: template,
			}
		}
		items = append(items, item)
	}
	items, err = orderKubernetesExecutionsForService(items, r.now())
	if err != nil {
		return nil, problem.Wrap(
			500,
			"kubernetes_execution_fair_queue_invalid",
			"Kubernetes Execution fair-queue authority is invalid.",
			err,
		)
	}
	return items, nil
}

func orderKubernetesExecutionsForService(items []kubernetesExecution, now time.Time) ([]kubernetesExecution, error) {
	activeServiceUnits := make(map[uuid.UUID]int64)
	queuedCandidates := make([]fairqueue.Candidate, 0, len(items))
	queuedByID := make(map[uuid.UUID]kubernetesExecution, len(items))
	activeItems := make([]kubernetesExecution, 0, len(items))
	for _, item := range items {
		if fairqueue.IsQueuedStatus(item.Status) {
			queuedCandidates = append(queuedCandidates, fairqueue.Candidate{
				TenantID: item.TenantID, ID: item.ID,
				QueueClass: item.QueueClass, QueuePriority: item.QueuePriority,
				QueuedAt: item.QueuedAt,
			})
			queuedByID[item.ID] = item
		} else if fairqueue.IsActiveServiceStatus(item.Status) {
			activeServiceUnits[item.TenantID]++
			activeItems = append(activeItems, item)
		} else {
			return nil, fairqueue.ErrInvalidCandidate
		}
	}
	orderedCandidates, err := fairqueue.OrderWithPolicy(queuedCandidates, activeServiceUnits, fairqueue.Policy{
		Now: now, StarvationThreshold: executionqueue.DefaultStarvationThreshold,
	})
	if err != nil {
		return nil, err
	}
	ordered := make([]kubernetesExecution, 0, len(items))
	ordered = append(ordered, activeItems...)
	for _, candidate := range orderedCandidates {
		item, ok := queuedByID[candidate.ID]
		if !ok {
			return nil, fairqueue.ErrInvalidCandidate
		}
		ordered = append(ordered, item)
	}
	return ordered, nil
}

func (r *KubernetesReconciler) loadKubernetesWarmPools(ctx context.Context, targetID uuid.UUID) ([]kubernetesWarmPool, error) {
	type warmPoolRow struct {
		ID                         uuid.UUID `gorm:"column:id"`
		Version                    int64     `gorm:"column:version"`
		CapacityClass              string    `gorm:"column:capacity_class"`
		ClusterID                  string    `gorm:"column:cluster_id"`
		Namespace                  string    `gorm:"column:namespace"`
		ConfiguredDesiredIdleUnits int       `gorm:"column:configured_desired_idle_units"`
		DesiredIdleUnits           int       `gorm:"column:effective_desired_idle_units"`
		MinIdleUnits               int       `gorm:"column:min_idle_units"`
		MaxActiveUnits             int       `gorm:"column:max_active_units"`
		SchedulingTemplate         string    `gorm:"column:scheduling_template"`
		Status                     string    `gorm:"column:status"`
	}
	var rows []warmPoolRow
	err := r.targets.db.WithContext(ctx).Table("worker_pools AS pool").
		Select(`pool.id, pool.version, pool.capacity_class, pool.cluster_id, pool.namespace,
			pool.desired_idle_units AS configured_desired_idle_units,
			CASE
			  WHEN autoscaling.enabled = ?
			   AND autoscaling_state.policy_version = autoscaling.version
			  THEN autoscaling_state.desired_idle_units
			  ELSE pool.desired_idle_units
			END AS effective_desired_idle_units,
			pool.min_idle_units, pool.max_active_units, pool.scheduling_template, pool.status`, true).
		Joins(`LEFT JOIN worker_pool_autoscaling_policies AS autoscaling
		  ON autoscaling.worker_pool_id = pool.id AND autoscaling.worker_pool_version = pool.version`).
		Joins(`LEFT JOIN worker_pool_autoscaling_state AS autoscaling_state
		  ON autoscaling_state.worker_pool_id = autoscaling.worker_pool_id
		 AND autoscaling_state.worker_pool_version = autoscaling.worker_pool_version`).
		Where("pool.execution_target_id = ? AND pool.mode = ?", targetID, placement.PoolModeWarm).
		Order("pool.capacity_class, pool.id").
		Scan(&rows).Error
	if err != nil {
		return nil, problem.Wrap(500, "kubernetes_worker_pools_load_failed", "Kubernetes warm Worker pools could not be loaded.", err)
	}
	items := make([]kubernetesWarmPool, 0, len(rows))
	for _, row := range rows {
		template := map[string]any{}
		if strings.TrimSpace(row.SchedulingTemplate) != "" {
			if err := json.Unmarshal([]byte(row.SchedulingTemplate), &template); err != nil {
				return nil, problem.Wrap(500, "kubernetes_worker_pools_load_failed", "Kubernetes warm Worker pool schedulingTemplate could not be decoded.", err)
			}
		}
		items = append(items, kubernetesWarmPool{
			ID: row.ID, Version: row.Version, CapacityClass: row.CapacityClass,
			ClusterID: row.ClusterID, Namespace: row.Namespace,
			ConfiguredDesiredIdleUnits: row.ConfiguredDesiredIdleUnits,
			DesiredIdleUnits:           row.DesiredIdleUnits, MinIdleUnits: row.MinIdleUnits, MaxActiveUnits: row.MaxActiveUnits,
			SchedulingTemplate: template, Status: row.Status,
		})
	}
	return items, nil
}

func (r *KubernetesReconciler) loadKubernetesWarmWorkerStates(ctx context.Context, targetID uuid.UUID) ([]kubernetesWarmWorkerState, error) {
	var items []kubernetesWarmWorkerState
	err := r.targets.db.WithContext(ctx).Table("worker_instances AS worker").
		Select(`worker.id,
			worker.incarnation,
			worker.pod_name,
			worker.instance_uid,
			worker.worker_pool_id,
			worker.worker_pool_version,
			worker.capacity_class,
			worker.worker_release_revision_id,
			worker.worker_release_channel,
			worker.worker_release_status,
			worker.registration_trust_mode,
			worker.protocol_version,
			worker.current_manifest_id,
			worker.compatibility_status,
			worker.lease_supported,
			worker.fencing_supported,
			worker.status,
			worker.administrative_status,
			worker.last_heartbeat_at,
			EXISTS (
				SELECT 1
				FROM worker_leases AS lease
				WHERE lease.worker_id = worker.id
				  AND lease.worker_incarnation = worker.incarnation
			) AS has_lease`).
		Where("worker.execution_target_id = ? AND worker.target_kind = ? AND worker.worker_mode = ? AND worker.status <> ?",
			targetID, "kubernetes", kubernetesWorkerModeWarmPool, "terminated").
		Order("worker.pod_name, worker.instance_uid, worker.id").
		Scan(&items).Error
	if err != nil {
		return nil, problem.Wrap(500, "kubernetes_warm_workers_load_failed", "Kubernetes warm Worker state could not be loaded.", err)
	}
	return items, nil
}

func (r *KubernetesReconciler) loadKubernetesWorkerPodStates(
	ctx context.Context,
	targetID uuid.UUID,
	namespace string,
) ([]kubernetesWorkerPodState, error) {
	var items []kubernetesWorkerPodState
	err := r.targets.db.WithContext(ctx).Table("worker_instances").
		Select("pod_name, instance_uid").
		Where(
			"execution_target_id = ? AND target_kind = ? AND namespace = ? AND status <> ?",
			targetID, "kubernetes", namespace, "terminated",
		).
		Order("pod_name, instance_uid").
		Scan(&items).Error
	if err != nil {
		return nil, problem.Wrap(
			500,
			"kubernetes_worker_pod_states_load_failed",
			"Kubernetes Worker Pod lifecycle state could not be loaded.",
			err,
		)
	}
	return items, nil
}

func (r *KubernetesReconciler) loadKubernetesWarmReleaseSelection(
	ctx context.Context,
	targetID uuid.UUID,
	baseImage string,
) (kubernetesWarmReleaseSelection, bool, error) {
	plan, err := loadManagedReleasePlan(ctx, r.targets.db, targetID, baseImage)
	if err != nil {
		return kubernetesWarmReleaseSelection{}, false, err
	}
	if plan == nil {
		return kubernetesWarmReleaseSelection{}, true, nil
	}
	if plan.Canary != nil {
		return kubernetesWarmReleaseSelection{}, false, nil
	}
	digest := immutableImageDigest(plan.Promoted.Image)
	if digest == "" {
		return kubernetesWarmReleaseSelection{}, false, problem.New(409, "worker_release_execution_invalid", "Warm Worker release selection is invalid.")
	}
	channel := plan.Promoted.Channel
	revisionID := plan.Promoted.RevisionID
	return kubernetesWarmReleaseSelection{
		RevisionID:  &revisionID,
		Channel:     &channel,
		ImageDigest: &digest,
	}, true, nil
}

func (r *KubernetesReconciler) observeWorkerPod(ctx context.Context, observation KubernetesWorkerPodObservation) error {
	if r.config.ObserveWorkerPod == nil {
		return nil
	}
	if err := r.config.ObserveWorkerPod(ctx, observation); err != nil {
		return problem.Wrap(
			500,
			"kubernetes_worker_pod_observation_failed",
			"The Kubernetes Worker Pod lifecycle observation could not be recorded.",
			err,
		)
	}
	return nil
}

func (r *KubernetesReconciler) observeExecutionPod(
	ctx context.Context,
	observation KubernetesExecutionPodObservation,
) error {
	if r.config.ObserveExecutionPod == nil {
		return nil
	}
	if err := r.config.ObserveExecutionPod(ctx, observation); err != nil {
		return problem.Wrap(
			500,
			"kubernetes_execution_pod_observation_failed",
			"The Kubernetes Execution Pod provisioning observation could not be recorded.",
			err,
		)
	}
	return nil
}

func (r *KubernetesReconciler) podPendingFailureThreshold() time.Duration {
	if r.config.PodPendingFailureThreshold > 0 {
		return r.config.PodPendingFailureThreshold
	}
	return 2 * time.Minute
}

func (r *KubernetesReconciler) observeTerminalExecutionPod(
	ctx context.Context,
	targetID uuid.UUID,
	namespace string,
	pod kubernetesPod,
) error {
	failureClass, _ := classifyKubernetesExecutionPodFailure(pod)
	reason := "terminal-observation"
	if failureClass != "" {
		reason += ":" + failureClass
	}
	return r.observeWorkerPod(ctx, KubernetesWorkerPodObservation{
		ExecutionTargetID: targetID,
		Namespace:         namespace,
		PodName:           pod.Name,
		PodUID:            pod.UID,
		Phase:             pod.Phase,
		Reason:            reason,
		ObservedAt:        r.now(),
	})
}

func (r *KubernetesReconciler) deleteObservedPod(
	ctx context.Context,
	client kubernetesClient,
	targetID uuid.UUID,
	namespace string,
	pod kubernetesPod,
	reason string,
) error {
	if !strings.HasPrefix(reason, "delete-requested:") {
		reason = "delete-requested:" + reason
	}
	if err := r.observeWorkerPod(ctx, KubernetesWorkerPodObservation{
		ExecutionTargetID: targetID,
		Namespace:         namespace,
		PodName:           pod.Name,
		PodUID:            pod.UID,
		Phase:             pod.Phase,
		Reason:            reason,
		ObservedAt:        r.now(),
	}); err != nil {
		return err
	}
	if err := client.DeletePod(ctx, namespace, pod.Name, pod.UID); err != nil {
		if errors.Is(err, errKubernetesPodUIDPreconditionFailed) {
			return err
		}
		return problem.Wrap(502, "kubernetes_pod_delete_failed", "An obsolete Kubernetes Worker Pod could not be deleted.", err)
	}
	return nil
}

func (r *KubernetesReconciler) deleteObservedPodSafely(
	ctx context.Context,
	client kubernetesClient,
	targetID uuid.UUID,
	namespace string,
	pod kubernetesPod,
	reason string,
) (deleted bool, retainedWorker *persistence.WorkerInstance, err error) {
	deleteAllowed, retainedWorker, err := r.prepareKubernetesPodDeletion(ctx, targetID, namespace, pod, reason)
	if err != nil {
		return false, nil, err
	}
	if !deleteAllowed {
		return false, retainedWorker, nil
	}
	if err := r.deleteObservedPod(ctx, client, targetID, namespace, pod, reason); err != nil {
		if errors.Is(err, errKubernetesPodUIDPreconditionFailed) {
			// The exact observed UID no longer owns this Pod name. Keep the
			// durable old-UID fence and let the next snapshot reconcile the
			// replacement instead of marking the target unhealthy.
			return false, nil, nil
		}
		return false, nil, err
	}
	return true, nil, nil
}

func (r *KubernetesReconciler) prepareKubernetesPodDeletion(
	ctx context.Context,
	targetID uuid.UUID,
	namespace string,
	pod kubernetesPod,
	reason string,
) (deleteAllowed bool, retainedWorker *persistence.WorkerInstance, err error) {
	observedAt := r.now()
	identity, identityErr := podlifecycle.NewExactPodIdentity(targetID, namespace, pod.Name, pod.UID)
	if identityErr != nil {
		return false, nil, problem.Wrap(
			502,
			"kubernetes_pod_identity_invalid",
			"The Kubernetes Pod identity is invalid for lifecycle fencing.",
			identityErr,
		)
	}
	err = persistence.InTransaction(ctx, r.targets.db, func(tx *gorm.DB) error {
		acquired, err := podlifecycle.TryTransactionLogicalIdentityLock(ctx, tx, identity)
		if err != nil {
			return problem.Wrap(500, "kubernetes_pod_lifecycle_lock_failed", "The Kubernetes Pod lifecycle lock could not be acquired.", err)
		}
		if !acquired {
			return problem.New(503, "kubernetes_pod_lifecycle_lock_unavailable", "The Kubernetes Pod lifecycle is changing; retry reconciliation.")
		}
		var worker persistence.WorkerInstance
		lockErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Select("id", "incarnation", "instance_uid", "worker_pool_id", "status", "administrative_status").
			Where(
				"execution_target_id = ? AND target_kind = ? AND namespace = ? AND pod_name = ? AND instance_uid = ?",
				identity.ExecutionTargetID,
				"kubernetes",
				identity.Namespace,
				identity.PodName,
				identity.PodUID,
			).
			Take(&worker).Error
		workerFound := lockErr == nil
		if lockErr != nil && !errors.Is(lockErr, gorm.ErrRecordNotFound) {
			return problem.Wrap(500, "kubernetes_warm_worker_lock_failed", "The warm Worker lifecycle could not be locked for reconciliation.", lockErr)
		}
		if workerFound {
			var leaseCount int64
			if err := tx.WithContext(ctx).Model(&persistence.WorkerLease{}).
				Where("worker_id = ? AND worker_incarnation = ?", worker.ID, worker.Incarnation).
				Count(&leaseCount).Error; err != nil {
				return problem.Wrap(500, "kubernetes_warm_worker_lease_lookup_failed", "The warm Worker lease state could not be verified.", err)
			}
			if leaseCount > 0 {
				workerCopy := worker
				retainedWorker = &workerCopy
				return nil
			}
			var cleanupLeaseCount int64
			if err := tx.WithContext(ctx).Model(&persistence.WorkspaceCleanupCommand{}).
				Where(
					"delivery_worker_id = ? AND delivery_worker_incarnation = ? AND status IN ?",
					worker.ID,
					worker.Incarnation,
					[]string{"leased", "running"},
				).
				Count(&cleanupLeaseCount).Error; err != nil {
				return problem.Wrap(500, "kubernetes_warm_worker_cleanup_lease_lookup_failed", "The Worker cleanup delivery lease state could not be verified.", err)
			}
			if cleanupLeaseCount > 0 {
				workerCopy := worker
				retainedWorker = &workerCopy
				return nil
			}
		}
		if err := podlifecycle.EnsureDeletionFence(ctx, tx, identity, observedAt, reason); err != nil {
			return problem.Wrap(500, "kubernetes_pod_deletion_fence_failed", "The Kubernetes Pod deletion fence could not be persisted.", err)
		}
		if workerFound && worker.AdministrativeStatus == "active" && worker.Status == "online" {
			updates := map[string]any{"status": "draining", "draining_at": observedAt}
			result := tx.WithContext(ctx).Model(&persistence.WorkerInstance{}).
				Where(
					"id = ? AND incarnation = ? AND instance_uid = ? AND administrative_status = ? AND status = ?",
					worker.ID, worker.Incarnation, worker.InstanceUID, "active", "online",
				).
				Updates(updates)
			if result.Error != nil {
				return problem.Wrap(500, "kubernetes_warm_worker_drain_failed", "The warm Worker could not be drained before Pod deletion.", result.Error)
			}
			if result.RowsAffected != 1 {
				return problem.New(409, "worker_not_claimable", "The warm Worker changed while the Pod deletion was being prepared.")
			}
		}
		deleteAllowed = true
		return nil
	})
	if err != nil {
		return false, nil, err
	}
	return deleteAllowed, retainedWorker, nil
}

func kubernetesExecutionWithinAbsoluteLifetime(execution kubernetesExecution, now time.Time) bool {
	return execution.AbsoluteExpiresAt == nil || execution.AbsoluteExpiresAt.After(now)
}

func (r *KubernetesReconciler) loadConfiguration(target persistence.ExecutionTarget) (kubernetesTargetConfiguration, error) {
	if target.TenantID == nil {
		return kubernetesTargetConfiguration{}, problem.New(409, "kubernetes_target_tenant_required", "Managed Kubernetes targets must belong to a Tenant.")
	}
	if len(target.ConfigurationEncrypted) == 0 || r.targets.cipher == nil {
		return kubernetesTargetConfiguration{}, problem.New(409, "kubernetes_configuration_missing", "Kubernetes execution target configuration is missing.")
	}
	decoded, err := r.targets.cipher.Decrypt(target.ConfigurationEncrypted)
	if err != nil {
		return kubernetesTargetConfiguration{}, problem.Wrap(503, "kubernetes_configuration_unavailable", "Kubernetes execution target configuration could not be decrypted.", err)
	}
	var configuration kubernetesTargetConfiguration
	decoder := json.NewDecoder(strings.NewReader(decoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&configuration); err != nil {
		return kubernetesTargetConfiguration{}, problem.New(400, "invalid_kubernetes_configuration", "Kubernetes execution target configuration is invalid.")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return kubernetesTargetConfiguration{}, problem.New(400, "invalid_kubernetes_configuration", "Kubernetes execution target configuration is invalid.")
	}
	return r.normalizeKubernetes(target, configuration)
}

var (
	kubernetesNamePattern                 = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	quantityPattern                       = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?(?:m|Ki|Mi|Gi|Ti|Pi|Ei)?$`)
	kubernetesExtendedResourceNamePattern = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?/[A-Za-z0-9](?:[-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)
	kubernetesPositiveIntegerPattern      = regexp.MustCompile(`^[1-9][0-9]*$`)
)

func (r *KubernetesReconciler) normalizeKubernetes(
	target persistence.ExecutionTarget,
	configuration kubernetesTargetConfiguration,
) (kubernetesTargetConfiguration, error) {
	if err := normalizeKubernetesAllocationConfiguration(&configuration); err != nil {
		return kubernetesTargetConfiguration{}, err
	}
	if err := normalizeKubernetesRuntimeIsolationConfiguration(&configuration); err != nil {
		return kubernetesTargetConfiguration{}, err
	}
	if configuration.AllocationBackend == string(kubernetesAllocationBackendNativePod) {
		if len(configuration.SandboxAllowedTenantIDs) != 0 {
			return kubernetesTargetConfiguration{}, problem.New(
				400,
				"invalid_kubernetes_allocation_backend_configuration",
				"Native Kubernetes Pod allocation cannot include a Sandbox tenant allowlist.",
			)
		}
	} else if target.TenantID == nil || !slices.Contains(configuration.SandboxAllowedTenantIDs, *target.TenantID) {
		return kubernetesTargetConfiguration{}, problem.New(
			403,
			"kubernetes_sandbox_tenant_not_allowed",
			"The Kubernetes target Tenant is not explicitly allowed to use the Sandbox allocation backend.",
		)
	}
	configuration.APIServer = strings.TrimRight(strings.TrimSpace(configuration.APIServer), "/")
	if configuration.APIServer == "" {
		configuration.APIServer = "https://kubernetes.default.svc"
	}
	configuration.BearerTokenFile = strings.TrimSpace(configuration.BearerTokenFile)
	if configuration.BearerToken == "" && configuration.BearerTokenFile == "" {
		configuration.BearerTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	}
	configuration.CAFile = strings.TrimSpace(configuration.CAFile)
	if configuration.CACertificate == "" && configuration.CAFile == "" {
		configuration.CAFile = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	}
	for _, path := range []string{configuration.BearerTokenFile, configuration.CAFile} {
		if path != "" && !strings.HasPrefix(path, "/var/run/secrets/kubernetes.io/serviceaccount/") {
			return kubernetesTargetConfiguration{}, problem.New(400, "invalid_kubernetes_configuration", "Kubernetes token and CA files must use the in-cluster ServiceAccount path; use inline encrypted values for external clusters.")
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
	if configuration.ManageNamespace == nil {
		manageNamespace := true
		configuration.ManageNamespace = &manageNamespace
	}
	configuration.Image = strings.TrimSpace(configuration.Image)
	configuration.ImagePullPolicy = strings.TrimSpace(configuration.ImagePullPolicy)
	if configuration.ImagePullPolicy == "" {
		configuration.ImagePullPolicy = "IfNotPresent"
	}
	configuration.ControlPlaneURL = strings.TrimRight(strings.TrimSpace(configuration.ControlPlaneURL), "/")
	if configuration.ControlPlaneURL == "" {
		configuration.ControlPlaneURL = strings.TrimRight(strings.TrimSpace(r.config.PublicControlPlaneURL), "/")
	}
	if configuration.MaxActivePods == 0 {
		configuration.MaxActivePods = 50
	}
	configuration.GitCachePersistentVolumeClaim = strings.TrimSpace(configuration.GitCachePersistentVolumeClaim)
	configuration.GPUResourceName = strings.TrimSpace(configuration.GPUResourceName)
	configuration.GPURequest = strings.TrimSpace(configuration.GPURequest)
	configuration.QuotaGPURequests = strings.TrimSpace(configuration.QuotaGPURequests)
	if !kubernetesNamePattern.MatchString(configuration.Namespace) || len(configuration.Namespace) > 63 ||
		!kubernetesNamePattern.MatchString(configuration.ServiceAccountName) || len(configuration.ServiceAccountName) > 63 ||
		(configuration.GitCachePersistentVolumeClaim != "" &&
			(!kubernetesNamePattern.MatchString(configuration.GitCachePersistentVolumeClaim) || len(configuration.GitCachePersistentVolumeClaim) > 63)) {
		return kubernetesTargetConfiguration{}, problem.New(400, "invalid_kubernetes_configuration", "Kubernetes namespace or serviceAccountName is invalid.")
	}
	apiURL, err := url.Parse(configuration.APIServer)
	if err != nil || apiURL.Scheme != "https" || apiURL.Host == "" {
		return kubernetesTargetConfiguration{}, problem.New(400, "invalid_kubernetes_configuration", "Kubernetes apiServer must be an HTTPS origin.")
	}
	controlPlaneURL, err := url.Parse(configuration.ControlPlaneURL)
	if err != nil || controlPlaneURL.Scheme == "" || controlPlaneURL.Host == "" ||
		(controlPlaneURL.Scheme != "https" && !(controlPlaneURL.Scheme == "http" && configuration.AllowInsecureControlPlane)) {
		return kubernetesTargetConfiguration{}, problem.New(400, "invalid_kubernetes_configuration", "Kubernetes controlPlaneUrl must use HTTPS unless allowInsecureControlPlane is explicitly enabled.")
	}
	if configuration.Image == "" || len(configuration.Image) > 512 || len(configuration.RunnerCommand) == 0 {
		return kubernetesTargetConfiguration{}, problem.New(503, "kubernetes_worker_configuration_unavailable", "Kubernetes image and runnerCommand are required.")
	}
	if configuration.ImagePullPolicy != "Always" && configuration.ImagePullPolicy != "IfNotPresent" && configuration.ImagePullPolicy != "Never" {
		return kubernetesTargetConfiguration{}, problem.New(400, "invalid_kubernetes_configuration", "Kubernetes imagePullPolicy is invalid.")
	}
	if configuration.MaxActivePods < 1 || configuration.MaxActivePods > 10_000 || len(configuration.EgressCIDRs) == 0 {
		return kubernetesTargetConfiguration{}, problem.New(400, "invalid_kubernetes_configuration", "Kubernetes maxActivePods and egressCidrs are required and must be valid.")
	}
	for _, cidr := range configuration.EgressCIDRs {
		_, candidate, err := net.ParseCIDR(strings.TrimSpace(cidr))
		if err != nil {
			return kubernetesTargetConfiguration{}, problem.New(400, "invalid_kubernetes_configuration", "Kubernetes egressCidrs contains an invalid CIDR.")
		}
		for _, forbiddenCIDR := range kubernetesForbiddenEgressCIDRs {
			_, forbidden, _ := net.ParseCIDR(forbiddenCIDR)
			if cidrContainsCIDR(forbidden, candidate) {
				return kubernetesTargetConfiguration{}, problem.New(400, "invalid_kubernetes_egress_policy", "Kubernetes egressCidrs cannot directly allow link-local or metadata endpoints.")
			}
		}
	}
	if _, err := gitpolicy.ParsePrivateNetworkCIDRs(configuration.PrivateNetworkCIDRs); err != nil {
		return kubernetesTargetConfiguration{}, problem.New(400, "invalid_kubernetes_private_network_policy", "Kubernetes privateNetworkCidrs contains a non-private or unsafe CIDR.")
	}
	for _, privateCIDR := range configuration.PrivateNetworkCIDRs {
		_, privateNetwork, _ := net.ParseCIDR(privateCIDR)
		covered := false
		for _, egressCIDR := range configuration.EgressCIDRs {
			_, egressNetwork, _ := net.ParseCIDR(strings.TrimSpace(egressCIDR))
			if cidrContainsCIDR(egressNetwork, privateNetwork) {
				covered = true
				break
			}
		}
		if !covered {
			return kubernetesTargetConfiguration{}, problem.New(400, "invalid_kubernetes_private_network_policy", "Every Kubernetes privateNetworkCidrs entry must be covered by egressCidrs.")
		}
	}
	explicitEgressPorts := len(configuration.EgressTCPPorts) > 0
	ports := make(map[int]struct{}, len(configuration.EgressTCPPorts)+4)
	for _, port := range configuration.EgressTCPPorts {
		if port < 1 || port > 65535 {
			return kubernetesTargetConfiguration{}, problem.New(400, "invalid_kubernetes_egress_ports", "Kubernetes egressTcpPorts contains an invalid port.")
		}
		ports[port] = struct{}{}
	}
	controlPlanePort := 443
	if controlPlaneURL.Scheme == "http" {
		controlPlanePort = 80
	}
	if parsedPort := controlPlaneURL.Port(); parsedPort != "" {
		controlPlanePort, _ = strconv.Atoi(parsedPort)
	}
	ports[controlPlanePort] = struct{}{}
	for field, mode := range map[*string]providerproxy.Mode{
		&configuration.ProviderHTTPProxy:  providerproxy.HTTPOrHTTPS,
		&configuration.ProviderHTTPSProxy: providerproxy.HTTPOrHTTPS,
		&configuration.ProviderAllProxy:   providerproxy.HTTPHTTPSOrSOCKS5,
	} {
		normalized, proxyPort, proxyErr := providerproxy.Normalize(*field, mode)
		if proxyErr != nil {
			return kubernetesTargetConfiguration{}, kubernetesProviderProxyProblem(proxyErr)
		}
		*field = normalized
		if proxyPort > 0 {
			ports[proxyPort] = struct{}{}
		}
	}
	if !explicitEgressPorts {
		// HTTPS package/registry/provider APIs and SSH Git are the portable
		// default. Operators can replace this set explicitly.
		ports[443] = struct{}{}
		ports[22] = struct{}{}
	}
	configuration.EgressTCPPorts = configuration.EgressTCPPorts[:0]
	for port := range ports {
		configuration.EgressTCPPorts = append(configuration.EgressTCPPorts, port)
	}
	sort.Ints(configuration.EgressTCPPorts)
	if len(configuration.ProviderNoProxy) > 64 {
		return kubernetesTargetConfiguration{}, problem.New(400, "invalid_kubernetes_provider_proxy", "Kubernetes providerNoProxy exceeds 64 entries.")
	}
	for index := range configuration.ProviderNoProxy {
		value := strings.TrimSpace(configuration.ProviderNoProxy[index])
		if value == "" || value == "*" || len(value) > 253 || strings.ContainsAny(value, "\r\n\t\x00") {
			return kubernetesTargetConfiguration{}, problem.New(400, "invalid_kubernetes_provider_proxy", "Kubernetes providerNoProxy contains an invalid entry.")
		}
		configuration.ProviderNoProxy[index] = value
	}
	for _, value := range configuration.RunnerCommand {
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\r\n\x00") {
			return kubernetesTargetConfiguration{}, problem.New(400, "invalid_kubernetes_configuration", "Kubernetes runnerCommand is invalid.")
		}
	}
	resourceQuantities := []*string{
		&configuration.CPURequest, &configuration.CPULimit, &configuration.MemoryRequest, &configuration.MemoryLimit,
		&configuration.EphemeralStorageRequest, &configuration.EphemeralStorageLimit, &configuration.WorkspaceSizeLimit,
		&configuration.QuotaCPURequests, &configuration.QuotaCPULimits, &configuration.QuotaMemoryRequests,
		&configuration.QuotaMemoryLimits, &configuration.QuotaEphemeralStorage,
		&configuration.GPURequest, &configuration.QuotaGPURequests,
	}
	for _, value := range resourceQuantities {
		*value = strings.TrimSpace(*value)
	}
	if configuration.CPULimit == "" || configuration.MemoryLimit == "" ||
		configuration.EphemeralStorageLimit == "" || configuration.PIDsLimit == 0 {
		return kubernetesTargetConfiguration{}, problem.New(
			400,
			"kubernetes_resource_limits_required",
			"Kubernetes cpuLimit, memoryLimit, ephemeralStorageLimit, and pidsLimit are required.",
		)
	}
	if configuration.PIDsLimit > platform.MaximumKubernetesPIDsLimit {
		return kubernetesTargetConfiguration{}, problem.New(
			400,
			"invalid_kubernetes_configuration",
			"Kubernetes pidsLimit exceeds the supported maximum.",
		)
	}
	for _, value := range resourceQuantities {
		if *value != "" && !quantityPattern.MatchString(*value) {
			return kubernetesTargetConfiguration{}, problem.New(400, "invalid_kubernetes_configuration", "Kubernetes resource quantities are invalid.")
		}
	}
	hasGPUConfiguration := configuration.GPUResourceName != "" || configuration.GPURequest != "" || configuration.QuotaGPURequests != ""
	if hasGPUConfiguration {
		if !kubernetesExtendedResourceNamePattern.MatchString(configuration.GPUResourceName) ||
			!kubernetesPositiveIntegerPattern.MatchString(configuration.GPURequest) ||
			!kubernetesPositiveIntegerPattern.MatchString(configuration.QuotaGPURequests) {
			return kubernetesTargetConfiguration{}, problem.New(400, "invalid_kubernetes_gpu_configuration", "Kubernetes GPU capacity requires gpuResourceName, gpuRequest, and quotaGpuRequests.")
		}
		request, requestErr := parseKubernetesRequestedQuantity(configuration.GPURequest, 1)
		quota, quotaErr := parseKubernetesRequestedQuantity(configuration.QuotaGPURequests, 1)
		if requestErr != nil || quotaErr != nil || request == nil || quota == nil || *request > *quota {
			return kubernetesTargetConfiguration{}, problem.New(400, "invalid_kubernetes_gpu_configuration", "Kubernetes GPU request must be a positive integer no greater than quotaGpuRequests.")
		}
	}
	return configuration, nil
}

func kubernetesProviderProxyProblem(err error) error {
	switch {
	case errors.Is(err, providerproxy.ErrUnsupportedScheme):
		return problem.New(400, "invalid_kubernetes_provider_proxy", "Kubernetes Provider proxy scheme is unsupported.")
	case errors.Is(err, providerproxy.ErrInvalidHost):
		return problem.New(400, "invalid_kubernetes_provider_proxy", "Kubernetes Provider proxy host is invalid.")
	case errors.Is(err, providerproxy.ErrInvalidPort):
		return problem.New(400, "invalid_kubernetes_provider_proxy", "Kubernetes Provider proxy port is invalid.")
	case errors.Is(err, providerproxy.ErrSOCKS5Port):
		return problem.New(400, "invalid_kubernetes_provider_proxy", "SOCKS5 Provider proxy requires an explicit port.")
	default:
		return problem.New(400, "invalid_kubernetes_provider_proxy", "Kubernetes Provider proxy must be a credential-free HTTP(S) or SOCKS5 authority.")
	}
}

func (r *KubernetesReconciler) resolveImagePullCredential(
	ctx context.Context,
	target persistence.ExecutionTarget,
	image string,
) (ImagePullCredentialResolution, error) {
	if r.config.ResolveImagePull == nil {
		return ImagePullCredentialResolution{Authoritative: true}, nil
	}
	if target.TenantID == nil {
		return ImagePullCredentialResolution{Authoritative: true}, problem.New(409, "kubernetes_target_tenant_required", "Managed Kubernetes targets must belong to a Tenant.")
	}
	authority, err := registryAuthorityFromImageReference(image)
	if err != nil {
		return ImagePullCredentialResolution{Authoritative: true}, err
	}
	return r.config.ResolveImagePull(ctx, *target.TenantID, target.ID, registryComparisonAuthority(authority))
}

func (r *KubernetesReconciler) rejectImagePullCredential(
	ctx context.Context,
	client kubernetesClient,
	target persistence.ExecutionTarget,
	configuration kubernetesTargetConfiguration,
	authoritative bool,
	cause error,
) error {
	if authoritative {
		clearHash := ""
		if baseHash, err := r.foundationHash(target, configuration); err == nil {
			clearHash, _ = kubernetesFoundationApplyHash(baseHash, nil)
		}
		state := r.foundationState(target.ID)
		shouldApply := clearHash == "" || state.hash != clearHash || r.now().Sub(state.appliedAt) >= 5*time.Minute
		if shouldApply {
			if err := r.applyFoundation(ctx, client, target, configuration, nil); err != nil {
				r.setKubernetesStatus(ctx, target, "offline", false, false, 0, 0)
				return problem.Wrap(
					502,
					"kubernetes_registry_secret_clear_failed",
					"The invalid Kubernetes Worker Registry Secret could not be cleared.",
					errors.Join(cause, err),
				)
			}
			if clearHash != "" {
				r.recordFoundationState(target.ID, clearHash, r.now())
			}
		}
	}
	r.setKubernetesStatus(ctx, target, "offline", false, false, 0, 0)
	return cause
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func kubernetesNamespacedPath(namespace, resource, name string) string {
	return "/api/v1/namespaces/" + url.PathEscape(namespace) + "/" + resource + "/" + url.PathEscape(name)
}

func kubernetesNetworkPolicyPath(namespace, name string) string {
	return "/apis/networking.k8s.io/v1/namespaces/" + url.PathEscape(namespace) + "/networkpolicies/" + url.PathEscape(name)
}

func (r *KubernetesReconciler) setKubernetesStatus(
	ctx context.Context,
	target persistence.ExecutionTarget,
	status string,
	foundationChanged, podsChanged bool,
	created, deleted int,
) error {
	statusChanged := target.Status != status
	if !statusChanged && !foundationChanged && !podsChanged {
		return nil
	}
	if target.TenantID == nil {
		return problem.New(409, "kubernetes_target_tenant_required", "Managed Kubernetes targets must belong to a Tenant.")
	}
	return persistence.InTransaction(ctx, r.targets.db, func(tx *gorm.DB) error {
		if statusChanged {
			if err := tx.WithContext(ctx).Model(&persistence.ExecutionTarget{}).
				Where("id = ? AND kind = ? AND status <> ?", target.ID, "kubernetes", "disabled").
				Update("status", status).Error; err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: *target.TenantID, ActorType: "system",
			Action: "execution_target.kubernetes_reconciled", ResourceType: "execution_target", ResourceID: &target.ID,
			OrganizationID: target.OrganizationID, RequestID: "kubernetes-reconciler:" + uuid.NewString(),
			Metadata: map[string]any{"status": status, "foundationApplied": foundationChanged, "podsCreated": created, "podsDeleted": deleted},
		})
	})
}
