package kmsworker

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	ProviderName       = "synara-kms"
	AlgorithmAES256GCM = "AES-256-GCM"

	StateActive          = "active"
	StateDecryptOnly     = "decrypt-only"
	StateDisabled        = "disabled"
	StatePendingDeletion = "pending-deletion"
	StateDestroyed       = "destroyed"
)

type Role string

const (
	RoleControlPlaneCryptor Role = "control-plane-cryptor"
	RoleKeyManager          Role = "kms-key-manager"
	RoleKeyDisabler         Role = "kms-key-disabler"
	RoleDeletionApprover    Role = "kms-deletion-approver"
	RoleAuditor             Role = "kms-auditor"
)

type Actor struct {
	Identity string
	Role     Role
}

type LogicalKey struct {
	ID               string       `gorm:"primaryKey;size:36"`
	Name             string       `gorm:"size:200;not null"`
	Description      string       `gorm:"size:2000;not null"`
	PolicyReference  string       `gorm:"size:500;not null"`
	LabelsJSON       string       `gorm:"type:text;not null"`
	MetadataRevision int64        `gorm:"not null;check:kms_metadata_revision_positive,metadata_revision > 0"`
	ActiveVersion    int64        `gorm:"not null;check:kms_active_version_nonnegative,active_version >= 0"`
	CreatedAt        time.Time    `gorm:"not null"`
	UpdatedAt        time.Time    `gorm:"not null"`
	Versions         []KeyVersion `gorm:"foreignKey:KeyID;references:ID;constraint:OnUpdate:RESTRICT,OnDelete:RESTRICT" json:"-"`
}

func (LogicalKey) TableName() string { return "kms_logical_keys" }

type KeyVersion struct {
	KeyID             string `gorm:"primaryKey;size:36"`
	Version           int64  `gorm:"primaryKey;check:kms_key_version_positive,version > 0"`
	Algorithm         string `gorm:"size:40;not null;check:kms_key_algorithm,algorithm = 'AES-256-GCM'"`
	State             string `gorm:"size:32;not null;index;check:kms_key_version_state,state IN ('active','decrypt-only','disabled','pending-deletion','destroyed')"`
	SealKeyID         string `gorm:"size:64;not null"`
	SealedMaterial    []byte `gorm:"not null"`
	EncryptNotAfter   *time.Time
	DecryptNotAfter   *time.Time
	CreatedAt         time.Time `gorm:"not null"`
	DisabledAt        *time.Time
	PendingDeletionAt *time.Time
	DestroyAfter      *time.Time
	DestroyedAt       *time.Time
}

func (KeyVersion) TableName() string { return "kms_key_versions" }

type AuditEvent struct {
	ID                string    `gorm:"primaryKey;size:36" json:"id"`
	OccurredAt        time.Time `gorm:"not null;index" json:"occurredAt"`
	ActorIdentity     string    `gorm:"size:500;not null" json:"actorIdentity"`
	ActorRole         string    `gorm:"size:64;not null" json:"actorRole"`
	Operation         string    `gorm:"size:100;not null;index" json:"operation"`
	KeyID             string    `gorm:"size:36;not null;index" json:"keyId"`
	KeyVersion        *int64    `json:"keyVersion,omitempty"`
	Outcome           string    `gorm:"size:32;not null" json:"outcome"`
	OperatorReference string    `gorm:"size:500;not null" json:"operatorReference"`
	DetailsDigest     string    `gorm:"size:64;not null" json:"detailsDigest"`
}

func (AuditEvent) TableName() string { return "kms_audit_events" }

type IdempotencyRecord struct {
	ActorIdentity  string    `gorm:"primaryKey;size:500"`
	Operation      string    `gorm:"primaryKey;size:100"`
	IdempotencyKey string    `gorm:"primaryKey;size:200"`
	RequestHash    string    `gorm:"size:64;not null"`
	StatusCode     int       `gorm:"not null"`
	ResponseJSON   []byte    `gorm:"not null"`
	CreatedAt      time.Time `gorm:"not null"`
}

func (IdempotencyRecord) TableName() string { return "kms_idempotency_records" }

