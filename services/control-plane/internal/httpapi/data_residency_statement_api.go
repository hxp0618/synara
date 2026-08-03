package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/schedulingpolicy"
)

const dataResidencyStatementSchema = "synara-data-residency-statement-v1"

type dataResidencyStatement struct {
	SchemaVersion string                           `json:"schemaVersion"`
	GeneratedAt   time.Time                        `json:"generatedAt"`
	Tenant        dataResidencyStatementTenant     `json:"tenant"`
	Execution     dataResidencyStatementExecution  `json:"execution"`
	DataPlanes    dataResidencyStatementDataPlanes `json:"dataPlanes"`
	Limitation    string                           `json:"limitation"`
}

type dataResidencyStatementTenant struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	HomeRegion string    `json:"homeRegion"`
}

type dataResidencyStatementExecution struct {
	Status         string   `json:"status"`
	AllowedRegions []string `json:"allowedRegions"`
	PolicyVersion  int64    `json:"policyVersion"`
	PolicyDigest   string   `json:"policyDigest"`
	Enforcement    string   `json:"enforcement"`
}

type dataResidencyStatementDataPlanes struct {
	Metadata  string `json:"metadata"`
	Artifacts string `json:"artifacts"`
	KMS       string `json:"kms"`
}

func (s *Server) getTenantDataResidencyStatement(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok || !s.requireSchedulingPolicyTenantPermission(w, r, tenantID, authorization.SchedulingPolicyRead) {
		return
	}

	var tenant persistence.Tenant
	if err := s.db.WithContext(r.Context()).Where("id = ? AND deleted_at IS NULL", tenantID).Take(&tenant).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			s.writeError(w, r, problem.New(404, "tenant_not_found", "Tenant not found."))
			return
		}
		s.writeError(w, r, problem.Wrap(500, "tenant_load_failed", "Failed to load the tenant.", err))
		return
	}

	snapshot, err := s.schedulingPolicies.GetTenant(r.Context(), tenantID)
	if err != nil {
		s.writeError(w, r, executionSchedulingPolicyProblem(err))
		return
	}
	policy := snapshot.Tenant
	status := "unrestricted"
	allowedRegions := []string{}
	if policy.Document.DenyAll || policy.Document.Region.Mode == schedulingpolicy.ModeAllow {
		status = "enforced"
		allowedRegions = append(allowedRegions, policy.Document.Region.Values...)
	}

	statement := dataResidencyStatement{
		SchemaVersion: dataResidencyStatementSchema,
		GeneratedAt:   time.Now().UTC(),
		Tenant: dataResidencyStatementTenant{
			ID: tenant.ID, Name: tenant.Name, HomeRegion: tenant.Region,
		},
		Execution: dataResidencyStatementExecution{
			Status: status, AllowedRegions: allowedRegions, PolicyVersion: policy.Version,
			PolicyDigest: policy.Digest, Enforcement: "candidate-selection-and-commit",
		},
		DataPlanes: dataResidencyStatementDataPlanes{
			Metadata: "deployment-annex-required", Artifacts: "deployment-annex-required", KMS: "deployment-annex-required",
		},
		Limitation: "This statement is not a full data-residency promise unless execution is enforced and a signed deployment annex covers Metadata, Artifact, KMS, backup, log and support-processing Regions.",
	}
	payload, err := json.Marshal(statement)
	if err != nil {
		s.writeError(w, r, problem.Wrap(500, "data_residency_statement_failed", "The data residency statement could not be encoded.", err))
		return
	}
	// Match json.Encoder's newline convention used by the other JSON endpoints so the
	// integrity digest covers exactly the bytes delivered to the caller.
	payload = append(bytes.Clone(payload), '\n')
	digest := sha256.Sum256(payload)
	principal := mustPrincipal(r)
	if err := audit.Record(r.Context(), s.db, audit.Entry{
		TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
		Action: "tenant.data_residency_statement_exported", ResourceType: "data_residency_statement", ResourceID: &tenantID,
		RequestID: requestID(r), IPAddress: clientIP(r), Metadata: map[string]any{
			"schemaVersion": dataResidencyStatementSchema, "policyVersion": policy.Version,
			"policyDigest": policy.Digest, "status": status, "bytes": len(payload),
			"sha256": "sha256:" + hex.EncodeToString(digest[:]),
		},
	}); err != nil {
		s.writeError(w, r, problem.Wrap(500, "data_residency_statement_audit_failed", "The data residency statement export could not be audited.", err))
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="synara-data-residency-`+tenantID.String()+`.json"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Synara-Data-Residency-Schema", dataResidencyStatementSchema)
	w.Header().Set("X-Synara-Data-Residency-SHA256", hex.EncodeToString(digest[:]))
	w.Header().Set("X-Synara-Data-Residency-Bytes", strconv.Itoa(len(payload)))
	w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}
