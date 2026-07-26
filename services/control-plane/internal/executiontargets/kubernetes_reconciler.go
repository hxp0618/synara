package executiontargets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/fairqueue"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/placement"
	"github.com/synara-ai/synara/services/control-plane/internal/podlifecycle"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/routing"
	"github.com/synara-ai/synara/services/control-plane/internal/workertiming"
)

const (
	kubernetesManagedLabel              = "synara.io/managed"
	kubernetesTargetLabel               = "synara.io/execution-target-id"
	kubernetesExecutionLabel            = "synara.io/execution-id"
	kubernetesGenerationLabel           = "synara.io/generation"
	kubernetesReleaseLabel              = "synara.io/worker-release-revision-id"
	kubernetesChannelLabel              = "synara.io/worker-release-channel"
	kubernetesWarmSlotLabel             = "synara.io/warm-slot"
	kubernetesConfigAnnotation          = "synara.io/config-sha256"
	kubernetesWorkloadIdentityVolume    = "workload-identity"
	kubernetesWorkloadIdentityTokenPath = "/var/run/secrets/synara.io/workload-identity/token"
	kubernetesLocalClusterID            = "kubernetes"
	kubernetesWorkerProtocolVersion     = 2
	kubernetesPodBoundRegistrationTrust = "kubernetes-pod-bound-v1"

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
	Observer                           BackgroundObserver
	ResolveImagePull                   ImagePullCredentialResolver
}

type kubernetesTargetConfiguration struct {
	APIServer                     string            `json:"apiServer"`
	BearerToken                   string            `json:"bearerToken"`
	BearerTokenFile               string            `json:"bearerTokenFile"`
	CACertificate                 string            `json:"caCertificate"`
	CAFile                        string            `json:"caFile"`
	Namespace                     string            `json:"namespace"`
	ManageNamespace               *bool             `json:"manageNamespace"`
	ServiceAccountName            string            `json:"serviceAccountName"`
	Image                         string            `json:"image"`
	ImagePullPolicy               string            `json:"imagePullPolicy"`
	ImagePullSecrets              []string          `json:"imagePullSecrets"`
	ControlPlaneURL               string            `json:"controlPlaneUrl"`
	AllowInsecureControlPlane     bool              `json:"allowInsecureControlPlane"`
	RunnerCommand                 []string          `json:"runnerCommand"`
	MaxActivePods                 int               `json:"maxActivePods"`
	EgressCIDRs                   []string          `json:"egressCidrs"`
	CPURequest                    string            `json:"cpuRequest"`
	CPULimit                      string            `json:"cpuLimit"`
	MemoryRequest                 string            `json:"memoryRequest"`
	MemoryLimit                   string            `json:"memoryLimit"`
	EphemeralStorageRequest       string            `json:"ephemeralStorageRequest"`
	EphemeralStorageLimit         string            `json:"ephemeralStorageLimit"`
	WorkspaceSizeLimit            string            `json:"workspaceSizeLimit"`
	GitCachePersistentVolumeClaim string            `json:"gitCachePersistentVolumeClaim"`
	QuotaCPURequests              string            `json:"quotaCpuRequests"`
	QuotaCPULimits                string            `json:"quotaCpuLimits"`
	QuotaMemoryRequests           string            `json:"quotaMemoryRequests"`
	QuotaMemoryLimits             string            `json:"quotaMemoryLimits"`
	QuotaEphemeralStorage         string            `json:"quotaEphemeralStorage"`
	NodeSelector                  map[string]string `json:"nodeSelector"`
	Tolerations                   []map[string]any  `json:"tolerations"`
	RequireNodeSpread             bool              `json:"requireNodeSpread"`
}

