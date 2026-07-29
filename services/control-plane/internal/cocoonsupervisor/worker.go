package cocoonsupervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func (d *Daemon) runWorker(ctx context.Context, pod Pod) (returned error) {
	workerRoot := filepath.Join(d.config.StateRoot, "workers", pod.UID.String())
	runtimeRoot := filepath.Join(d.runtimeWorkersRoot(), pod.UID.String())
	workspaceRoot := filepath.Join(workerRoot, "data", "workspaces")
	gitCacheRoot := filepath.Join(workerRoot, "data", "git-cache")
	privateTempRoot := filepath.Join(workerRoot, "private-tmp")
	homeRoot := filepath.Join(workerRoot, "home")
	for _, path := range []string{workerRoot, runtimeRoot, workspaceRoot, gitCacheRoot, privateTempRoot, homeRoot} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("prepare Cocoon Worker path: %w", err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("protect Cocoon Worker path: %w", err)
		}
	}
	tag := "synara-" + strings.ReplaceAll(pod.UID.String(), "-", "")[:12]
	socketPath := filepath.Join(runtimeRoot, "virtiofs.sock")
	virtiofs := exec.Command(
		d.config.VirtiofsdCommand,
		"--socket-path="+socketPath, "-o", "source="+workerRoot, "-o", "cache=none", "-f",
	)
	virtiofs.Stdout = io.Discard
	virtiofsStderr := &boundedLogBuffer{maximum: 16 << 10}
	virtiofs.Stderr = virtiofsStderr
	if err := virtiofs.Start(); err != nil {
		return fmt.Errorf("start virtiofsd: %w", err)
	}
	virtiofsDone := make(chan error, 1)
	go func() {
		virtiofsDone <- virtiofs.Wait()
		close(virtiofsDone)
	}()
	defer func() {
		if virtiofs.Process != nil {
			_ = virtiofs.Process.Signal(syscall.SIGTERM)
			select {
			case <-virtiofsDone:
			case <-time.After(2 * time.Second):
				_ = virtiofs.Process.Kill()
				<-virtiofsDone
			}
		}
	}()
	if err := waitForSocket(ctx, socketPath, virtiofsDone); err != nil {
		return fmt.Errorf("wait for virtiofsd: %w (%s)", err, virtiofsStderr.String())
	}
	attached := false
	mounted := false
	defer func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if mounted {
			_, _ = d.commandOutput(cleanupContext, d.config.CocoonCommand, "vm", "exec", pod.VMID, "--", "/bin/umount", workerRoot)
		}
		if attached {
			_, _ = d.commandOutput(cleanupContext, d.config.CocoonCommand, "vm", "fs", "detach", pod.VMID, "--tag", tag)
		}
	}()
	if _, err := d.commandOutput(ctx, d.config.CocoonCommand, "vm", "fs", "attach", pod.VMID, "--socket", socketPath, "--tag", tag); err != nil {
		return fmt.Errorf("attach Cocoon Workspace filesystem: %w", err)
	}
	attached = true
	if _, err := d.commandOutput(ctx, d.config.CocoonCommand, "vm", "exec", pod.VMID, "--", "/bin/mkdir", "-p", workerRoot); err != nil {
		return fmt.Errorf("prepare guest Workspace mountpoint: %w", err)
	}
	if _, err := d.commandOutput(ctx, d.config.CocoonCommand, "vm", "exec", pod.VMID, "--", "/bin/mount", "-t", "virtiofs", tag, workerRoot); err != nil {
		return fmt.Errorf("mount Cocoon Workspace filesystem: %w", err)
	}
	mounted = true
	if _, err := d.commandOutput(ctx, d.config.CocoonCommand, "vm", "exec", pod.VMID, "--", "/usr/bin/mountpoint", "-q", workerRoot); err != nil {
		return fmt.Errorf("attest guest Workspace mount: %w", err)
	}

	token, err := d.requestBoundRegistrationToken(ctx, pod)
	if err != nil {
		return err
	}
	tokenPath := filepath.Join(runtimeRoot, "registration-token")
	assignmentPath := filepath.Join(runtimeRoot, "assigned-execution-id")
	attestationPath := filepath.Join(runtimeRoot, "supervisor-attestation.json")
	for _, path := range []string{tokenPath, assignmentPath, attestationPath} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("clear stale Cocoon Worker bootstrap file: %w", err)
		}
	}
	if err := writePrivateFile(tokenPath, []byte(token+"\n")); err != nil {
		return fmt.Errorf("stage Pod-bound registration token: %w", err)
	}
	if err := writePrivateFile(assignmentPath, []byte(pod.ExecutionID.String()+"\n")); err != nil {
		return fmt.Errorf("stage assigned Execution identity: %w", err)
	}
	attestation := WorkerAttestation{
		Version: WorkerAttestationVersion, PodUID: pod.UID.String(), VMID: pod.VMID,
		SupervisorInstance: d.instanceID.String(), ObservedAt: d.now().UTC(),
		HostSupervisor: HostSupervisorVersion, ProviderTransport: ProviderTransport,
		IsolationProfile: IsolationProfile,
	}
	encodedAttestation, err := json.Marshal(attestation)
	if err != nil {
		return fmt.Errorf("encode Cocoon Worker attestation: %w", err)
	}
	if err := writePrivateFile(attestationPath, append(encodedAttestation, '\n')); err != nil {
		return fmt.Errorf("stage Cocoon Worker attestation: %w", err)
	}
	runner := []string{d.config.TransportCommand, "host", "--vm-id", pod.VMID, "--"}
	runner = append(runner, d.config.GuestProviderCommand...)
	runnerCommand, err := json.Marshal(runner)
	if err != nil {
		return fmt.Errorf("encode Cocoon Provider transport command: %w", err)
	}
	imageDigest := pod.GuestImage[strings.LastIndex(pod.GuestImage, "@")+1:]
	environment := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=" + homeRoot, "TMPDIR=" + privateTempRoot,
		"SYNARA_CONTROL_PLANE_URL=" + strings.TrimRight(d.config.ControlPlaneURL.String(), "/"),
		"SYNARA_EXECUTION_TARGET_ID=" + d.config.ExecutionTargetID.String(),
		"SYNARA_EXECUTION_TARGET_KIND=kubernetes",
		"SYNARA_WORKER_REGISTRATION_TOKEN_FILE=" + tokenPath,
		"SYNARA_AGENTD_ASSIGNED_EXECUTION_ID_FILE=" + assignmentPath,
		"SYNARA_AGENTD_WORKER_MODE=execution-pinned",
		"SYNARA_AGENTD_INSTANCE_ID=" + pod.Name,
		"SYNARA_AGENTD_INSTANCE_UID=" + pod.UID.String(),
		"SYNARA_AGENTD_NAMESPACE=" + pod.Namespace,
		"SYNARA_AGENTD_CLUSTER_ID=" + d.config.ClusterID,
		"SYNARA_AGENTD_RUNNER_COMMAND_JSON=" + string(runnerCommand),
		"SYNARA_AGENTD_PROVIDER_HOST_PROTOCOL=v2",
		"SYNARA_AGENTD_CAPABILITIES_JSON=" + d.config.CapabilitiesJSON,
		"SYNARA_AGENTD_WORKSPACE_ROOT=" + workspaceRoot,
		"SYNARA_AGENTD_GIT_CACHE_ROOT=" + gitCacheRoot,
		"SYNARA_AGENTD_PRIVATE_TMP_ROOT=" + privateTempRoot,
		"SYNARA_AGENTD_IMAGE_DIGEST=" + imageDigest,
		"SYNARA_AGENTD_COCOON_SUPERVISOR_ATTESTATION_FILE=" + attestationPath,
		"SYNARA_AGENTD_KUBERNETES_PIDS_MAX=" + strconv.FormatUint(d.config.PIDsMax, 10),
		"SYNARA_AGENTD_REQUEST_TIMEOUT=" + d.config.AgentRequestTimeout.String(),
	}
	if d.config.WorkerVersion != "" {
		environment = append(environment, "SYNARA_AGENTD_VERSION="+d.config.WorkerVersion)
	}
	if d.config.WorkerBuildGitSHA != "" {
		environment = append(environment, "SYNARA_AGENTD_BUILD_GIT_SHA="+d.config.WorkerBuildGitSHA)
	}
	for _, name := range []string{
		"SYNARA_PROVIDER_HTTP_PROXY", "SYNARA_PROVIDER_HTTPS_PROXY", "SYNARA_PROVIDER_ALL_PROXY", "SYNARA_PROVIDER_NO_PROXY",
	} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" && !strings.ContainsAny(value, "\r\n\x00") {
			environment = append(environment, name+"="+value)
		}
	}
	agentd := exec.Command(d.config.AgentdCommand)
	agentd.Env = environment
	agentd.Dir = workerRoot
	agentd.Stdout = os.Stdout
	agentd.Stderr = os.Stderr
	if err := agentd.Start(); err != nil {
		return fmt.Errorf("start host agentd: %w", err)
	}
	agentdDone := make(chan error, 1)
	go func() { agentdDone <- agentd.Wait() }()
	select {
	case err := <-agentdDone:
		if err != nil {
			return fmt.Errorf("host agentd exited: %w", err)
		}
		return nil
	case <-ctx.Done():
		_ = agentd.Process.Signal(syscall.SIGTERM)
		select {
		case <-agentdDone:
		case <-time.After(20 * time.Second):
			_ = agentd.Process.Kill()
			<-agentdDone
		}
		return ctx.Err()
	}
}

func writePrivateFile(path string, contents []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(contents); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	committed = true
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func waitForSocket(ctx context.Context, path string, processDone <-chan error) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		info, err := os.Lstat(path)
		if err == nil && info.Mode()&os.ModeSocket != 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-processDone:
			if err == nil {
				return errors.New("virtiofsd exited before publishing its socket")
			}
			return err
		case <-timeout.C:
			return errors.New("virtiofsd socket deadline exceeded")
		case <-ticker.C:
		}
	}
}

type boundedLogBuffer struct {
	buffer  bytes.Buffer
	maximum int
}

func (b *boundedLogBuffer) Write(payload []byte) (int, error) {
	original := len(payload)
	remaining := b.maximum - b.buffer.Len()
	if remaining > 0 {
		if len(payload) > remaining {
			payload = payload[:remaining]
		}
		_, _ = b.buffer.Write(payload)
	}
	return original, nil
}

func (b *boundedLogBuffer) String() string {
	return strings.TrimSpace(b.buffer.String())
}
