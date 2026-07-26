package schedulingpolicy_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/schedulingpolicy"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

type fixture struct {
	ctx      context.Context
	db       *gorm.DB
	service  *schedulingpolicy.Service
	tenantID uuid.UUID
	orgID    uuid.UUID
	userID   uuid.UUID
}

func openFixture(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	config, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "policy.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "scheduling-policy-test-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	return fixture{ctx: ctx, db: store.DB(), service: schedulingpolicy.NewService(store.DB()), tenantID: domain.TenantID, orgID: domain.OrganizationID, userID: domain.UserID}
}

func TestRuleSemanticsDistinguishAnyFromEmptyAllow(t *testing.T) {
	document := schedulingpolicy.UnrestrictedDocument()
	_, digest, err := schedulingpolicy.Normalize(document)
	if err != nil || digest != schedulingpolicy.UnrestrictedDigest {
		t.Fatalf("well-known unrestricted digest = %q, err = %v", digest, err)
	}
	if !schedulingpolicy.AllowsPlacement(document, "unknown-future-class") {
		t.Fatal("any capacity class must remain unrestricted")
	}
	document.CapacityClass = schedulingpolicy.Rule{Mode: schedulingpolicy.ModeAllow, Values: []string{}}
	if schedulingpolicy.AllowsPlacement(document, "standard") {
		t.Fatal("empty allow must deny the dimension")
	}
	document.Provider = schedulingpolicy.Rule{Mode: schedulingpolicy.ModeAllow, Values: []string{"codex"}}
	if !schedulingpolicy.AllowsTarget(document, schedulingpolicy.Target{ID: uuid.New(), Provider: "CODEX"}) {
		t.Fatal("allow provider should match its catalog canonical name")
	}
	if schedulingpolicy.AllowsTarget(document, schedulingpolicy.Target{ID: uuid.New(), Provider: "unknown"}) {
		t.Fatal("unknown provider must fail closed under allow mode")
	}

	targetID := uuid.New()
	document.Target = schedulingpolicy.Rule{Mode: schedulingpolicy.ModeAllow, Values: []string{targetID.String()}}
	if _, _, err := schedulingpolicy.Normalize(document); err != nil {
		t.Fatal(err)
	}
	document.Target.Values[0] = " " + targetID.String()
	if _, _, err := schedulingpolicy.Normalize(document); err == nil {
		t.Fatal("non-canonical Target UUID text was accepted")
	}
}

func TestUpdateResolveIntersectionAndCAS(t *testing.T) {
	f := openFixture(t)
	missing, err := f.service.GetTenant(f.ctx, f.tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if missing.Tenant.Version != 0 || missing.Tenant.Digest != schedulingpolicy.UnrestrictedDigest {
		t.Fatalf("missing head = %#v", missing.Tenant)
	}

	tenantDocument := schedulingpolicy.UnrestrictedDocument()
	tenantDocument.Region = schedulingpolicy.Rule{Mode: schedulingpolicy.ModeAllow, Values: []string{"us-east-1", "us-west-2"}}
	tenant, err := f.service.UpdateTenant(f.ctx, f.tenantID, schedulingpolicy.UpdateInput{ExpectedVersion: 0, Document: tenantDocument, ActorID: f.userID})
	if err != nil {
		t.Fatal(err)
	}
	if tenant.Tenant.Version != 1 {
		t.Fatalf("tenant version = %d", tenant.Tenant.Version)
	}

	organizationDocument := schedulingpolicy.UnrestrictedDocument()
	organizationDocument.Region = schedulingpolicy.Rule{Mode: schedulingpolicy.ModeAllow, Values: []string{"us-east-1"}}
	organization, err := f.service.UpdateOrganization(f.ctx, f.tenantID, f.orgID, schedulingpolicy.UpdateInput{ExpectedVersion: 0, Document: organizationDocument, ActorID: f.userID})
	if err != nil {
		t.Fatal(err)
	}
	if got := organization.Effective.Region.Values; len(got) != 1 || got[0] != "us-east-1" {
		t.Fatalf("effective intersection = %#v", got)
	}

	widening := schedulingpolicy.UnrestrictedDocument()
	widening.Region = schedulingpolicy.Rule{Mode: schedulingpolicy.ModeAllow, Values: []string{"eu-west-1"}}
	if _, err := f.service.UpdateOrganization(f.ctx, f.tenantID, f.orgID, schedulingpolicy.UpdateInput{ExpectedVersion: 1, Document: widening, ActorID: f.userID}); !errors.Is(err, schedulingpolicy.ErrOrganizationWidensTenant) {
		t.Fatalf("widening error = %v", err)
	}
	if _, err := f.service.UpdateTenant(f.ctx, f.tenantID, schedulingpolicy.UpdateInput{ExpectedVersion: 0, Document: tenantDocument, ActorID: f.userID}); !errors.Is(err, schedulingpolicy.ErrVersionConflict) {
		t.Fatalf("CAS error = %v", err)
	}

	var revisions int64
	if err := f.db.Model(&persistence.ExecutionSchedulingPolicyRevision{}).Where("tenant_id = ?", f.tenantID).Count(&revisions).Error; err != nil {
		t.Fatal(err)
	}
	if revisions != 2 {
		t.Fatalf("failed updates leaked revisions: %d", revisions)
	}
}

func TestResolveFailsClosedOnDigestTamperAndGuardsImmutableRows(t *testing.T) {
	f := openFixture(t)
	document := schedulingpolicy.UnrestrictedDocument()
	snapshot, err := f.service.UpdateTenant(f.ctx, f.tenantID, schedulingpolicy.UpdateInput{ExpectedVersion: 0, Document: document, ActorID: f.userID})
	if err != nil {
		t.Fatal(err)
	}
	revisionID := *snapshot.Tenant.RevisionID

	if err := f.db.Model(&persistence.ExecutionSchedulingPolicyRule{}).
		Where("tenant_id = ? AND revision_id = ? AND dimension = ?", f.tenantID, revisionID, schedulingpolicy.DimensionRegion).
		Update("mode", schedulingpolicy.ModeAllow).Error; err == nil {
		t.Fatal("immutable rule update succeeded")
	}

	if err := f.db.Exec(`DROP TRIGGER trg_execution_scheduling_policy_revisions_update`).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Exec(`UPDATE execution_scheduling_policy_revisions SET sha256 = ? WHERE id = ?`, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", revisionID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.GetTenant(f.ctx, f.tenantID); !errors.Is(err, schedulingpolicy.ErrPolicyCorrupt) {
		t.Fatalf("tampered digest error = %v", err)
	}
}

func TestLockEffectiveForCommitDetectsChangedHead(t *testing.T) {
	f := openFixture(t)
	document := schedulingpolicy.UnrestrictedDocument()
	snapshot, err := f.service.UpdateTenant(f.ctx, f.tenantID, schedulingpolicy.UpdateInput{ExpectedVersion: 0, Document: document, ActorID: f.userID})
	if err != nil {
		t.Fatal(err)
	}
	document.DenyAll = true
	if _, err := f.service.UpdateTenant(f.ctx, f.tenantID, schedulingpolicy.UpdateInput{ExpectedVersion: 1, Document: document, ActorID: f.userID}); err != nil {
		t.Fatal(err)
	}
	err = f.db.Transaction(func(tx *gorm.DB) error { return f.service.LockEffectiveForCommit(f.ctx, tx, f.tenantID, nil, snapshot) })
	if !errors.Is(err, schedulingpolicy.ErrPolicyStale) {
		t.Fatalf("stale lock error = %v", err)
	}
}