type kubernetesPod struct {
	Name        string
	UID         string
	Phase       string
	Reason      string
	CreatedAt   time.Time
	Labels      map[string]string
	Annotations map[string]string
	Conditions  []kubernetesPodCondition
	Containers  []kubernetesContainerStatus
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
	targets    *Service
	config     KubernetesReconcilerConfig
	factory    kubernetesClientFactory
	logger     *slog.Logger
	foundation map[uuid.UUID]kubernetesFoundationState
	now        func() time.Time
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
	release, acquired, err := persistence.TryAdvisoryLock(ctx, r.targets.db, "synara:kubernetes-execution-reconciler")
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

type kubernetesWarmPool struct {
	ID                 uuid.UUID      `gorm:"column:id"`
	Version            int64          `gorm:"column:version"`
	CapacityClass      string         `gorm:"column:capacity_class"`
	ClusterID          string         `gorm:"column:cluster_id"`
	Namespace          string         `gorm:"column:namespace"`
	DesiredIdleUnits   int            `gorm:"column:desired_idle_units"`
	MaxActiveUnits     int            `gorm:"column:max_active_units"`
	SchedulingTemplate map[string]any `gorm:"column:scheduling_template"`
	Status             string         `gorm:"column:status"`
}

type kubernetesWarmWorkerState struct {
	WorkerID                uuid.UUID  `gorm:"column:id"`
	WorkerIncarnation       int64      `gorm:"column:incarnation"`
	PodName                 string     `gorm:"column:pod_name"`
	InstanceUID             string     `gorm:"column:instance_uid"`
	WorkerPoolID            *uuid.UUID `gorm:"column:worker_pool_id"`
	WorkerPoolVersion       *int64     `gorm:"column:worker_pool_version"`
	CapacityClass           *string    `gorm:"column:capacity_class"`
	WorkerReleaseRevisionID *uuid.UUID `gorm:"column:worker_release_revision_id"`
	WorkerReleaseChannel    *string    `gorm:"column:worker_release_channel"`
	WorkerReleaseStatus     string     `gorm:"column:worker_release_status"`
	RegistrationTrustMode   string     `gorm:"column:registration_trust_mode"`
	ProtocolVersion         int        `gorm:"column:protocol_version"`
	CurrentManifestID       *uuid.UUID `gorm:"column:current_manifest_id"`
	CompatibilityStatus     string     `gorm:"column:compatibility_status"`
	LeaseSupported          bool       `gorm:"column:lease_supported"`
	FencingSupported        bool       `gorm:"column:fencing_supported"`
	Status                  string     `gorm:"column:status"`
	AdministrativeStatus    string     `gorm:"column:administrative_status"`
	LastHeartbeatAt         time.Time  `gorm:"column:last_heartbeat_at"`
	HasLease                bool       `gorm:"column:has_lease"`
}

type kubernetesWarmWorkerIdentity struct {
	PodName     string
	InstanceUID string
}

type kubernetesWorkerPodState struct {
	PodName     string `gorm:"column:pod_name"`
	InstanceUID string `gorm:"column:instance_uid"`
}

type kubernetesWarmReleaseSelection struct {
	RevisionID  *uuid.UUID
	Channel     *string
	ImageDigest *string
}

type kubernetesWarmPodPlan struct {
	Pool       kubernetesWarmPool
	Slot       int
	Release    kubernetesWarmReleaseSelection
	ConfigHash string
}

type kubernetesObservedWarmPod struct {
	Pod           kubernetesPod
	PoolID        uuid.UUID
	PoolVersion   int64
	CapacityClass string
	Slot          int
	State         *kubernetesWarmWorkerState
}

type kubernetesWarmDemandEvictionCandidate struct {
	Observed kubernetesObservedWarmPod
	Key      kubernetesWarmCapacityKey
}

func (r *KubernetesReconciler) reconcileTarget(ctx context.Context, target persistence.ExecutionTarget) (err error) {
	healthObservation := ManagedKubernetesRoutingHealthObservation{
		ExecutionTargetID: target.ID,
		TenantOwned:       target.TenantID != nil,
		Status:            routing.HealthUnknown,
		CapacityStatus:    routing.CapacityUnknown,
	}
	var warmCapacityObservations []ManagedKubernetesWarmCapacityObservation
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
	state := r.foundation[target.ID]
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
		r.foundation[target.ID] = kubernetesFoundationState{hash: foundationHash, appliedAt: r.now()}
	}
	executions, err := r.loadKubernetesExecutions(ctx, target.ID)
	if err != nil {
		healthObservation.Reason = managedKubernetesRoutingReasonPointer(
			"Managed Kubernetes execution state is unavailable.",
		)
		return err
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
	_, validationWarmPlansByName, err := kubernetesWarmPodPlans(
		warmPools, warmPoolsSupported, warmClaimedCounts, warmRelease, podBaseHash, configuration.Image,
	)
	if err != nil {
		return err
	}
	readyWarmCapacity := make(map[kubernetesWarmCapacityKey]int)
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
			if key, ok := kubernetesWarmCapacityKeyForPlan(plan); ok {
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
			if key, ok := kubernetesWarmCapacityKeyForPlan(plan); ok {
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
	desiredWarmPlans, _, err := kubernetesWarmPodPlans(
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
	for _, execution := range executions {
		if execution.Status != "queued" && execution.Status != "recovering" {
			continue
		}
		name := kubernetesPodName(execution)
		if _, found := existing[name]; found {
			continue
		}
		if key, ok := kubernetesWarmCapacityKeyForExecution(execution); ok && readyWarmCapacity[key] > 0 {
			readyWarmCapacity[key]--
			continue
		}
		if scheduled >= configuration.MaxActivePods {
			continue
		}
		podHash, err := kubernetesExecutionPodHash(podBaseHash, configuration.Image, execution)
		if err != nil {
			return err
		}
		pod, err := r.executionPod(target, configuration, podHash, execution, credential)
		if err != nil {
			return err
		}
		// The Session can cross its immutable deadline after the initial query.
		// Recheck at the external write boundary so a stale reconciliation
		// snapshot cannot create a post-expiry Pod.
		if !kubernetesExecutionWithinAbsoluteLifetime(execution, r.now()) {
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
				return errors.Join(
					problem.Wrap(502, "kubernetes_pod_apply_failed", "A Kubernetes Worker Pod could not be applied.", applyErr),
					err,
				)
			}
			return err
		}
		if applyErr != nil {
			return problem.Wrap(502, "kubernetes_pod_apply_failed", "A Kubernetes Worker Pod could not be applied.", applyErr)
		}
		created++
		scheduled++
	}
	for _, plan := range desiredWarmPlans {
		if scheduled >= configuration.MaxActivePods {
			break
		}
		name := kubernetesWarmPodName(plan)
		if _, found := existing[name]; found {
			continue
		}
		pod, err := r.warmPoolPod(target, configuration, plan, credential)
		if err != nil {
			return err
		}
		path := kubernetesNamespacedPath(configuration.Namespace, "pods", name)
		if err := client.Apply(ctx, path, pod); err != nil {
			return problem.Wrap(502, "kubernetes_pod_apply_failed", "A Kubernetes Worker Pod could not be applied.", err)
		}
		created++
		scheduled++
	}
	if err := r.setKubernetesStatus(ctx, target, "active", foundationChanged, created+deleted > 0, created, deleted); err != nil {
		return err
	}
	availableCapacity := configuration.MaxActivePods
	healthObservation.Status = routing.HealthHealthy
	healthObservation.CapacityStatus = routing.CapacityAvailable
	healthObservation.AvailableCapacityUnits = &availableCapacity
	healthObservation.AllocatedCapacityUnits = scheduled
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

func managedKubernetesWarmCapacityObservations(
	tenantID uuid.UUID,
	executionTargetID uuid.UUID,
	warmPools []kubernetesWarmPool,
	warmPoolsSupported bool,
	warmClaimedCounts map[uuid.UUID]int,
	readyWarmCapacity map[kubernetesWarmCapacityKey]int,
	warmRelease kubernetesWarmReleaseSelection,
) []ManagedKubernetesWarmCapacityObservation {
	observations := make([]ManagedKubernetesWarmCapacityObservation, 0, len(warmPools))
	for _, pool := range warmPools {
		if pool.Status != placement.PoolStatusActive {
			continue
		}
		claimed := warmClaimedCounts[pool.ID]
		if claimed < 0 {
			claimed = 0
		}
		observation := ManagedKubernetesWarmCapacityObservation{
			TenantID:          tenantID,
			ExecutionTargetID: executionTargetID,
			WorkerPoolID:      pool.ID,
			WorkerPoolVersion: pool.Version,
			WarmSupported:     warmPoolsSupported,
			ClaimedUnits:      claimed,
		}
		if !warmPoolsSupported {
			observation.Reason = managedKubernetesRoutingReasonPointer(
				"Managed Kubernetes warm capacity is unavailable while a canary Worker release is active.",
			)
			observations = append(observations, observation)
			continue
		}
		observation.WorkerReleaseRevisionID = warmRelease.RevisionID
		observation.WorkerReleaseChannel = warmRelease.Channel
		observation.DesiredTotalUnits = pool.DesiredIdleUnits + claimed
		if observation.DesiredTotalUnits > pool.MaxActiveUnits {
			observation.DesiredTotalUnits = pool.MaxActiveUnits
		}
		key := kubernetesWarmCapacityKey{
			PoolID:          pool.ID,
			PoolVersion:     pool.Version,
			CapacityClass:   pool.CapacityClass,
			ReleaseRevision: optionalUUIDString(warmRelease.RevisionID),
			ReleaseChannel:  stringValue(warmRelease.Channel),
		}
		observation.ReadyIdleUnits = readyWarmCapacity[key]
		observations = append(observations, observation)
	}
	return observations
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
		Select(`e.id, e.tenant_id, s.organization_id, s.project_id, e.session_id, s.absolute_expires_at, e.status, e.generation, e.queued_at,
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
			SessionID: row.SessionID, AbsoluteExpiresAt: row.AbsoluteExpiresAt, Status: row.Status,
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
	items, err = orderKubernetesExecutionsForService(items)
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

func orderKubernetesExecutionsForService(items []kubernetesExecution) ([]kubernetesExecution, error) {
	activeServiceUnits := make(map[uuid.UUID]int64)
	queuedCandidates := make([]fairqueue.Candidate, 0, len(items))
	queuedByID := make(map[uuid.UUID]kubernetesExecution, len(items))
	activeItems := make([]kubernetesExecution, 0, len(items))
	for _, item := range items {
		if fairqueue.IsQueuedStatus(item.Status) {
			queuedCandidates = append(queuedCandidates, fairqueue.Candidate{
				TenantID: item.TenantID,
				ID:       item.ID,
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
	orderedCandidates, err := fairqueue.Order(queuedCandidates, activeServiceUnits)
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
		ID                 uuid.UUID `gorm:"column:id"`
		Version            int64     `gorm:"column:version"`
		CapacityClass      string    `gorm:"column:capacity_class"`
		ClusterID          string    `gorm:"column:cluster_id"`
		Namespace          string    `gorm:"column:namespace"`
		DesiredIdleUnits   int       `gorm:"column:desired_idle_units"`
		MaxActiveUnits     int       `gorm:"column:max_active_units"`
		SchedulingTemplate string    `gorm:"column:scheduling_template"`
		Status             string    `gorm:"column:status"`
	}
	var rows []warmPoolRow
	err := r.targets.db.WithContext(ctx).Table("worker_pools").
		Select("id, version, capacity_class, cluster_id, namespace, desired_idle_units, max_active_units, scheduling_template, status").
		Where("execution_target_id = ? AND mode = ?", targetID, placement.PoolModeWarm).
		Order("capacity_class, id").
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
			DesiredIdleUnits: row.DesiredIdleUnits, MaxActiveUnits: row.MaxActiveUnits,
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

func kubernetesWarmWorkerStateForPod(
	states map[kubernetesWarmWorkerIdentity]kubernetesWarmWorkerState,
	pod kubernetesPod,
) (kubernetesWarmWorkerState, bool) {
	state, found := states[kubernetesWarmWorkerIdentityKey(pod.Name, pod.UID)]
	return state, found
}

func kubernetesWarmWorkerIdentityKey(podName, instanceUID string) kubernetesWarmWorkerIdentity {
	return kubernetesWarmWorkerIdentity{
		PodName:     strings.TrimSpace(podName),
		InstanceUID: strings.TrimSpace(instanceUID),
	}
}

func kubernetesWarmPodIdentity(pod kubernetesPod) (uuid.UUID, int64, string, int, error) {
	poolID, err := uuid.Parse(strings.TrimSpace(pod.Labels[kubernetesWorkerPoolIDLabel]))
	if err != nil {
		return uuid.UUID{}, 0, "", 0, err
	}
	poolVersion, err := strconv.ParseInt(strings.TrimSpace(pod.Labels[kubernetesWorkerPoolVersionLabel]), 10, 64)
	if err != nil || poolVersion <= 0 {
		return uuid.UUID{}, 0, "", 0, errors.New("invalid worker pool version")
	}
	capacityClass := strings.TrimSpace(pod.Labels[kubernetesCapacityClassLabel])
	if capacityClass != placement.CapacityClassStandard && capacityClass != placement.CapacityClassInteractive {
		return uuid.UUID{}, 0, "", 0, errors.New("invalid capacity class")
	}
	slot, err := strconv.Atoi(strings.TrimSpace(pod.Labels[kubernetesWarmSlotLabel]))
	if err != nil || slot < 0 {
		return uuid.UUID{}, 0, "", 0, errors.New("invalid warm slot")
	}
	return poolID, poolVersion, capacityClass, slot, nil
}

func kubernetesWarmWorkerStateMatchesPlan(state kubernetesWarmWorkerState, plan kubernetesWarmPodPlan) bool {
	return state.WorkerPoolID != nil &&
		state.WorkerPoolVersion != nil &&
		state.CapacityClass != nil &&
		*state.WorkerPoolID == plan.Pool.ID &&
		*state.WorkerPoolVersion == plan.Pool.Version &&
		*state.CapacityClass == plan.Pool.CapacityClass &&
		sameOptionalUUID(state.WorkerReleaseRevisionID, plan.Release.RevisionID) &&
		sameOptionalString(state.WorkerReleaseChannel, plan.Release.Channel)
}

type kubernetesWarmCapacityKey struct {
	PoolID          uuid.UUID
	PoolVersion     int64
	CapacityClass   string
	ReleaseRevision string
	ReleaseChannel  string
}

func kubernetesWarmWorkerStateReadyIdle(
	state kubernetesWarmWorkerState,
	pod kubernetesPod,
	plan kubernetesWarmPodPlan,
	now time.Time,
	heartbeatTimeout time.Duration,
) bool {
	return kubernetesWarmWorkerIdentityKey(state.PodName, state.InstanceUID) ==
		kubernetesWarmWorkerIdentityKey(pod.Name, pod.UID) &&
		pod.Phase == "Running" &&
		!state.HasLease &&
		strings.TrimSpace(state.Status) == "online" &&
		strings.TrimSpace(state.AdministrativeStatus) == "active" &&
		state.ProtocolVersion == kubernetesWorkerProtocolVersion &&
		state.CurrentManifestID != nil &&
		strings.TrimSpace(state.CompatibilityStatus) == "compatible" &&
		state.LeaseSupported &&
		state.FencingSupported &&
		strings.TrimSpace(state.RegistrationTrustMode) == kubernetesPodBoundRegistrationTrust &&
		kubernetesWarmWorkerHeartbeatFresh(state.LastHeartbeatAt, now, heartbeatTimeout) &&
		kubernetesWarmWorkerStateMatchesPlan(state, plan) &&
		kubernetesWarmWorkerReleaseReady(state, plan)
}

func kubernetesWarmWorkerStateShouldRecycle(
	state kubernetesWarmWorkerState,
	pod kubernetesPod,
	now time.Time,
	heartbeatTimeout time.Duration,
) bool {
	if pod.Phase == "Succeeded" || pod.Phase == "Failed" {
		return true
	}
	status := strings.TrimSpace(state.Status)
	administrativeStatus := strings.TrimSpace(state.AdministrativeStatus)
	return status == "offline" ||
		status == "draining" ||
		status == "terminated" ||
		administrativeStatus == "draining" ||
		!kubernetesWarmWorkerHeartbeatFresh(state.LastHeartbeatAt, now, heartbeatTimeout)
}

func kubernetesWarmWorkerHeartbeatFresh(lastHeartbeatAt, now time.Time, heartbeatTimeout time.Duration) bool {
	if heartbeatTimeout <= 0 || lastHeartbeatAt.IsZero() {
		return false
	}
	return !lastHeartbeatAt.Before(now.Add(-heartbeatTimeout))
}

func kubernetesWarmWorkerReleaseReady(state kubernetesWarmWorkerState, plan kubernetesWarmPodPlan) bool {
	planHasRevision := plan.Release.RevisionID != nil
	planHasChannel := plan.Release.Channel != nil
	if planHasRevision != planHasChannel ||
		!sameOptionalUUID(state.WorkerReleaseRevisionID, plan.Release.RevisionID) ||
		!sameOptionalString(state.WorkerReleaseChannel, plan.Release.Channel) {
		return false
	}
	status := strings.TrimSpace(state.WorkerReleaseStatus)
	if !planHasRevision {
		return status == "" || status == "unmanaged"
	}
	return status == "active"
}

func kubernetesWarmCapacityKeyForState(state kubernetesWarmWorkerState) (kubernetesWarmCapacityKey, bool) {
	if state.WorkerPoolID == nil || state.WorkerPoolVersion == nil || state.CapacityClass == nil {
		return kubernetesWarmCapacityKey{}, false
	}
	if (state.WorkerReleaseRevisionID == nil) != (state.WorkerReleaseChannel == nil) {
		return kubernetesWarmCapacityKey{}, false
	}
	return kubernetesWarmCapacityKey{
		PoolID: *state.WorkerPoolID, PoolVersion: *state.WorkerPoolVersion, CapacityClass: *state.CapacityClass,
		ReleaseRevision: optionalUUIDString(state.WorkerReleaseRevisionID),
		ReleaseChannel:  stringValue(state.WorkerReleaseChannel),
	}, true
}

func kubernetesWarmCapacityKeyForExecution(execution kubernetesExecution) (kubernetesWarmCapacityKey, bool) {
	if execution.WorkerPool == nil || execution.WorkerPool.Mode != placement.PoolModeWarm {
		return kubernetesWarmCapacityKey{}, false
	}
	if (execution.WorkerReleaseRevisionID == nil) != (execution.WorkerReleaseChannel == nil) {
		return kubernetesWarmCapacityKey{}, false
	}
	return kubernetesWarmCapacityKey{
		PoolID: execution.WorkerPool.ID, PoolVersion: execution.WorkerPool.Version, CapacityClass: execution.WorkerPool.CapacityClass,
		ReleaseRevision: optionalUUIDString(execution.WorkerReleaseRevisionID),
		ReleaseChannel:  stringValue(execution.WorkerReleaseChannel),
	}, true
}

func kubernetesWarmCapacityKeyForPlan(plan kubernetesWarmPodPlan) (kubernetesWarmCapacityKey, bool) {
	if (plan.Release.RevisionID == nil) != (plan.Release.Channel == nil) {
		return kubernetesWarmCapacityKey{}, false
	}
	return kubernetesWarmCapacityKey{
		PoolID: plan.Pool.ID, PoolVersion: plan.Pool.Version, CapacityClass: plan.Pool.CapacityClass,
		ReleaseRevision: optionalUUIDString(plan.Release.RevisionID),
		ReleaseChannel:  stringValue(plan.Release.Channel),
	}, true
}

func optionalUUIDString(value *uuid.UUID) string {
	if value == nil {
		return ""
	}
	return value.String()
}

func sameOptionalUUID(left, right *uuid.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func sameOptionalString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func kubernetesWarmPodPlans(
	warmPools []kubernetesWarmPool,
	warmPoolsSupported bool,
	warmClaimedCounts map[uuid.UUID]int,
	warmRelease kubernetesWarmReleaseSelection,
	podBaseHash string,
	baseImage string,
) ([]kubernetesWarmPodPlan, map[string]kubernetesWarmPodPlan, error) {
	plans := make([]kubernetesWarmPodPlan, 0)
	plansByName := make(map[string]kubernetesWarmPodPlan)
	if !warmPoolsSupported {
		return plans, plansByName, nil
	}
	for _, pool := range warmPools {
		if pool.Status != placement.PoolStatusActive {
			continue
		}
		claimed := warmClaimedCounts[pool.ID]
		if claimed < 0 {
			claimed = 0
		}
		desiredTotal := pool.DesiredIdleUnits + claimed
		if desiredTotal > pool.MaxActiveUnits {
			desiredTotal = pool.MaxActiveUnits
		}
		for slot := 0; slot < desiredTotal; slot++ {
			plan := kubernetesWarmPodPlan{Pool: pool, Slot: slot, Release: warmRelease}
			configHash, err := kubernetesWarmPoolPodHash(podBaseHash, baseImage, plan)
			if err != nil {
				return nil, nil, err
			}
			plan.ConfigHash = configHash
			plans = append(plans, plan)
			plansByName[kubernetesWarmPodName(plan)] = plan
		}
	}
	return plans, plansByName, nil
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

func kubernetesPodName(execution kubernetesExecution) string {
	generation := execution.Generation
	if execution.Status == "queued" || execution.Status == "recovering" {
		generation++
	}
	compactID := strings.ReplaceAll(execution.ID.String(), "-", "")
	return "synara-exec-" + compactID[:28] + "-g" + strconv.FormatInt(generation, 16)
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
	kubernetesNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	quantityPattern       = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?(?:m|Ki|Mi|Gi|Ti|Pi|Ei)?$`)
)

func (r *KubernetesReconciler) normalizeKubernetes(
	target persistence.ExecutionTarget,
	configuration kubernetesTargetConfiguration,
) (kubernetesTargetConfiguration, error) {
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
		if _, _, err := net.ParseCIDR(strings.TrimSpace(cidr)); err != nil {
			return kubernetesTargetConfiguration{}, problem.New(400, "invalid_kubernetes_configuration", "Kubernetes egressCidrs contains an invalid CIDR.")
		}
	}
	for _, value := range configuration.RunnerCommand {
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\r\n\x00") {
			return kubernetesTargetConfiguration{}, problem.New(400, "invalid_kubernetes_configuration", "Kubernetes runnerCommand is invalid.")
		}
	}
	for _, value := range []string{
		configuration.CPURequest, configuration.CPULimit, configuration.MemoryRequest, configuration.MemoryLimit,
		configuration.EphemeralStorageRequest, configuration.EphemeralStorageLimit, configuration.WorkspaceSizeLimit,
		configuration.QuotaCPURequests, configuration.QuotaCPULimits, configuration.QuotaMemoryRequests,
		configuration.QuotaMemoryLimits, configuration.QuotaEphemeralStorage,
	} {
		if value != "" && !quantityPattern.MatchString(value) {
			return kubernetesTargetConfiguration{}, problem.New(400, "invalid_kubernetes_configuration", "Kubernetes resource quantities are invalid.")
		}
	}
	return configuration, nil
}

func (r *KubernetesReconciler) foundationHash(
	target persistence.ExecutionTarget,
	configuration kubernetesTargetConfiguration,
) (string, error) {
	capabilities, err := json.Marshal(target.Capabilities)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(struct {
		Configuration kubernetesTargetConfiguration
		Capabilities  json.RawMessage
		LeaseRenew    time.Duration
	}{configuration, capabilities, workertiming.LeaseRenewInterval(r.config.WorkerLeaseTTL)})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func (r *KubernetesReconciler) applyFoundation(
	ctx context.Context,
	client kubernetesClient,
	target persistence.ExecutionTarget,
	configuration kubernetesTargetConfiguration,
	credential *ImagePullCredential,
) error {
	labels := kubernetesTargetLabels(target)
	if configuration.ManageNamespace != nil && *configuration.ManageNamespace {
		namespace := map[string]any{
			"apiVersion": "v1", "kind": "Namespace",
			"metadata": map[string]any{
				"name":   configuration.Namespace,
				"labels": kubernetesManagedNamespaceLabels(labels),
			},
		}
		if err := client.Apply(ctx, "/api/v1/namespaces/"+url.PathEscape(configuration.Namespace), namespace); err != nil {
			return problem.Wrap(502, "kubernetes_namespace_apply_failed", "Kubernetes Worker Namespace could not be applied.", err)
		}
	}
	serviceAccount := map[string]any{
		"apiVersion": "v1", "kind": "ServiceAccount",
		"metadata":                     map[string]any{"name": configuration.ServiceAccountName, "namespace": configuration.Namespace, "labels": labels},
		"automountServiceAccountToken": false,
	}
	if err := client.Apply(ctx, kubernetesNamespacedPath(configuration.Namespace, "serviceaccounts", configuration.ServiceAccountName), serviceAccount); err != nil {
		return problem.Wrap(502, "kubernetes_service_account_apply_failed", "Kubernetes Worker ServiceAccount could not be applied.", err)
	}
	registrySecret, err := kubernetesRegistrySecret(target, configuration.Namespace, labels, credential)
	if err != nil {
		return err
	}
	registrySecretName := kubernetesRegistrySecretName(target.ID)
	if err := client.Apply(ctx, kubernetesNamespacedPath(configuration.Namespace, "secrets", registrySecretName), registrySecret); err != nil {
		return problem.Wrap(502, "kubernetes_registry_secret_apply_failed", "Kubernetes Worker Registry Secret could not be applied.", err)
	}
	hard := map[string]any{"pods": strconv.Itoa(configuration.MaxActivePods)}
	for key, value := range map[string]string{
		"requests.cpu": configuration.QuotaCPURequests, "limits.cpu": configuration.QuotaCPULimits,
		"requests.memory": configuration.QuotaMemoryRequests, "limits.memory": configuration.QuotaMemoryLimits,
		"requests.ephemeral-storage": configuration.QuotaEphemeralStorage,
	} {
		if value != "" {
			hard[key] = value
		}
	}
	quotaName := "synara-agentd-" + strings.ReplaceAll(target.ID.String(), "-", "")[:12]
	quota := map[string]any{
		"apiVersion": "v1", "kind": "ResourceQuota",
		"metadata": map[string]any{"name": quotaName, "namespace": configuration.Namespace, "labels": labels},
		"spec":     map[string]any{"hard": hard},
	}
	if err := client.Apply(ctx, kubernetesNamespacedPath(configuration.Namespace, "resourcequotas", quotaName), quota); err != nil {
		return problem.Wrap(502, "kubernetes_quota_apply_failed", "Kubernetes Worker ResourceQuota could not be applied.", err)
	}
	ipBlocks := make([]any, 0, len(configuration.EgressCIDRs))
	for _, cidr := range configuration.EgressCIDRs {
		ipBlocks = append(ipBlocks, map[string]any{"ipBlock": map[string]any{"cidr": strings.TrimSpace(cidr)}})
	}
	networkPolicy := map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
		"metadata": map[string]any{"name": quotaName, "namespace": configuration.Namespace, "labels": labels},
		"spec": map[string]any{
			"podSelector": map[string]any{"matchLabels": map[string]any{kubernetesTargetLabel: target.ID.String()}},
			"policyTypes": []any{"Ingress", "Egress"}, "ingress": []any{},
			"egress": []any{
				map[string]any{"ports": []any{map[string]any{"protocol": "UDP", "port": 53}, map[string]any{"protocol": "TCP", "port": 53}}},
				map[string]any{"to": ipBlocks},
			},
		},
	}
	if err := client.Apply(ctx, kubernetesNetworkPolicyPath(configuration.Namespace, quotaName), networkPolicy); err != nil {
		return problem.Wrap(502, "kubernetes_network_policy_apply_failed", "Kubernetes Worker NetworkPolicy could not be applied.", err)
	}
	return nil
}

func (r *KubernetesReconciler) executionPod(
	target persistence.ExecutionTarget,
	configuration kubernetesTargetConfiguration,
	configHash string,
	execution kubernetesExecution,
	credential *ImagePullCredential,
) (map[string]any, error) {
	runner, err := json.Marshal(configuration.RunnerCommand)
	if err != nil {
		return nil, err
	}
	capabilities, err := json.Marshal(target.Capabilities)
	if err != nil {
		return nil, err
	}
	placementSnapshot, err := kubernetesExecutionPodPlacement(configuration, execution)
	if err != nil {
		return nil, err
	}
	generation := execution.Generation + 1
	labels := kubernetesTargetLabels(target)
	labels["synara.io/project-id"] = execution.ProjectID.String()
	labels["synara.io/session-id"] = execution.SessionID.String()
	labels[kubernetesExecutionLabel] = execution.ID.String()
	labels[kubernetesGenerationLabel] = strconv.FormatInt(generation, 10)
	labels[kubernetesWorkerModeLabel] = kubernetesWorkerModeExecutionPinned
	image, err := kubernetesExecutionImage(configuration.Image, execution)
	if err != nil {
		return nil, err
	}
	if err := validateKubernetesImagePullCredential(image, credential); err != nil {
		return nil, err
	}
	if execution.WorkerReleaseRevisionID != nil {
		labels[kubernetesReleaseLabel] = execution.WorkerReleaseRevisionID.String()
		labels[kubernetesChannelLabel] = stringValue(execution.WorkerReleaseChannel)
	}
	requests := map[string]any{}
	limits := map[string]any{}
	for key, value := range map[string]string{"cpu": configuration.CPURequest, "memory": configuration.MemoryRequest, "ephemeral-storage": configuration.EphemeralStorageRequest} {
		if value != "" {
			requests[key] = value
		}
	}
	for key, value := range map[string]string{"cpu": configuration.CPULimit, "memory": configuration.MemoryLimit, "ephemeral-storage": configuration.EphemeralStorageLimit} {
		if value != "" {
			limits[key] = value
		}
	}
	gitCacheRoot := "/data/git-cache"
	if configuration.GitCachePersistentVolumeClaim != "" {
		gitCacheRoot = "/git-cache"
	}
	environment := []any{
		map[string]any{"name": "SYNARA_CONTROL_PLANE_URL", "value": configuration.ControlPlaneURL},
		map[string]any{"name": "SYNARA_WORKER_REGISTRATION_TOKEN_FILE", "value": kubernetesWorkloadIdentityTokenPath},
		map[string]any{"name": "SYNARA_EXECUTION_TARGET_ID", "value": target.ID.String()},
		map[string]any{"name": "SYNARA_EXECUTION_TARGET_KIND", "value": "kubernetes"},
		map[string]any{"name": "SYNARA_AGENTD_ASSIGNED_EXECUTION_ID", "value": execution.ID.String()},
		map[string]any{"name": "SYNARA_AGENTD_CLUSTER_ID", "value": placementSnapshot.ClusterID},
		map[string]any{"name": "SYNARA_AGENTD_NAMESPACE", "value": placementSnapshot.Namespace},
		map[string]any{"name": "SYNARA_AGENTD_INSTANCE_ID", "valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "metadata.name"}}},
		map[string]any{"name": "SYNARA_AGENTD_INSTANCE_UID", "valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "metadata.uid"}}},
		map[string]any{"name": "SYNARA_AGENTD_CAPABILITIES_JSON", "value": string(capabilities)},
		map[string]any{"name": "SYNARA_AGENTD_RUNNER_COMMAND_JSON", "value": string(runner)},
		map[string]any{"name": "SYNARA_AGENTD_PROVIDER_HOST_PROTOCOL", "value": "v2"},
		map[string]any{"name": "SYNARA_AGENTD_LEASE_RENEW_INTERVAL", "value": workertiming.LeaseRenewInterval(r.config.WorkerLeaseTTL).String()},
		map[string]any{"name": "SYNARA_AGENTD_DRAIN_TIMEOUT", "value": "20s"},
		map[string]any{"name": "SYNARA_AGENTD_WORKSPACE_ROOT", "value": "/data/workspaces"},
		map[string]any{"name": "SYNARA_AGENTD_GIT_CACHE_ROOT", "value": gitCacheRoot},
	}
	if digest := immutableImageDigest(image); digest != "" {
		environment = append(environment, map[string]any{"name": "SYNARA_AGENTD_IMAGE_DIGEST", "value": digest})
	}
	volumes := []any{
		map[string]any{"name": "workspace", "emptyDir": map[string]any{}},
		map[string]any{"name": "tmp", "emptyDir": map[string]any{}},
		map[string]any{"name": "home", "emptyDir": map[string]any{}},
		map[string]any{
			"name": kubernetesWorkloadIdentityVolume,
			"projected": map[string]any{
				"defaultMode": 0o440,
				"sources": []any{map[string]any{
					"serviceAccountToken": map[string]any{
						"audience":          KubernetesWorkerRegistrationAudience(target.ID),
						"expirationSeconds": 600, "path": "token",
					},
				}},
			},
		},
	}
	if configuration.WorkspaceSizeLimit != "" {
		volumes[0] = map[string]any{"name": "workspace", "emptyDir": map[string]any{"sizeLimit": configuration.WorkspaceSizeLimit}}
	}
	volumeMounts := []any{
		map[string]any{"name": "workspace", "mountPath": "/data"},
		map[string]any{"name": "tmp", "mountPath": "/tmp"},
		map[string]any{"name": "home", "mountPath": "/home/synara"},
		map[string]any{
			"name":      kubernetesWorkloadIdentityVolume,
			"mountPath": "/var/run/secrets/synara.io/workload-identity", "readOnly": true,
		},
	}
	if configuration.GitCachePersistentVolumeClaim != "" {
		volumes = append(volumes, map[string]any{
			"name": "git-cache", "persistentVolumeClaim": map[string]any{"claimName": configuration.GitCachePersistentVolumeClaim},
		})
		volumeMounts = append(volumeMounts, map[string]any{"name": "git-cache", "mountPath": "/git-cache"})
	}
	container := map[string]any{
		"name": "agentd", "image": image, "imagePullPolicy": configuration.ImagePullPolicy,
		"command": []any{"/usr/local/bin/synara-agentd"}, "env": environment,
		"workingDir": "/data", "volumeMounts": volumeMounts,
		"securityContext": map[string]any{
			"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
			"runAsNonRoot": true, "runAsUser": 10001, "runAsGroup": 10001,
			"capabilities":   map[string]any{"drop": []any{"ALL"}},
			"seccompProfile": map[string]any{"type": "RuntimeDefault"},
		},
		"resources": map[string]any{"requests": requests, "limits": limits},
	}
	podSpec := map[string]any{
		"serviceAccountName": configuration.ServiceAccountName, "automountServiceAccountToken": false,
		"enableServiceLinks": false, "hostNetwork": false, "hostPID": false, "hostIPC": false,
		"restartPolicy": "Never", "terminationGracePeriodSeconds": 30,
		"securityContext": map[string]any{"runAsNonRoot": true, "fsGroup": 10001, "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
		"containers":      []any{container}, "volumes": volumes,
	}
	if len(configuration.NodeSelector) > 0 {
		podSpec["nodeSelector"] = cloneStringMap(configuration.NodeSelector)
	}
	if len(configuration.Tolerations) > 0 {
		podSpec["tolerations"] = cloneObjectList(configuration.Tolerations)
	}
	if configuration.RequireNodeSpread {
		podSpec["topologySpreadConstraints"] = kubernetesNodeSpreadConstraints(target.ID)
	}
	if err := applyKubernetesWorkerPoolSchedulingTemplate(podSpec, placementSnapshot.SchedulingTemplate); err != nil {
		return nil, err
	}
	if len(configuration.ImagePullSecrets) > 0 || credential != nil {
		secrets := make([]any, 0, len(configuration.ImagePullSecrets)+1)
		seen := make(map[string]struct{}, len(configuration.ImagePullSecrets)+1)
		for _, name := range configuration.ImagePullSecrets {
			if _, duplicate := seen[name]; duplicate {
				continue
			}
			secrets = append(secrets, map[string]any{"name": name})
			seen[name] = struct{}{}
		}
		if credential != nil {
			name := kubernetesRegistrySecretName(target.ID)
			if _, duplicate := seen[name]; !duplicate {
				secrets = append(secrets, map[string]any{"name": name})
			}
		}
		podSpec["imagePullSecrets"] = secrets
	}
	return map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{
			"name": kubernetesPodName(execution), "namespace": configuration.Namespace, "labels": labels,
			"annotations": map[string]any{kubernetesConfigAnnotation: configHash},
		},
		"spec": podSpec,
	}, nil
}

type kubernetesExecutionPodPlacementSnapshot struct {
	ClusterID          string
	Namespace          string
	SchedulingTemplate map[string]any
}

func kubernetesExecutionPodPlacement(
	configuration kubernetesTargetConfiguration,
	execution kubernetesExecution,
) (kubernetesExecutionPodPlacementSnapshot, error) {
	placementSnapshot := kubernetesExecutionPodPlacementSnapshot{
		ClusterID:          kubernetesLocalClusterID,
		Namespace:          configuration.Namespace,
		SchedulingTemplate: map[string]any{},
	}
	if execution.WorkerPool == nil {
		return placementSnapshot, nil
	}
	clusterID := strings.TrimSpace(execution.WorkerPool.ClusterID)
	if clusterID != "" && clusterID != kubernetesLocalClusterID {
		return kubernetesExecutionPodPlacementSnapshot{}, problem.New(
			409,
			"worker_pool_cluster_mismatch",
			"Worker pool cluster does not match the Kubernetes execution target cluster.",
		)
	}
	if namespace := strings.TrimSpace(execution.WorkerPool.Namespace); namespace != "" && namespace != configuration.Namespace {
		return kubernetesExecutionPodPlacementSnapshot{}, problem.New(
			409,
			"worker_pool_namespace_mismatch",
			"Worker pool namespace does not match the Kubernetes execution target namespace.",
		)
	}
	placementSnapshot.SchedulingTemplate = execution.WorkerPool.SchedulingTemplate
	return placementSnapshot, nil
}

func (r *KubernetesReconciler) warmPoolPod(
	target persistence.ExecutionTarget,
	configuration kubernetesTargetConfiguration,
	plan kubernetesWarmPodPlan,
	credential *ImagePullCredential,
) (map[string]any, error) {
	runner, err := json.Marshal(configuration.RunnerCommand)
	if err != nil {
		return nil, err
	}
	capabilities, err := json.Marshal(target.Capabilities)
	if err != nil {
		return nil, err
	}
	clusterID := strings.TrimSpace(plan.Pool.ClusterID)
	if clusterID == "" {
		clusterID = kubernetesLocalClusterID
	}
	if clusterID != kubernetesLocalClusterID {
		return nil, problem.New(409, "worker_pool_cluster_mismatch", "Warm Worker pool cluster does not match the Kubernetes execution target cluster.")
	}
	if namespace := strings.TrimSpace(plan.Pool.Namespace); namespace != "" && namespace != configuration.Namespace {
		return nil, problem.New(409, "worker_pool_namespace_mismatch", "Warm Worker pool namespace does not match the Kubernetes execution target namespace.")
	}
	image, err := kubernetesWarmPoolImage(configuration.Image, plan.Release)
	if err != nil {
		return nil, err
	}
	if err := validateKubernetesImagePullCredential(image, credential); err != nil {
		return nil, err
	}
	labels := kubernetesTargetLabels(target)
	labels[kubernetesWorkerModeLabel] = kubernetesWorkerModeWarmPool
	labels[kubernetesWorkerPoolIDLabel] = plan.Pool.ID.String()
	labels[kubernetesWorkerPoolVersionLabel] = strconv.FormatInt(plan.Pool.Version, 10)
	labels[kubernetesCapacityClassLabel] = plan.Pool.CapacityClass
	labels[kubernetesWarmSlotLabel] = strconv.Itoa(plan.Slot)
	if plan.Release.RevisionID != nil {
		labels[kubernetesReleaseLabel] = plan.Release.RevisionID.String()
		labels[kubernetesChannelLabel] = stringValue(plan.Release.Channel)
	}
	requests := map[string]any{}
	limits := map[string]any{}
	for key, value := range map[string]string{"cpu": configuration.CPURequest, "memory": configuration.MemoryRequest, "ephemeral-storage": configuration.EphemeralStorageRequest} {
		if value != "" {
			requests[key] = value
		}
	}
	for key, value := range map[string]string{"cpu": configuration.CPULimit, "memory": configuration.MemoryLimit, "ephemeral-storage": configuration.EphemeralStorageLimit} {
		if value != "" {
			limits[key] = value
		}
	}
	gitCacheRoot := "/data/git-cache"
	if configuration.GitCachePersistentVolumeClaim != "" {
		gitCacheRoot = "/git-cache"
	}
	environment := []any{
		map[string]any{"name": "SYNARA_CONTROL_PLANE_URL", "value": configuration.ControlPlaneURL},
		map[string]any{"name": "SYNARA_WORKER_REGISTRATION_TOKEN_FILE", "value": kubernetesWorkloadIdentityTokenPath},
		map[string]any{"name": "SYNARA_EXECUTION_TARGET_ID", "value": target.ID.String()},
		map[string]any{"name": "SYNARA_EXECUTION_TARGET_KIND", "value": "kubernetes"},
		map[string]any{"name": "SYNARA_AGENTD_WORKER_MODE", "value": kubernetesWorkerModeWarmPool},
		map[string]any{"name": "SYNARA_AGENTD_CLUSTER_ID", "value": clusterID},
		map[string]any{"name": "SYNARA_AGENTD_NAMESPACE", "value": configuration.Namespace},
		map[string]any{"name": "SYNARA_AGENTD_INSTANCE_ID", "valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "metadata.name"}}},
		map[string]any{"name": "SYNARA_AGENTD_INSTANCE_UID", "valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "metadata.uid"}}},
		map[string]any{"name": "SYNARA_AGENTD_CAPABILITIES_JSON", "value": string(capabilities)},
		map[string]any{"name": "SYNARA_AGENTD_RUNNER_COMMAND_JSON", "value": string(runner)},
		map[string]any{"name": "SYNARA_AGENTD_PROVIDER_HOST_PROTOCOL", "value": "v2"},
		map[string]any{"name": "SYNARA_AGENTD_LEASE_RENEW_INTERVAL", "value": workertiming.LeaseRenewInterval(r.config.WorkerLeaseTTL).String()},
		map[string]any{"name": "SYNARA_AGENTD_DRAIN_TIMEOUT", "value": "20s"},
		map[string]any{"name": "SYNARA_AGENTD_WORKSPACE_ROOT", "value": "/data/workspaces"},
		map[string]any{"name": "SYNARA_AGENTD_GIT_CACHE_ROOT", "value": gitCacheRoot},
	}
	if digest := immutableImageDigest(image); digest != "" {
		environment = append(environment, map[string]any{"name": "SYNARA_AGENTD_IMAGE_DIGEST", "value": digest})
	}
	volumes := []any{
		map[string]any{"name": "workspace", "emptyDir": map[string]any{}},
		map[string]any{"name": "tmp", "emptyDir": map[string]any{}},
		map[string]any{"name": "home", "emptyDir": map[string]any{}},
		map[string]any{
			"name": kubernetesWorkloadIdentityVolume,
			"projected": map[string]any{
				"defaultMode": 0o440,
				"sources": []any{map[string]any{
					"serviceAccountToken": map[string]any{
						"audience":          KubernetesWorkerRegistrationAudience(target.ID),
						"expirationSeconds": 600, "path": "token",
					},
				}},
			},
		},
	}
	if configuration.WorkspaceSizeLimit != "" {
		volumes[0] = map[string]any{"name": "workspace", "emptyDir": map[string]any{"sizeLimit": configuration.WorkspaceSizeLimit}}
	}
	volumeMounts := []any{
		map[string]any{"name": "workspace", "mountPath": "/data"},
		map[string]any{"name": "tmp", "mountPath": "/tmp"},
		map[string]any{"name": "home", "mountPath": "/home/synara"},
		map[string]any{
			"name":      kubernetesWorkloadIdentityVolume,
			"mountPath": "/var/run/secrets/synara.io/workload-identity", "readOnly": true,
		},
	}
	if configuration.GitCachePersistentVolumeClaim != "" {
		volumes = append(volumes, map[string]any{
			"name": "git-cache", "persistentVolumeClaim": map[string]any{"claimName": configuration.GitCachePersistentVolumeClaim},
		})
		volumeMounts = append(volumeMounts, map[string]any{"name": "git-cache", "mountPath": "/git-cache"})
	}
	container := map[string]any{
		"name": "agentd", "image": image, "imagePullPolicy": configuration.ImagePullPolicy,
		"command": []any{"/usr/local/bin/synara-agentd"}, "env": environment,
		"workingDir": "/data", "volumeMounts": volumeMounts,
		"securityContext": map[string]any{
			"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
			"runAsNonRoot": true, "runAsUser": 10001, "runAsGroup": 10001,
			"capabilities":   map[string]any{"drop": []any{"ALL"}},
			"seccompProfile": map[string]any{"type": "RuntimeDefault"},
		},
		"resources": map[string]any{"requests": requests, "limits": limits},
	}
	podSpec := map[string]any{
		"serviceAccountName": configuration.ServiceAccountName, "automountServiceAccountToken": false,
		"enableServiceLinks": false, "hostNetwork": false, "hostPID": false, "hostIPC": false,
		"restartPolicy": "Never", "terminationGracePeriodSeconds": 30,
		"securityContext": map[string]any{"runAsNonRoot": true, "fsGroup": 10001, "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
		"containers":      []any{container}, "volumes": volumes,
	}
	if len(configuration.NodeSelector) > 0 {
		podSpec["nodeSelector"] = cloneStringMap(configuration.NodeSelector)
	}
	if len(configuration.Tolerations) > 0 {
		podSpec["tolerations"] = cloneObjectList(configuration.Tolerations)
	}
	if configuration.RequireNodeSpread {
		podSpec["topologySpreadConstraints"] = kubernetesNodeSpreadConstraints(target.ID)
	}
	if err := applyKubernetesWorkerPoolSchedulingTemplate(podSpec, plan.Pool.SchedulingTemplate); err != nil {
		return nil, err
	}
	if len(configuration.ImagePullSecrets) > 0 || credential != nil {
		secrets := make([]any, 0, len(configuration.ImagePullSecrets)+1)
		seen := make(map[string]struct{}, len(configuration.ImagePullSecrets)+1)
		for _, name := range configuration.ImagePullSecrets {
			if _, duplicate := seen[name]; duplicate {
				continue
			}
			secrets = append(secrets, map[string]any{"name": name})
			seen[name] = struct{}{}
		}
		if credential != nil {
			name := kubernetesRegistrySecretName(target.ID)
			if _, duplicate := seen[name]; !duplicate {
				secrets = append(secrets, map[string]any{"name": name})
			}
		}
		podSpec["imagePullSecrets"] = secrets
	}
	return map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{
			"name": kubernetesWarmPodName(plan), "namespace": configuration.Namespace, "labels": labels,
			"annotations": map[string]any{kubernetesConfigAnnotation: plan.ConfigHash},
		},
		"spec": podSpec,
	}, nil
}

func kubernetesWarmPodName(plan kubernetesWarmPodPlan) string {
	compactPoolID := strings.ReplaceAll(plan.Pool.ID.String(), "-", "")
	releaseKey := "unmanaged"
	if plan.Release.RevisionID != nil && plan.Release.Channel != nil {
		compactReleaseID := strings.ReplaceAll(plan.Release.RevisionID.String(), "-", "")
		channelKey := "p"
		if *plan.Release.Channel == "canary" {
			channelKey = "c"
		}
		releaseKey = channelKey + compactReleaseID[:8]
	}
	return fmt.Sprintf("synara-warm-%s-v%s-%s-s%d", compactPoolID[:10], strconv.FormatInt(plan.Pool.Version, 16), releaseKey, plan.Slot)
}

func kubernetesWarmPoolImage(baseImage string, release kubernetesWarmReleaseSelection) (string, error) {
	if release.RevisionID == nil && release.Channel == nil && release.ImageDigest == nil {
		return baseImage, nil
	}
	if release.RevisionID == nil || release.Channel == nil || release.ImageDigest == nil ||
		(*release.Channel != "promoted" && *release.Channel != "canary") {
		return "", problem.New(409, "worker_release_execution_invalid", "Warm Worker release selection is invalid.")
	}
	return pinImageReference(baseImage, *release.ImageDigest)
}

func kubernetesWarmPoolPodHash(baseHash, baseImage string, plan kubernetesWarmPodPlan) (string, error) {
	image, err := kubernetesWarmPoolImage(baseImage, plan.Release)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(struct {
		BaseHash      string
		Image         string
		PoolID        uuid.UUID
		PoolVersion   int64
		CapacityClass string
		Slot          int
		RevisionID    *uuid.UUID
		Channel       *string
	}{baseHash, image, plan.Pool.ID, plan.Pool.Version, plan.Pool.CapacityClass, plan.Slot, plan.Release.RevisionID, plan.Release.Channel})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func applyKubernetesWorkerPoolSchedulingTemplate(podSpec map[string]any, template map[string]any) error {
	if len(template) == 0 {
		return nil
	}
	for key, value := range template {
		switch key {
		case "priorityClassName":
			text, ok := value.(string)
			if !ok || strings.TrimSpace(text) == "" || strings.ContainsAny(text, "\r\n\t") {
				return problem.New(409, "worker_pool_scheduling_template_invalid", "Worker pool priorityClassName must be a non-empty string.")
			}
			podSpec["priorityClassName"] = strings.TrimSpace(text)
		case "nodeSelector":
			nodeSelector, err := normalizeSchedulingTemplateStringMap(value, "nodeSelector")
			if err != nil {
				return err
			}
			merged := map[string]string{}
			if existing, ok := podSpec["nodeSelector"].(map[string]string); ok {
				merged = cloneStringMap(existing)
			}
			for nodeKey, nodeValue := range nodeSelector {
				merged[nodeKey] = nodeValue
			}
			podSpec["nodeSelector"] = merged
		case "tolerations":
			tolerations, err := normalizeSchedulingTemplateTolerations(value)
			if err != nil {
				return err
			}
			existing := make([]any, 0)
			if current, ok := podSpec["tolerations"].([]any); ok {
				existing = append(existing, current...)
			}
			podSpec["tolerations"] = append(existing, tolerations...)
		default:
			return problem.New(409, "worker_pool_scheduling_template_unsupported", "Worker pool schedulingTemplate contains an unsupported field.")
		}
	}
	return nil
}

func normalizeSchedulingTemplateStringMap(value any, field string) (map[string]string, error) {
	raw, ok := value.(map[string]any)
	if !ok {
		return nil, problem.New(409, "worker_pool_scheduling_template_invalid", "Worker pool "+field+" must be an object.")
	}
	normalized := make(map[string]string, len(raw))
	for key, item := range raw {
		text, ok := item.(string)
		if !ok || strings.TrimSpace(key) == "" || strings.TrimSpace(text) == "" || strings.ContainsAny(key+text, "\r\n\t") {
			return nil, problem.New(409, "worker_pool_scheduling_template_invalid", "Worker pool "+field+" values must be non-empty strings.")
		}
		normalized[strings.TrimSpace(key)] = strings.TrimSpace(text)
	}
	return normalized, nil
}

func normalizeSchedulingTemplateTolerations(value any) ([]any, error) {
	items, ok := value.([]any)
	if !ok {
		return nil, problem.New(409, "worker_pool_scheduling_template_invalid", "Worker pool tolerations must be an array.")
	}
	normalized := make([]any, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, problem.New(409, "worker_pool_scheduling_template_invalid", "Worker pool tolerations must contain objects.")
		}
		clone := make(map[string]any, len(object))
		for key, value := range object {
			clone[key] = value
		}
		normalized = append(normalized, clone)
	}
	return normalized, nil
}

func cloneStringMap(input map[string]string) map[string]string {
	cloned := make(map[string]string, len(input))
	for key, value := range input {
		cloned[key] = value
	}
	return cloned
}

func cloneObjectList(input []map[string]any) []any {
	cloned := make([]any, 0, len(input))
	for _, item := range input {
		copy := make(map[string]any, len(item))
		for key, value := range item {
			copy[key] = value
		}
		cloned = append(cloned, copy)
	}
	return cloned
}

func kubernetesNodeSpreadConstraints(targetID uuid.UUID) []any {
	return []any{
		map[string]any{
			"maxSkew":           1,
			"topologyKey":       "kubernetes.io/hostname",
			"whenUnsatisfiable": "DoNotSchedule",
			"labelSelector": map[string]any{
				"matchLabels": map[string]any{
					kubernetesTargetLabel: targetID.String(),
				},
			},
		},
	}
}

func kubernetesTargetLabels(target persistence.ExecutionTarget) map[string]string {
	labels := map[string]string{
		kubernetesManagedLabel: "true", kubernetesTargetLabel: target.ID.String(),
	}
	if target.TenantID != nil {
		labels["synara.io/tenant-id"] = target.TenantID.String()
	}
	if target.OrganizationID != nil {
		labels["synara.io/organization-id"] = target.OrganizationID.String()
	}
	return labels
}

func kubernetesManagedNamespaceLabels(labels map[string]string) map[string]string {
	namespaced := make(map[string]string, len(labels)+3)
	for key, value := range labels {
		namespaced[key] = value
	}
	namespaced["pod-security.kubernetes.io/enforce"] = "restricted"
	namespaced["pod-security.kubernetes.io/audit"] = "restricted"
	namespaced["pod-security.kubernetes.io/warn"] = "restricted"
	return namespaced
}

func kubernetesSecretName(targetID uuid.UUID) string {
	return "synara-agentd-" + strings.ReplaceAll(targetID.String(), "-", "")[:12]
}

func kubernetesRegistrySecretName(targetID uuid.UUID) string {
	return kubernetesSecretName(targetID) + "-registry"
}

func kubernetesRegistrySecret(
	target persistence.ExecutionTarget,
	namespace string,
	labels map[string]string,
	credential *ImagePullCredential,
) (map[string]any, error) {
	auths := map[string]any{}
	if credential != nil {
		if credential.RegistryToken != "" {
			return nil, problem.New(
				409,
				"worker_image_pull_bearer_unsupported",
				"Kubernetes Worker image pull Secrets require an OCI Registry basic Credential.",
			)
		}
		authority, err := normalizeRegistryAuthority(credential.Host)
		if err != nil {
			return nil, problem.New(500, "worker_image_pull_credential_invalid", "Worker image pull Credential projection is invalid.")
		}
		auths[registryAuthServerAddress(authority)] = map[string]any{
			"username": credential.Username,
			"password": credential.Password,
			"auth":     base64.StdEncoding.EncodeToString([]byte(credential.Username + ":" + credential.Password)),
		}
	}
	config, err := json.Marshal(map[string]any{"auths": auths})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/dockerconfigjson",
		"metadata": map[string]any{
			"name": kubernetesRegistrySecretName(target.ID), "namespace": namespace, "labels": labels,
		},
		"data": map[string]any{".dockerconfigjson": base64.StdEncoding.EncodeToString(config)},
	}, nil
}

func kubernetesFoundationApplyHash(baseHash string, credential *ImagePullCredential) (string, error) {
	var identity any
	if credential != nil {
		identity = struct {
			BindingID         uuid.UUID
			CredentialID      uuid.UUID
			CredentialVersion int
			Host              string
		}{credential.BindingID, credential.CredentialID, credential.CredentialVersion, credential.Host}
	}
	payload, err := json.Marshal(struct {
		BaseHash   string
		Credential any
	}{baseHash, identity})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func kubernetesExecutionPodHash(baseHash, baseImage string, execution kubernetesExecution) (string, error) {
	image, err := kubernetesExecutionImage(baseImage, execution)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(struct {
		BaseHash   string
		Image      string
		RevisionID *uuid.UUID
		Channel    *string
		WorkerPool *kubernetesExecutionPoolSnapshot
	}{baseHash, image, execution.WorkerReleaseRevisionID, execution.WorkerReleaseChannel, execution.WorkerPool})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func kubernetesExecutionImage(baseImage string, execution kubernetesExecution) (string, error) {
	if execution.WorkerReleaseRevisionID == nil && execution.WorkerReleaseChannel == nil && execution.WorkerReleaseImageDigest == nil {
		return baseImage, nil
	}
	if execution.WorkerReleaseRevisionID == nil || execution.WorkerReleaseChannel == nil ||
		execution.WorkerReleaseImageDigest == nil ||
		(*execution.WorkerReleaseChannel != "promoted" && *execution.WorkerReleaseChannel != "canary") {
		return "", problem.New(409, "worker_release_execution_invalid", "Execution Worker release selection is invalid.")
	}
	return pinImageReference(baseImage, *execution.WorkerReleaseImageDigest)
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

func validateKubernetesImagePullCredential(image string, credential *ImagePullCredential) error {
	if err := validateImagePullCredential(image, credential); err != nil {
		return err
	}
	if credential != nil && credential.RegistryToken != "" {
		return problem.New(
			409,
			"worker_image_pull_bearer_unsupported",
			"Kubernetes Worker image pull Secrets require an OCI Registry basic Credential.",
		)
	}
	return nil
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
		state := r.foundation[target.ID]
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
				r.foundation[target.ID] = kubernetesFoundationState{hash: clearHash, appliedAt: r.now()}
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

type kubernetesHTTPFactory struct{}

func (kubernetesHTTPFactory) Open(configuration kubernetesTargetConfiguration) (kubernetesClient, error) {
	return newKubernetesHTTPClient(configuration)
}

type kubernetesHTTPClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func (c *kubernetesHTTPClient) Apply(ctx context.Context, path string, object map[string]any) error {
	query := url.Values{"fieldManager": {"synara-control-plane"}, "force": {"true"}}
	return c.do(ctx, http.MethodPatch, path+"?"+query.Encode(), object, nil, http.StatusOK, http.StatusCreated)
}

func (c *kubernetesHTTPClient) ListPods(ctx context.Context, namespace string, targetID uuid.UUID) ([]kubernetesPod, error) {
	return c.listPods(ctx, namespace, kubernetesTargetLabel+"="+targetID.String())
}

func (c *kubernetesHTTPClient) ListPodUIDs(ctx context.Context, namespace string) ([]string, error) {
	pods, err := c.listPods(ctx, namespace, "")
	if err != nil {
		return nil, err
	}
	uids := make([]string, 0, len(pods))
	for _, pod := range pods {
		uids = append(uids, pod.UID)
	}
	return uids, nil
}

func (c *kubernetesHTTPClient) listPods(ctx context.Context, namespace, labelSelector string) ([]kubernetesPod, error) {
	items := make([]kubernetesPod, 0)
	continueToken := ""
	seenContinueTokens := map[string]struct{}{}
	for {
		var response struct {
			Metadata struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
			Items []struct {
				Metadata struct {
					Name              string            `json:"name"`
					UID               string            `json:"uid"`
					CreationTimestamp time.Time         `json:"creationTimestamp"`
					Labels            map[string]string `json:"labels"`
					Annotations       map[string]string `json:"annotations"`
				} `json:"metadata"`
				Status struct {
					Phase      string `json:"phase"`
					Reason     string `json:"reason"`
					Conditions []struct {
						Type   string `json:"type"`
						Status string `json:"status"`
						Reason string `json:"reason"`
					} `json:"conditions"`
					ContainerStatuses []struct {
						Name  string `json:"name"`
						State struct {
							Waiting *struct {
								Reason string `json:"reason"`
							} `json:"waiting"`
							Terminated *struct {
								ExitCode int    `json:"exitCode"`
								Reason   string `json:"reason"`
							} `json:"terminated"`
						} `json:"state"`
						LastState struct {
							Terminated *struct {
								Reason string `json:"reason"`
							} `json:"terminated"`
						} `json:"lastState"`
					} `json:"containerStatuses"`
				} `json:"status"`
			} `json:"items"`
		}
		query := url.Values{}
		query.Set("limit", "100")
		if labelSelector != "" {
			query.Set("labelSelector", labelSelector)
		}
		if continueToken != "" {
			query.Set("continue", continueToken)
		}
		path := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/pods"
		if encoded := query.Encode(); encoded != "" {
			path += "?" + encoded
		}
		if err := c.do(ctx, http.MethodGet, path, nil, &response, http.StatusOK); err != nil {
			return nil, err
		}
		for _, item := range response.Items {
			conditions := make([]kubernetesPodCondition, 0, len(item.Status.Conditions))
			for _, condition := range item.Status.Conditions {
				conditions = append(conditions, kubernetesPodCondition{
					Type: condition.Type, Status: condition.Status, Reason: condition.Reason,
				})
			}
			containers := make([]kubernetesContainerStatus, 0, len(item.Status.ContainerStatuses))
			for _, status := range item.Status.ContainerStatuses {
				container := kubernetesContainerStatus{Name: status.Name}
				if status.State.Waiting != nil {
					container.WaitingReason = status.State.Waiting.Reason
				}
				if status.State.Terminated != nil {
					container.Terminated = true
					container.ExitCode = status.State.Terminated.ExitCode
					container.TerminatedReason = status.State.Terminated.Reason
				}
				if status.LastState.Terminated != nil {
					container.LastTerminatedReason = status.LastState.Terminated.Reason
				}
				containers = append(containers, container)
			}
			items = append(items, kubernetesPod{
				Name: item.Metadata.Name, UID: item.Metadata.UID, Phase: item.Status.Phase,
				Reason: item.Status.Reason, CreatedAt: item.Metadata.CreationTimestamp.UTC(),
				Labels: item.Metadata.Labels, Annotations: item.Metadata.Annotations,
				Conditions: conditions, Containers: containers,
			})
		}
		continueToken = strings.TrimSpace(response.Metadata.Continue)
		if continueToken == "" {
			break
		}
		if _, seen := seenContinueTokens[continueToken]; seen {
			return nil, errors.New("Kubernetes Pod list repeated its continuation token")
		}
		seenContinueTokens[continueToken] = struct{}{}
	}
	return items, nil
}

func (c *kubernetesHTTPClient) DeletePod(ctx context.Context, namespace, name, uid string) error {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return errors.New("Kubernetes Pod UID is required for safe deletion")
	}
	path := kubernetesNamespacedPath(namespace, "pods", name) + "?gracePeriodSeconds=30&propagationPolicy=Background"
	err := c.do(ctx, http.MethodDelete, path, map[string]any{
		"apiVersion": "v1", "kind": "DeleteOptions",
		"preconditions": map[string]any{"uid": uid},
	}, nil, http.StatusOK, http.StatusAccepted, http.StatusNotFound)
	var statusErr *kubernetesAPIStatusError
	if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusConflict {
		return fmt.Errorf("%w: %s", errKubernetesPodUIDPreconditionFailed, statusErr.Detail)
	}
	return err
}

func (c *kubernetesHTTPClient) do(
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
		contentType := "application/json"
		if method == http.MethodPatch {
			contentType = "application/apply-patch+yaml"
		}
		request.Header.Set("Content-Type", contentType)
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