type DeletionRequest struct {
	KeyID                string    `gorm:"primaryKey;size:36"`
	Version              int64     `gorm:"primaryKey"`
	RequestedBy          string    `gorm:"size:500;not null"`
	OperatorReference    string    `gorm:"size:500;not null"`
	RequestedAt          time.Time `gorm:"not null"`
	DestroyAfter         time.Time `gorm:"not null;index"`
	FirstEvidenceID      string    `gorm:"size:200;not null"`
	FirstEvidenceDigest  string    `gorm:"size:64;not null"`
	FirstEvidenceAt      time.Time `gorm:"not null"`
	FirstEvidenceJSON    string    `gorm:"type:text;not null"`
	CancelledAt          *time.Time
	CancelledBy          string `gorm:"size:500;not null"`
	DestroyedAt          *time.Time
	DestroyedBy          string `gorm:"size:500;not null"`
	SecondEvidenceID     string `gorm:"size:200;not null"`
	SecondEvidenceDigest string `gorm:"size:64;not null"`
	SecondEvidenceJSON   string `gorm:"type:text;not null"`
}

func (DeletionRequest) TableName() string { return "kms_deletion_requests" }

type ResealReceipt struct {
	ID                string    `gorm:"primaryKey;size:36" json:"id"`
	RunID             string    `gorm:"size:36;not null;uniqueIndex" json:"runId"`
	StartedAt         time.Time `gorm:"not null" json:"startedAt"`
	CompletedAt       time.Time `gorm:"not null" json:"completedAt"`
	ActorIdentity     string    `gorm:"size:500;not null" json:"actorIdentity"`
	OperatorReference string    `gorm:"size:500;not null" json:"operatorReference"`
	OldSealKeyIDsJSON string    `gorm:"type:text;not null" json:"oldSealKeyIdsJson"`
	NewSealKeyID      string    `gorm:"size:64;not null" json:"newSealKeyId"`
	VersionCount      int64     `gorm:"not null" json:"versionCount"`
	ReceiptDigest     string    `gorm:"size:64;not null" json:"receiptDigest"`
}

func (ResealReceipt) TableName() string { return "kms_reseal_receipts" }

type ResealRun struct {
	ID                string    `gorm:"primaryKey;size:36" json:"id"`
	StartedAt         time.Time `gorm:"not null" json:"startedAt"`
	CompletedAt       time.Time `gorm:"not null" json:"completedAt"`
	ActorIdentity     string    `gorm:"size:500;not null" json:"actorIdentity"`
	OperatorReference string    `gorm:"size:500;not null" json:"operatorReference"`
	OldSealKeyIDsJSON string    `gorm:"type:text;not null" json:"oldSealKeyIdsJson"`
	NewSealKeyID      string    `gorm:"size:64;not null" json:"newSealKeyId"`
	VersionCount      int64     `gorm:"not null" json:"versionCount"`
}

func (ResealRun) TableName() string { return "kms_reseal_runs" }

type DestructionReceipt struct {
	ID                   string    `gorm:"primaryKey;size:36" json:"id"`
	KeyID                string    `gorm:"size:36;not null;uniqueIndex:kms_destroyed_version" json:"keyId"`
	Version              int64     `gorm:"not null;uniqueIndex:kms_destroyed_version" json:"version"`
	DestroyedAt          time.Time `gorm:"not null" json:"destroyedAt"`
	DestroyedBy          string    `gorm:"size:500;not null" json:"destroyedBy"`
	OperatorReference    string    `gorm:"size:500;not null" json:"operatorReference"`
	FirstEvidenceID      string    `gorm:"size:200;not null" json:"firstEvidenceId"`
	FirstEvidenceDigest  string    `gorm:"size:64;not null" json:"firstEvidenceDigest"`
	SecondEvidenceID     string    `gorm:"size:200;not null" json:"secondEvidenceId"`
	SecondEvidenceDigest string    `gorm:"size:64;not null" json:"secondEvidenceDigest"`
	ReceiptDigest        string    `gorm:"size:64;not null" json:"receiptDigest"`
}

func (DestructionReceipt) TableName() string { return "kms_destruction_receipts" }

type KeyDescription struct {
	KeyID            string               `json:"keyId"`
	Name             string               `json:"name"`
	Description      string               `json:"description"`
	PolicyReference  string               `json:"policyReference"`
	Labels           map[string]string    `json:"labels"`
	MetadataRevision int64                `json:"metadataRevision"`
	ActiveVersion    int64                `json:"activeVersion"`
	CreatedAt        time.Time            `json:"createdAt"`
	UpdatedAt        time.Time            `json:"updatedAt"`
	Versions         []VersionDescription `json:"versions"`
}

