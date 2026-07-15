package executions

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/credentialscope"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const providerCursorBindingVersion = 1

type providerCursorClaimSnapshot struct {
	CredentialID      *uuid.UUID
	CredentialVersion *int
	ResumeStrategy    string
	BindingVersion    *int
	BindingDigest     []byte
}

type providerCursorBindingMaterial struct {
	TenantID                          uuid.UUID
	SessionID                         uuid.UUID
	Provider                          string
	Model                             *string
	CredentialID                      *uuid.UUID
	CredentialVersion                 *int
	CapabilityDescriptorHash          string
	ProviderHostProtocolMajor         int
	ProviderHostProtocolMinor         int
	HostBuildVersion                  string
	AdapterVersion                    string
	ProviderCLIVersion                *string
	RuntimeKind                       string
	RuntimeName                       string
	RuntimeVersion                    *string
	RuntimeVersionSource              string
	RuntimeMinimumInclusive           string
	RuntimeMaximumExclusive           *string
	RuntimeAvailable                  bool
	RuntimeCompatible                 bool
	ReleaseRequiresExplicitEnablement bool
	ReleaseEnabled                    bool
	ResumeStrategy                    string
}

func loadProviderCursorClaimSnapshot(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	worker persistence.WorkerInstance,
	now time.Time,
) (providerCursorClaimSnapshot, error) {
	snapshot := providerCursorClaimSnapshot{ResumeStrategy: "authoritative-history"}
	if execution.Provider == nil || strings.TrimSpace(*execution.Provider) == "" || worker.CurrentManifestID == nil {
		return snapshot, nil
	}
	provider := strings.TrimSpace(*execution.Provider)
	var session struct {
		OrganizationID       uuid.UUID  `gorm:"column:organization_id"`
		CreatedBy            uuid.UUID  `gorm:"column:created_by"`
		Provider             string     `gorm:"column:provider"`
		Model                *string    `gorm:"column:model"`
		ProviderCredentialID *uuid.UUID `gorm:"column:provider_credential_id"`
	}
	if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Table("agent_sessions").
		Select("organization_id", "created_by", "provider", "model", "provider_credential_id").
		Where("tenant_id = ? AND id = ?", execution.TenantID, execution.SessionID).
		Take(&session).Error; err != nil {
		return providerCursorClaimSnapshot{}, problem.Wrap(500, "provider_cursor_session_snapshot_failed", "Failed to capture the Session Provider Cursor snapshot.", err)
	}
	if strings.TrimSpace(session.Provider) != provider {
		return providerCursorClaimSnapshot{}, problem.New(409, "provider_cursor_provider_mismatch", "The Execution Provider does not match the Agent Session provider.")
	}
	if session.ProviderCredentialID != nil {
		selection, err := credentialscope.Resolve(ctx, tx, credentialscope.Request{
			TenantID: execution.TenantID, OrganizationID: session.OrganizationID,
			SessionOwnerUserID: session.CreatedBy, Provider: provider, Model: session.Model,
			ExplicitCredentialID: session.ProviderCredentialID, Now: now,
		})
		if err != nil {
			return providerCursorClaimSnapshot{}, err
		}
		if selection == nil {
			return providerCursorClaimSnapshot{}, problem.New(409, "credential_unavailable", "The bound Provider Credential is unavailable for this Execution.")
		}
		credential := selection.Credential
		credentialID := credential.ID
		credentialVersion := credential.Version
		snapshot.CredentialID = &credentialID
		snapshot.CredentialVersion = &credentialVersion
	}

	var manifest persistence.WorkerProviderManifest
	if err := tx.WithContext(ctx).
		Where("worker_manifest_id = ? AND provider = ?", *worker.CurrentManifestID, provider).
		Take(&manifest).Error; err != nil {
		return providerCursorClaimSnapshot{}, problem.Wrap(409, "worker_provider_incompatible", "The Worker manifest does not support the Execution Provider.", err)
	}
	if !providerCursorNativeResumeEligible(manifest) {
		return snapshot, nil
	}
	material := providerCursorBindingMaterial{
		TenantID: execution.TenantID, SessionID: execution.SessionID, Provider: provider, Model: session.Model,
		CredentialID: snapshot.CredentialID, CredentialVersion: snapshot.CredentialVersion,
		CapabilityDescriptorHash:  manifest.CapabilityDescriptorHash,
		ProviderHostProtocolMajor: manifest.ProviderHostMajor, ProviderHostProtocolMinor: manifest.ProviderHostMinor,
		HostBuildVersion: manifest.HostBuildVersion, AdapterVersion: manifest.AdapterVersion,
		ProviderCLIVersion: manifest.ProviderCLIVersion,
		RuntimeKind:        manifest.RuntimeKind, RuntimeName: manifest.RuntimeName, RuntimeVersion: manifest.RuntimeVersion,
		RuntimeVersionSource:    manifest.RuntimeVersionSource,
		RuntimeMinimumInclusive: manifest.RuntimeMinimumInclusive,
		RuntimeMaximumExclusive: manifest.RuntimeMaximumExclusive,
		RuntimeAvailable:        manifest.RuntimeAvailable, RuntimeCompatible: manifest.RuntimeCompatible,
		ReleaseRequiresExplicitEnablement: manifest.ReleaseRequiresExplicitEnablement,
		ReleaseEnabled:                    manifest.ReleaseEnabled, ResumeStrategy: "native-cursor",
	}
	digest := material.digest()
	bindingVersion := providerCursorBindingVersion
	snapshot.ResumeStrategy = "native-cursor"
	snapshot.BindingVersion = &bindingVersion
	snapshot.BindingDigest = append([]byte(nil), digest[:]...)
	return snapshot, nil
}

