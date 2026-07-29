package agentd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/synara-ai/synara/services/control-plane/internal/platform"
)

const maximumKubernetesRegistrationTokenBytes = 64 << 10

// RunKubernetesRegistrationTokenStager is the narrow init-container entrypoint
// that moves Pod-bound identity into a one-shot volume. The Provider container
// never mounts the original projected ServiceAccount token.
func RunKubernetesRegistrationTokenStager(arguments []string) (bool, error) {
	if len(arguments) < 2 || arguments[1] != platform.KubernetesRegistrationTokenStageArgument {
		return false, nil
	}
	if len(arguments) != 2 {
		return true, errors.New("Kubernetes registration token stager accepts no additional arguments")
	}
	return true, stageKubernetesRegistrationToken(
		platform.KubernetesWorkloadIdentityTokenPath,
		platform.KubernetesStagedRegistrationTokenPath,
	)
}

func stageKubernetesRegistrationToken(sourcePath, destinationPath string) error {
	token, err := readProjectedKubernetesRegistrationTokenFile(sourcePath)
	if err != nil {
		return fmt.Errorf("read projected Kubernetes registration token: %w", err)
	}
	directory := filepath.Dir(destinationPath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("prepare staged Kubernetes registration token directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return fmt.Errorf("restrict staged Kubernetes registration token directory: %w", err)
	}
	temporary, err := os.OpenFile(destinationPath+".staging", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create staged Kubernetes registration token: %w", err)
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if _, err := io.WriteString(temporary, token+"\n"); err != nil {
		return fmt.Errorf("write staged Kubernetes registration token: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync staged Kubernetes registration token: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close staged Kubernetes registration token: %w", err)
	}
	if err := os.Rename(temporaryPath, destinationPath); err != nil {
		return fmt.Errorf("commit staged Kubernetes registration token: %w", err)
	}
	committed = true
	if err := syncRegistrationTokenDirectory(directory); err != nil {
		return fmt.Errorf("sync staged Kubernetes registration token directory: %w", err)
	}
	return nil
}

// Kubernetes projected volumes use the kubelet AtomicWriter layout: the
// visible token path is a symlink into a versioned, read-only directory. This
// reader follows that kubelet-managed link, then validates the opened target.
// The staged main-container token continues to use the stricter no-symlink
// reader below.
func readProjectedKubernetesRegistrationTokenFile(path string) (string, error) {
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("projected registration token target is not a regular file")
	}
	return readRegistrationTokenContents(file)
}

func readRegularRegistrationTokenFile(path string) (string, error) {
	info, err := os.Lstat(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("registration token path is not a regular file")
	}
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	defer file.Close()
	return readRegistrationTokenContents(file)
}

func readRegistrationTokenContents(file *os.File) (string, error) {
	contents, err := io.ReadAll(io.LimitReader(file, maximumKubernetesRegistrationTokenBytes+1))
	if err != nil {
		return "", err
	}
	if len(contents) > maximumKubernetesRegistrationTokenBytes {
		return "", errors.New("registration token file is too large")
	}
	token := strings.TrimSpace(string(contents))
	if token == "" || strings.ContainsAny(token, "\r\n\x00") {
		return "", errors.New("registration token is empty or malformed")
	}
	return token, nil
}

func removeConsumedRegistrationTokenFile(path string) error {
	info, err := os.Lstat(filepath.Clean(path))
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("consumed registration token path is not a regular file")
	}
	if err := os.Remove(filepath.Clean(path)); err != nil {
		return err
	}
	return syncRegistrationTokenDirectory(filepath.Dir(filepath.Clean(path)))
}

func syncRegistrationTokenDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