type VersionDescription struct {
	KeyID           string     `json:"keyId"`
	Version         int64      `json:"version"`
	Algorithm       string     `json:"algorithm"`
	State           string     `json:"state"`
	Encryptable     bool       `json:"encryptable"`
	Decryptable     bool       `json:"decryptable"`
	EncryptNotAfter *time.Time `json:"encryptNotAfter,omitempty"`
	DecryptNotAfter *time.Time `json:"decryptNotAfter,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	DisabledAt      *time.Time `json:"disabledAt,omitempty"`
	DestroyAfter    *time.Time `json:"destroyAfter,omitempty"`
	DestroyedAt     *time.Time `json:"destroyedAt,omitempty"`
}

type CreateKeyInput struct {
	Name            string            `json:"name"`
	Description     string            `json:"description,omitempty"`
	PolicyReference string            `json:"policyReference,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	EncryptNotAfter *time.Time        `json:"encryptNotAfter,omitempty"`
	DecryptNotAfter *time.Time        `json:"decryptNotAfter,omitempty"`
	IdempotencyKey  string            `json:"-"`
}

type OptionalTime struct {
	Present bool
	Value   *time.Time
}

type UpdateKeyInput struct {
	Name             *string
	Description      *string
	PolicyReference  *string
	Labels           *map[string]string
	EncryptNotAfter  OptionalTime
	DecryptNotAfter  OptionalTime
	ExpectedRevision int64
	IdempotencyKey   string
}

type RotateKeyInput struct {
	EncryptNotAfter   *time.Time `json:"encryptNotAfter,omitempty"`
	DecryptNotAfter   *time.Time `json:"decryptNotAfter,omitempty"`
	OperatorReference string     `json:"operatorReference"`
	IdempotencyKey    string     `json:"-"`
}

type InventoryEvidence struct {
	EvidenceID         string    `json:"evidenceId"`
	DeploymentIdentity string    `json:"deploymentIdentity"`
	DatabaseIdentity   string    `json:"databaseIdentity"`
	KeyID              string    `json:"keyId"`
	Version            int64     `json:"version"`
	ResourceCount      int64     `json:"resourceCount"`
	Digest             string    `json:"digest"`
	CompletedAt        time.Time `json:"completedAt"`
}

type DisableKeyInput struct {
	OperatorReference string `json:"operatorReference"`
	IdempotencyKey    string `json:"-"`
}

type ScheduleDeletionInput struct {
	OperatorReference string            `json:"operatorReference"`
	DestroyAfter      time.Time         `json:"destroyAfter"`
	Inventory         InventoryEvidence `json:"inventory"`
	IdempotencyKey    string            `json:"-"`
}

type CancelDeletionInput struct {
	OperatorReference string `json:"operatorReference"`
	IdempotencyKey    string `json:"-"`
}

type DestroyKeyInput struct {
	OperatorReference string            `json:"operatorReference"`
	Inventory         InventoryEvidence `json:"inventory"`
	IdempotencyKey    string            `json:"-"`
}

type WrapResult struct {
	KeyID          string `json:"keyId"`
	WrappedDataKey []byte `json:"wrappedDataKey"`
}

type ServiceError struct {
	Status int
	Code   string
	Cause  error
}

func (e *ServiceError) Error() string {
	if e.Cause == nil {
		return e.Code
	}
	return fmt.Sprintf("%s: %v", e.Code, e.Cause)
}

func (e *ServiceError) Unwrap() error { return e.Cause }

func serviceError(status int, code string, cause error) error {
	return &ServiceError{Status: status, Code: code, Cause: cause}
}

func errorStatus(err error) (int, string) {
	var typed *ServiceError
	if errors.As(err, &typed) {
		return typed.Status, typed.Code
	}
	return http.StatusInternalServerError, "kms_internal_error"
}

func VersionedKeyID(keyID string, version int64) string {
	return fmt.Sprintf("%s/versions/%d", keyID, version)
}

func ParseVersionedKeyID(value string) (string, int64, error) {
	value = strings.TrimSpace(value)
	keyID, encodedVersion, found := strings.Cut(value, "/versions/")
	version, err := strconv.ParseInt(encodedVersion, 10, 64)
	parsedID, parseErr := uuid.Parse(keyID)
	if !found || err != nil || parseErr != nil || parsedID.String() != keyID || version <= 0 {
		return "", 0, serviceError(http.StatusBadRequest, "key_version_not_found", errors.New("invalid versioned key identity"))
	}
	if VersionedKeyID(keyID, version) != strings.TrimSpace(value) {
		return "", 0, serviceError(http.StatusBadRequest, "key_version_not_found", errors.New("invalid versioned key identity"))
	}
	return keyID, version, nil
}