func providerCursorNativeResumeEligible(manifest persistence.WorkerProviderManifest) bool {
	resumeCapability, _ := manifest.Capabilities["resume-session"].(string)
	hasNativeStrategy := false
	for _, strategy := range manifest.ResumeStrategies {
		if strategy == "native-cursor" {
			hasNativeStrategy = true
			break
		}
	}
	return manifest.CompatibilityStatus == "compatible" && resumeCapability == "native" && hasNativeStrategy &&
		(manifest.ProviderHostMajor > 2 || (manifest.ProviderHostMajor == 2 && manifest.ProviderHostMinor >= 1)) &&
		strings.TrimSpace(manifest.CapabilityDescriptorHash) != "" && strings.TrimSpace(manifest.HostBuildVersion) != "" &&
		strings.TrimSpace(manifest.AdapterVersion) != "" && strings.TrimSpace(manifest.RuntimeKind) != "" &&
		strings.TrimSpace(manifest.RuntimeName) != "" && manifest.RuntimeVersion != nil && strings.TrimSpace(*manifest.RuntimeVersion) != "" &&
		strings.TrimSpace(manifest.RuntimeVersionSource) != "" && strings.TrimSpace(manifest.RuntimeMinimumInclusive) != "" &&
		manifest.RuntimeAvailable && manifest.RuntimeCompatible && manifest.ReleaseEnabled
}

func (material providerCursorBindingMaterial) digest() [sha256.Size]byte {
	encoder := cursorBindingEncoder{hash: sha256.New()}
	encoder.string("synara-provider-cursor-binding-v1")
	encoder.string(material.TenantID.String())
	encoder.string(material.SessionID.String())
	encoder.string(material.Provider)
	encoder.optionalString(material.Model)
	encoder.optionalUUID(material.CredentialID)
	encoder.optionalInt(material.CredentialVersion)
	encoder.string(material.CapabilityDescriptorHash)
	encoder.integer(material.ProviderHostProtocolMajor)
	encoder.integer(material.ProviderHostProtocolMinor)
	encoder.string(material.HostBuildVersion)
	encoder.string(material.AdapterVersion)
	encoder.optionalString(material.ProviderCLIVersion)
	encoder.string(material.RuntimeKind)
	encoder.string(material.RuntimeName)
	encoder.optionalString(material.RuntimeVersion)
	encoder.string(material.RuntimeVersionSource)
	encoder.string(material.RuntimeMinimumInclusive)
	encoder.optionalString(material.RuntimeMaximumExclusive)
	encoder.boolean(material.RuntimeAvailable)
	encoder.boolean(material.RuntimeCompatible)
	encoder.boolean(material.ReleaseRequiresExplicitEnablement)
	encoder.boolean(material.ReleaseEnabled)
	encoder.string(material.ResumeStrategy)
	var result [sha256.Size]byte
	copy(result[:], encoder.hash.Sum(nil))
	return result
}

type cursorBindingEncoder struct {
	hash hash.Hash
}

func (encoder cursorBindingEncoder) bytes(value []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	_, _ = encoder.hash.Write(length[:])
	_, _ = encoder.hash.Write(value)
}

func (encoder cursorBindingEncoder) string(value string) {
	encoder.bytes([]byte(value))
}

func (encoder cursorBindingEncoder) boolean(value bool) {
	if value {
		encoder.bytes([]byte{1})
		return
	}
	encoder.bytes([]byte{0})
}

func (encoder cursorBindingEncoder) integer(value int) {
	encoder.string(strconv.Itoa(value))
}

func (encoder cursorBindingEncoder) optionalString(value *string) {
	if value == nil {
		encoder.bytes(nil)
		return
	}
	encoder.bytes(append([]byte{1}, []byte(*value)...))
}

func (encoder cursorBindingEncoder) optionalUUID(value *uuid.UUID) {
	if value == nil {
		encoder.bytes(nil)
		return
	}
	encoder.bytes(append([]byte{1}, value[:]...))
}

func (encoder cursorBindingEncoder) optionalInt(value *int) {
	if value == nil {
		encoder.bytes(nil)
		return
	}
	encoder.bytes(append([]byte{1}, []byte(strconv.Itoa(*value))...))
}
