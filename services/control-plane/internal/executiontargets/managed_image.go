package executiontargets

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/gitpolicy"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

type ImagePullCredential struct {
	BindingID         uuid.UUID
	CredentialID      uuid.UUID
	CredentialVersion int
	Host              string
	Username          string
	Password          string
	RegistryToken     string
}

// ImagePullCredentialResolution distinguishes an authoritative Credential
// state from a transient lookup or KMS failure. Provisioners may remove a
// previously materialized registry secret only when Authoritative is true.
type ImagePullCredentialResolution struct {
	Credential    *ImagePullCredential
	Authoritative bool
}

type ImagePullCredentialResolver func(
	context.Context,
	uuid.UUID,
	uuid.UUID,
	string,
) (ImagePullCredentialResolution, error)

type managedReleaseImage struct {
	RevisionID uuid.UUID
	Channel    string
	Image      string
}

type managedReleasePlan struct {
	PolicyVersion int64
	Promoted      managedReleaseImage
	Canary        *managedReleaseImage
	CanaryPercent int
}

func loadManagedReleasePlan(
	ctx context.Context,
	db *gorm.DB,
	targetID uuid.UUID,
	baseImage string,
) (*managedReleasePlan, error) {
	var policy persistence.WorkerReleasePolicy
	err := db.WithContext(ctx).Where("execution_target_id = ?", targetID).Take(&policy).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, problem.Wrap(500, "worker_release_policy_lookup_failed", "Failed to load the Worker release policy.", err)
	}
	promoted, err := loadManagedReleaseImage(ctx, db, targetID, policy.PromotedRevisionID, "promoted", baseImage)
	if err != nil {
		return nil, err
	}
	plan := &managedReleasePlan{PolicyVersion: policy.PolicyVersion, Promoted: promoted}
	if policy.CanaryRevisionID != nil && policy.CanaryPercent > 0 {
		canary, err := loadManagedReleaseImage(ctx, db, targetID, *policy.CanaryRevisionID, "canary", baseImage)
		if err != nil {
			return nil, err
		}
		plan.Canary, plan.CanaryPercent = &canary, policy.CanaryPercent
	}
	return plan, nil
}

func loadManagedReleaseImage(
	ctx context.Context,
	db *gorm.DB,
	targetID, revisionID uuid.UUID,
	channel, baseImage string,
) (managedReleaseImage, error) {
	var row struct {
		RevisionID  uuid.UUID `gorm:"column:revision_id"`
		ImageDigest *string   `gorm:"column:image_digest"`
	}
	err := db.WithContext(ctx).Table("worker_release_revisions AS revision").
		Select("revision.id AS revision_id, manifest.image_digest").
		Joins("JOIN worker_manifests AS manifest ON manifest.id = revision.worker_manifest_id").
		Where("revision.execution_target_id = ? AND revision.id = ?", targetID, revisionID).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return managedReleaseImage{}, problem.New(409, "worker_release_revision_unavailable", "Worker release revision is unavailable for managed reconciliation.")
	}
	if err != nil {
		return managedReleaseImage{}, problem.Wrap(500, "worker_release_revision_lookup_failed", "Failed to load the Worker release revision image.", err)
	}
	if row.ImageDigest == nil {
		return managedReleaseImage{}, problem.New(409, "worker_release_image_digest_required", "Managed Worker release revisions require an immutable image digest.")
	}
	image, err := pinImageReference(baseImage, *row.ImageDigest)
	if err != nil {
		return managedReleaseImage{}, err
	}
	return managedReleaseImage{RevisionID: row.RevisionID, Channel: channel, Image: image}, nil
}

func pinImageReference(reference, digest string) (string, error) {
	reference = strings.TrimSpace(reference)
	digest = strings.TrimSpace(digest)
	if reference == "" || immutableImageDigest("image@"+digest) == "" {
		return "", problem.New(409, "worker_release_image_reference_invalid", "Worker release image reference or digest is invalid.")
	}
	if separator := strings.LastIndex(reference, "@"); separator >= 0 {
		reference = reference[:separator]
	}
	lastSlash := strings.LastIndex(reference, "/")
	if tag := strings.LastIndex(reference, ":"); tag > lastSlash {
		reference = reference[:tag]
	}
	if reference == "" || strings.ContainsAny(reference, "\r\n\t\x00") {
		return "", problem.New(409, "worker_release_image_reference_invalid", "Worker release image reference or digest is invalid.")
	}
	return reference + "@" + digest, nil
}

