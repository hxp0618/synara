package executiontargets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
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
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/workertiming"
)

const (
	kubernetesManagedLabel     = "synara.io/managed"
	kubernetesTargetLabel      = "synara.io/execution-target-id"
	kubernetesExecutionLabel   = "synara.io/execution-id"
	kubernetesGenerationLabel  = "synara.io/generation"
	kubernetesReleaseLabel     = "synara.io/worker-release-revision-id"
	kubernetesChannelLabel     = "synara.io/worker-release-channel"
	kubernetesConfigAnnotation = "synara.io/config-sha256"
)

type KubernetesReconcilerConfig struct {
	RegistrationToken                  string
	PublicControlPlaneURL              string
	WorkerLeaseTTL                     time.Duration
	Interval                           time.Duration
	RecoverExpired                     func(context.Context, int) error
	ReconcileEphemeralWorkspaceCleanup func(context.Context, uuid.UUID, []string, time.Time) (int, error)
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
	Labels      map[string]string
	Annotations map[string]string
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
		Where("kind = ? AND status <> ?", "kubernetes", "disabled").Order("id").Find(&targets).Error; err != nil {
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
	Status                   string     `gorm:"column:status"`
	Generation               int64      `gorm:"column:generation"`
	WorkerReleaseRevisionID  *uuid.UUID `gorm:"column:worker_release_revision_id"`
	WorkerReleaseChannel     *string    `gorm:"column:worker_release_channel"`
	WorkerReleaseImageDigest *string    `gorm:"column:worker_release_image_digest"`
}

func (r *KubernetesReconciler) reconcileTarget(ctx context.Context, target persistence.ExecutionTarget) error {
	configuration, err := r.loadConfiguration(target)
	if err != nil {
		r.setKubernetesStatus(ctx, target, "offline", false, false, 0, 0)
		return err
	}
	client, err := r.factory.Open(configuration)
	if err != nil {
		r.setKubernetesStatus(ctx, target, "offline", false, false, 0, 0)
		return problem.Wrap(503, "kubernetes_api_unavailable", "Kubernetes API configuration is unavailable.", err)
	}
	resolution, err := r.resolveImagePullCredential(ctx, target, configuration.Image)
	if err != nil {
		return r.rejectImagePullCredential(ctx, client, target, configuration, resolution.Authoritative, err)
	}
	credential := resolution.Credential
	if err := validateKubernetesImagePullCredential(configuration.Image, credential); err != nil {
		return r.rejectImagePullCredential(ctx, client, target, configuration, resolution.Authoritative, err)
	}
	podBaseHash, err := r.foundationHash(target, configuration)
	if err != nil {
		return err
	}
	foundationHash, err := kubernetesFoundationApplyHash(podBaseHash, credential)
	if err != nil {
		return err
	}
	state := r.foundation[target.ID]
	foundationChanged := state.hash != foundationHash || r.now().Sub(state.appliedAt) >= 5*time.Minute
	if foundationChanged {
		if err := r.applyFoundation(ctx, client, target, configuration, credential); err != nil {
			r.setKubernetesStatus(ctx, target, "offline", false, false, 0, 0)
			return err
		}
		r.foundation[target.ID] = kubernetesFoundationState{hash: foundationHash, appliedAt: r.now()}
	}
	executions, err := r.loadKubernetesExecutions(ctx, target.ID)
	if err != nil {
		return err
	}
	pods, err := client.ListPods(ctx, configuration.Namespace, target.ID)
	if err != nil {
		return problem.Wrap(502, "kubernetes_pods_load_failed", "Kubernetes Worker Pods could not be listed.", err)
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
	existing := make(map[string]kubernetesPod, len(pods))
	created, deleted := 0, 0
	for _, pod := range pods {
		executionID, parseErr := uuid.Parse(pod.Labels[kubernetesExecutionLabel])
		if parseErr != nil {
			if err := client.DeletePod(ctx, configuration.Namespace, pod.Name, pod.UID); err != nil {
				return problem.Wrap(502, "kubernetes_pod_delete_failed", "An obsolete Kubernetes Worker Pod could not be deleted.", err)
			}
			deleted++
			continue
		}
		execution, found := active[executionID]
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
		if !found || pod.Name != expectedName || pod.Annotations[kubernetesConfigAnnotation] != expectedHash || terminalPod {
			if err := client.DeletePod(ctx, configuration.Namespace, pod.Name, pod.UID); err != nil {
				return problem.Wrap(502, "kubernetes_pod_delete_failed", "An obsolete Kubernetes Worker Pod could not be deleted.", err)
			}
			deleted++
			continue
		}
		existing[pod.Name] = pod
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
		if scheduled >= configuration.MaxActivePods {
			break
		}
		name := kubernetesPodName(execution)
		if _, found := existing[name]; found {
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
	return nil
}

func (r *KubernetesReconciler) loadKubernetesExecutions(ctx context.Context, targetID uuid.UUID) ([]kubernetesExecution, error) {
	var items []kubernetesExecution
	err := r.targets.db.WithContext(ctx).Table("agent_executions AS e").
		Select(`e.id, e.tenant_id, s.organization_id, s.project_id, e.session_id, e.status, e.generation,
			e.worker_release_revision_id, e.worker_release_channel,
			manifest.image_digest AS worker_release_image_digest`).
		Joins("JOIN agent_sessions AS s ON s.tenant_id = e.tenant_id AND s.id = e.session_id").
		Joins("LEFT JOIN worker_release_revisions AS release ON release.execution_target_id = e.execution_target_id AND release.id = e.worker_release_revision_id").
		Joins("LEFT JOIN worker_manifests AS manifest ON manifest.id = release.worker_manifest_id").
		Where("e.execution_target_id = ? AND e.target_kind = ? AND e.status IN ?", targetID, "kubernetes", []string{"queued", "recovering", "leased", "running", "waiting-for-approval"}).
		Order("e.queued_at, e.id").Scan(&items).Error
	if err != nil {
		return nil, problem.Wrap(500, "kubernetes_executions_load_failed", "Kubernetes executions could not be loaded.", err)
	}
	return items, nil
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
	if configuration.Image == "" || len(configuration.Image) > 512 || len(configuration.RunnerCommand) == 0 ||
		strings.TrimSpace(r.config.RegistrationToken) == "" {
		return kubernetesTargetConfiguration{}, problem.New(503, "kubernetes_worker_configuration_unavailable", "Kubernetes image, runnerCommand, and Worker registration are required.")
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
		TokenHash     [32]byte
		LeaseRenew    time.Duration
	}{configuration, capabilities, sha256.Sum256([]byte(r.config.RegistrationToken)), workertiming.LeaseRenewInterval(r.config.WorkerLeaseTTL)})
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
			"metadata": map[string]any{"name": configuration.Namespace, "labels": labels},
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
	secretName := kubernetesSecretName(target.ID)
	secret := map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata":   map[string]any{"name": secretName, "namespace": configuration.Namespace, "labels": labels},
		"stringData": map[string]any{"registration-token": r.config.RegistrationToken},
	}
	if err := client.Apply(ctx, kubernetesNamespacedPath(configuration.Namespace, "secrets", secretName), secret); err != nil {
		return problem.Wrap(502, "kubernetes_secret_apply_failed", "Kubernetes Worker registration Secret could not be applied.", err)
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
	generation := execution.Generation + 1
	labels := kubernetesTargetLabels(target)
	labels["synara.io/project-id"] = execution.ProjectID.String()
	labels["synara.io/session-id"] = execution.SessionID.String()
	labels[kubernetesExecutionLabel] = execution.ID.String()
	labels[kubernetesGenerationLabel] = strconv.FormatInt(generation, 10)
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
		map[string]any{"name": "SYNARA_WORKER_REGISTRATION_TOKEN", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": kubernetesSecretName(target.ID), "key": "registration-token"}}},
		map[string]any{"name": "SYNARA_EXECUTION_TARGET_ID", "value": target.ID.String()},
		map[string]any{"name": "SYNARA_EXECUTION_TARGET_KIND", "value": "kubernetes"},
		map[string]any{"name": "SYNARA_AGENTD_ASSIGNED_EXECUTION_ID", "value": execution.ID.String()},
		map[string]any{"name": "SYNARA_AGENTD_CLUSTER_ID", "value": "kubernetes"},
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
	}
	if configuration.WorkspaceSizeLimit != "" {
		volumes[0] = map[string]any{"name": "workspace", "emptyDir": map[string]any{"sizeLimit": configuration.WorkspaceSizeLimit}}
	}
	volumeMounts := []any{
		map[string]any{"name": "workspace", "mountPath": "/data"},
		map[string]any{"name": "tmp", "mountPath": "/tmp"},
		map[string]any{"name": "home", "mountPath": "/home/synara"},
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
			"capabilities": map[string]any{"drop": []any{"ALL"}},
		},
		"resources": map[string]any{"requests": requests, "limits": limits},
	}
	podSpec := map[string]any{
		"serviceAccountName": configuration.ServiceAccountName, "automountServiceAccountToken": false,
		"restartPolicy": "Never", "terminationGracePeriodSeconds": 30,
		"securityContext": map[string]any{"runAsNonRoot": true, "fsGroup": 10001, "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
		"containers":      []any{container}, "volumes": volumes,
	}
	if len(configuration.NodeSelector) > 0 {
		podSpec["nodeSelector"] = configuration.NodeSelector
	}
	if len(configuration.Tolerations) > 0 {
		podSpec["tolerations"] = configuration.Tolerations
	}
	if configuration.RequireNodeSpread {
		podSpec["topologySpreadConstraints"] = kubernetesNodeSpreadConstraints(target.ID)
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
	}{baseHash, image, execution.WorkerReleaseRevisionID, execution.WorkerReleaseChannel})
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
		baseURL: configuration.APIServer, token: token,
		client: &http.Client{Transport: transport, Timeout: 30 * time.Second},
	}, nil
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
					Name        string            `json:"name"`
					UID         string            `json:"uid"`
					Labels      map[string]string `json:"labels"`
					Annotations map[string]string `json:"annotations"`
				} `json:"metadata"`
				Status struct {
					Phase string `json:"phase"`
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
			items = append(items, kubernetesPod{
				Name: item.Metadata.Name, UID: item.Metadata.UID, Phase: item.Status.Phase,
				Labels: item.Metadata.Labels, Annotations: item.Metadata.Annotations,
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
	return c.do(ctx, http.MethodDelete, path, map[string]any{
		"apiVersion": "v1", "kind": "DeleteOptions",
		"preconditions": map[string]any{"uid": uid},
	}, nil, http.StatusOK, http.StatusAccepted, http.StatusNotFound)
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
		return fmt.Errorf("Kubernetes API returned status %d: %s", response.StatusCode, detail)
	}
	if output == nil {
		_, _ = io.Copy(io.Discard, response.Body)
		return nil
	}
	return json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(output)
}