func validateImagePullCredential(image string, credential *ImagePullCredential) error {
	if credential == nil {
		return nil
	}
	authority, err := registryAuthorityFromImageReference(image)
	if err != nil {
		return err
	}
	credentialAuthority, err := normalizeRegistryAuthority(credential.Host)
	if err != nil || registryComparisonAuthority(authority) != registryComparisonAuthority(credentialAuthority) {
		return problem.New(409, "worker_image_pull_binding_selector_mismatch", "Worker image pull Credential does not match the configured image registry.")
	}
	if (credential.Username == "") == (credential.RegistryToken == "") ||
		(credential.Username != "" && credential.Password == "") {
		return problem.New(500, "worker_image_pull_credential_invalid", "Worker image pull Credential projection is invalid.")
	}
	return nil
}

func registryAuthorityFromImageReference(reference string) (string, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" || strings.ContainsAny(reference, "\r\n\t\x00") || strings.Contains(reference, "://") {
		return "", problem.New(400, "invalid_worker_image_reference", "Worker image reference is invalid.")
	}
	slash := strings.IndexByte(reference, '/')
	if slash < 0 {
		// A registry authority is only present when it precedes a repository
		// path. In an unqualified image such as `worker:local` or
		// `worker@sha256:...`, the colon belongs to the tag or digest rather
		// than a registry port.
		return "docker.io", nil
	}
	if slash == 0 {
		return "", problem.New(400, "invalid_worker_image_reference", "Worker image reference is invalid.")
	}
	first := reference[:slash]
	if !strings.ContainsAny(first, ".:") && first != "localhost" {
		return "docker.io", nil
	}
	authority, err := normalizeRegistryAuthority(first)
	if err != nil {
		return "", problem.New(400, "invalid_worker_image_reference", "Worker image reference is invalid.")
	}
	return authority, nil
}

func normalizeRegistryAuthority(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 320 || strings.ContainsAny(value, "/\\@?#\r\n\t\x00") {
		return "", errors.New("invalid registry authority")
	}

	host, port := value, ""
	if strings.HasPrefix(value, "[") {
		closing := strings.IndexByte(value, ']')
		if closing <= 1 {
			return "", errors.New("invalid registry authority")
		}
		host = value[1:closing]
		remainder := value[closing+1:]
		if remainder != "" {
			if !strings.HasPrefix(remainder, ":") || len(remainder) == 1 {
				return "", errors.New("invalid registry authority")
			}
			port = remainder[1:]
		}
	} else {
		if strings.Count(value, ":") > 1 {
			return "", errors.New("unbracketed IPv6 registry authority")
		}
		if separator := strings.LastIndexByte(value, ':'); separator >= 0 {
			host, port = value[:separator], value[separator+1:]
			if host == "" || port == "" {
				return "", errors.New("invalid registry authority")
			}
		}
	}

	normalizedHost, err := gitpolicy.NormalizeHostname(host)
	if err != nil {
		return "", errors.New("invalid registry authority")
	}
	if port != "" {
		parsedPort, err := strconv.Atoi(port)
		if err != nil || parsedPort < 1 || parsedPort > 65535 || strconv.Itoa(parsedPort) != port {
			return "", errors.New("invalid registry authority")
		}
		return net.JoinHostPort(normalizedHost, port), nil
	}
	if strings.Contains(normalizedHost, ":") {
		return "[" + normalizedHost + "]", nil
	}
	return normalizedHost, nil
}

func registryComparisonAuthority(authority string) string {
	if authority == "docker.io" || authority == "index.docker.io" {
		return "docker.io"
	}
	return authority
}

func registryAuthServerAddress(authority string) string {
	if registryComparisonAuthority(authority) == "docker.io" {
		return "https://index.docker.io/v1/"
	}
	return authority
}
