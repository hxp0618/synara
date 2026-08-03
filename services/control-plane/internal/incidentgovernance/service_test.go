package incidentgovernance

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestIncidentGovernanceRecordsSeparatedPublicTimelineAndSecurityResolution(t *testing.T) {
	fixture := newIncidentFixture(t)
	incident := fixture.create(t, true)
	if incident.State != "investigating" || incident.Version != 1 || incident.Cadence.FirstInternalUpdateTargetAt == nil ||
		incident.InternalStatusBoardOrigin == nil || *incident.InternalStatusBoardOrigin != "https://status.example.test" ||
		incident.IncidentCommander.UserID != fixture.commanderID || incident.CommunicationsLead.UserID != fixture.communicationsID ||
		incident.SecurityPrivacyLead == nil || incident.SecurityPrivacyLead.UserID != fixture.securityID {
		t.Fatalf("created incident = %#v", incident)
	}

	var err error
	incident, err = fixture.service.BindStatusBoard(fixture.ctx, fixture.commanderID, incident.ID, BindStatusBoardInput{
		ExpectedVersion: incident.Version, InternalStatusBoardIncidentReference: "status-provider-incident-117",
		Reason: "Bind the independently hosted internal Status Board incident.",
	}, "bind-status", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	incident, err = fixture.service.AddInternalUpdate(fixture.ctx, fixture.communicationsID, incident.ID, AddUpdateInput{
		ExpectedVersion: incident.Version, Kind: "initial",
		Summary:           "We are investigating elevated errors affecting execution scheduling.",
		PublishedAt:       fixture.now.Add(5 * time.Minute),
		EvidenceReference: "https://status.example.test/incidents/status-provider-incident-117/updates/initial",
	}, "initial-update", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if len(incident.InternalUpdates) != 1 || incident.InternalUpdates[0].CreatedBy.UserID != fixture.communicationsID ||
		incident.Cadence.FirstInternalUpdateWithinTarget == nil || !*incident.Cadence.FirstInternalUpdateWithinTarget {
		t.Fatalf("initial internal timeline = %#v", incident)
	}
	fixture.assertNotification(t, incident, 1, "initial")
	incident = fixture.transition(t, incident, "identified")
	incident = fixture.transition(t, incident, "monitoring")
	incident, err = fixture.service.AddInternalUpdate(fixture.ctx, fixture.communicationsID, incident.ID, AddUpdateInput{
		ExpectedVersion: incident.Version, Kind: "progress",
		Summary:           "The affected queue lane is stable and customer recovery checks are running.",
		PublishedAt:       fixture.now.Add(20 * time.Minute),
		EvidenceReference: "https://status.example.test/incidents/status-provider-incident-117/updates/progress",
	}, "progress-update", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	incident, err = fixture.service.AddInternalUpdate(fixture.ctx, fixture.communicationsID, incident.ID, AddUpdateInput{
		ExpectedVersion: incident.Version, Kind: "resolved",
		Summary:           "Service has recovered and completed an external customer-path observation window.",
		PublishedAt:       fixture.now.Add(25 * time.Minute),
		EvidenceReference: "https://status.example.test/incidents/status-provider-incident-117/updates/resolved",
	}, "resolved-update", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if incident.Cadence.NextInternalUpdateTargetAt != nil {
		t.Fatalf("resolved internal update retained next deadline: %#v", incident.Cadence)
	}

	if _, err := fixture.service.Transition(fixture.ctx, fixture.commanderID, incident.ID, TransitionInput{
		ExpectedVersion: incident.Version, TargetState: "resolved",
		Reason: "Attempt resolution before independent Security and Privacy approval.",
	}, "premature-resolve", "127.0.0.1"); problemCode(err) != "incident_security_approval_required" {
		t.Fatalf("premature resolution error = %v", err)
	}
	_, err = fixture.service.RecordResolutionApproval(fixture.ctx, fixture.securityID, incident.ID, ResolutionApprovalInput{
		Decision: "approved", Reason: "Security and Privacy must bind exact non-zero resolution evidence bytes.",
		EvidenceReference: "https://evidence.example.test/incidents/status-provider-incident-117/zero-digest",
		EvidenceSHA256:    "sha256:" + strings.Repeat("0", 64),
	}, "zero-digest", "127.0.0.1")
	if problemCode(err) != "incident_resolution_approval_invalid" {
		t.Fatalf("zero digest resolution approval error = %v", err)
	}
	incident, err = fixture.service.RecordResolutionApproval(fixture.ctx, fixture.securityID, incident.ID, ResolutionApprovalInput{
		Decision: "approved", Reason: "Security and Privacy reviewed containment and the customer-safe resolution evidence.",
		EvidenceReference: "https://evidence.example.test/incidents/status-provider-incident-117/security-resolution",
		EvidenceSHA256:    "sha256:" + strings.Repeat("2", 64),
	}, "security-resolution", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	incident = fixture.transition(t, incident, "resolved")
	if incident.State != "resolved" || incident.ResolvedAt == nil || incident.Cadence.Status != "resolved" ||
		incident.ResolutionApproval == nil || incident.ResolutionApproval.Decision != "approved" {
		t.Fatalf("resolved incident = %#v", incident)
	}
	if incident.ResolutionApproval.EvidenceSHA256 == nil || incident.ResolutionApproval.SupersededAt != nil || len(incident.ResolutionApprovals) != 1 {
		t.Fatalf("byte-bound incident resolution approval = %#v", incident.ResolutionApprovals)
	}
	fixture.assertNotification(t, incident, 3, "resolved")

	if err := fixture.db.Model(&persistence.Stage6IncidentUpdate{}).
		Where("incident_id = ? AND update_kind = ?", incident.ID, "initial").
		Update("summary", "mutated public history").Error; err == nil {
		t.Fatal("public incident update was mutable")
	}
	if err := fixture.db.Where("incident_id = ?", incident.ID).
		Delete(&persistence.Stage6IncidentResolutionApproval{}).Error; err == nil {
		t.Fatal("incident resolution approval was deletable")
	}
	if err := fixture.db.Model(&persistence.Stage6IncidentResolutionApproval{}).Where("incident_id = ?", incident.ID).
		Update("evidence_sha256", "sha256:"+strings.Repeat("f", 64)).Error; err == nil {
		t.Fatal("incident resolution approval evidence digest was mutable")
	}
	if err := fixture.db.Delete(&persistence.Stage6Incident{}, "id = ?", incident.ID).Error; err == nil {
		t.Fatal("incident operational history was deletable")
	}
	var auditCount int64
	if err := fixture.db.Model(&persistence.AuditLog{}).
		Where("tenant_id = ? AND resource_id = ? AND action LIKE ?", fixture.operatorTenantID, incident.ID, "incident.%").
		Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if auditCount != 9 {
		t.Fatalf("incident Audit event count = %d, want 9", auditCount)
	}
}

func TestIncidentGovernanceFailsClosedOnRolesOriginsAndOrdering(t *testing.T) {
	fixture := newIncidentFixture(t)
	unconfigured := NewService(fixture.db, fixture.operatorTenantID, "")
	unconfigured.now = fixture.service.now
	if _, err := unconfigured.Create(fixture.ctx, fixture.commanderID, fixture.createInput(false), "unconfigured", "127.0.0.1"); problemCode(err) != "internal_status_board_unconfigured" {
		t.Fatalf("unconfigured internal Status Board create error = %v", err)
	}
	input := fixture.createInput(true)
	input.CommunicationsLeadUserID = fixture.commanderID
	if _, err := fixture.service.Create(fixture.ctx, fixture.commanderID, input, "same-role", "127.0.0.1"); problemCode(err) != "incident_invalid" {
		t.Fatalf("same-role create error = %v", err)
	}

	incident := fixture.create(t, false)
	bound, err := fixture.service.BindStatusBoard(fixture.ctx, fixture.communicationsID, incident.ID, BindStatusBoardInput{
		ExpectedVersion: incident.Version, InternalStatusBoardIncidentReference: "status-provider-incident-order",
		Reason: "Bind the internal Status Board before publishing employee updates.",
	}, "bind-order", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Create(&persistence.Stage6IncidentUpdate{
		ID: uuid.New(), IncidentID: bound.ID, OperatorTenantID: fixture.operatorTenantID,
		Kind: "initial", Summary: "Direct SQL cannot record evidence from a different internal Status Board authority.",
		PublishedAt: fixture.now.Add(90 * time.Second), EvidenceReference: "https://other-status.example.test/incidents/direct-bypass",
		CreatedBy: fixture.communicationsID, CreatedAt: fixture.now.Add(90 * time.Second),
	}).Error; err == nil {
		t.Fatal("database accepted an internal update outside the incident Status Board origin")
	}
	_, err = fixture.service.AddInternalUpdate(fixture.ctx, fixture.communicationsID, bound.ID, AddUpdateInput{
		ExpectedVersion: bound.Version, Kind: "progress",
		Summary:           "A progress update cannot be the first internal update for an incident.",
		PublishedAt:       fixture.now.Add(2 * time.Minute),
		EvidenceReference: "https://status.example.test/incidents/status-provider-incident-order/updates/progress",
	}, "progress-first", "127.0.0.1")
	if problemCode(err) != "incident_initial_update_required" {
		t.Fatalf("progress-first error = %v", err)
	}
	_, err = fixture.service.AddInternalUpdate(fixture.ctx, fixture.communicationsID, bound.ID, AddUpdateInput{
		ExpectedVersion: bound.Version, Kind: "initial",
		Summary:           "The first update points to the wrong internal Status Board origin.",
		PublishedAt:       fixture.now.Add(2 * time.Minute),
		EvidenceReference: "https://other-status.example.test/incidents/wrong-origin",
	}, "wrong-origin", "127.0.0.1")
	if problemCode(err) != "incident_internal_update_invalid" {
		t.Fatalf("wrong-origin error = %v", err)
	}
	_, err = fixture.service.AddInternalUpdate(fixture.ctx, fixture.communicationsID, bound.ID, AddUpdateInput{
		ExpectedVersion: bound.Version, Kind: "initial",
		Summary:           "The update accidentally contains token: super-secret-value and must be rejected.",
		PublishedAt:       fixture.now.Add(2 * time.Minute),
		EvidenceReference: "https://status.example.test/incidents/status-provider-incident-order/updates/initial",
	}, "sensitive-summary", "127.0.0.1")
	if problemCode(err) != "incident_internal_update_sensitive_content" {
		t.Fatalf("sensitive summary error = %v", err)
	}
	_, err = fixture.service.AddInternalUpdate(fixture.ctx, fixture.commanderID, bound.ID, AddUpdateInput{
		ExpectedVersion: bound.Version, Kind: "initial",
		Summary:           "The incident commander cannot impersonate the assigned communications lead.",
		PublishedAt:       fixture.now.Add(2 * time.Minute),
		EvidenceReference: "https://status.example.test/incidents/status-provider-incident-order/updates/initial",
	}, "wrong-role", "127.0.0.1")
	if problemCode(err) != "incident_internal_update_conflict" {
		t.Fatalf("wrong-role error = %v", err)
	}
}

func TestOutboxMessageStatusProjectionIsFailClosedAndDeterministic(t *testing.T) {
	now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	published := now.Add(time.Minute)
	deadLettered := now.Add(2 * time.Minute)
	cases := []struct {
		name     string
		message  persistence.OutboxMessage
		expected string
	}{
		{name: "pending", message: persistence.OutboxMessage{}, expected: "pending"},
		{name: "retrying", message: persistence.OutboxMessage{Attempts: 1}, expected: "retrying"},
		{name: "published wins", message: persistence.OutboxMessage{Attempts: 4, PublishedAt: &published}, expected: "published"},
		{name: "dead letter wins over retry", message: persistence.OutboxMessage{Attempts: 4, DeadLetteredAt: &deadLettered}, expected: "dead-letter"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := outboxMessageStatus(testCase.message); got != testCase.expected {
				t.Fatalf("outboxMessageStatus() = %q, want %q", got, testCase.expected)
			}
		})
	}
}

type incidentFixture struct {
	ctx              context.Context
	db               *gorm.DB
	service          *Service
	now              time.Time
	operatorTenantID uuid.UUID
	commanderID      uuid.UUID
	communicationsID uuid.UUID
	securityID       uuid.UUID
}

func newIncidentFixture(t *testing.T) incidentFixture {
	t.Helper()
	ctx := context.Background()
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, profile, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "incident-governance-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second).Add(-30 * time.Minute)
	communicationsID := createIncidentOperator(t, store.DB(), domain.TenantID, "incident-comms@example.test", "Incident Comms", "admin", now)
	securityID := createIncidentOperator(t, store.DB(), domain.TenantID, "incident-security@example.test", "Incident Security", "security_admin", now)
	service := NewService(store.DB(), domain.TenantID, "https://status.example.test/history")
	service.now = func() time.Time { return now.Add(30 * time.Minute) }
	return incidentFixture{
		ctx: ctx, db: store.DB(), service: service, now: now,
		operatorTenantID: domain.TenantID, commanderID: domain.UserID,
		communicationsID: communicationsID, securityID: securityID,
	}
}

func createIncidentOperator(t *testing.T, db *gorm.DB, tenantID uuid.UUID, email, name, role string, now time.Time) uuid.UUID {
	t.Helper()
	user := persistence.User{
		ID: uuid.New(), Email: email, DisplayName: name, Status: "active",
		EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.TenantMembership{
		TenantID: tenantID, UserID: user.ID, Role: role, Status: "active",
		JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	return user.ID
}

func (f incidentFixture) createInput(security bool) CreateInput {
	input := CreateInput{
		IncidentKey: "INC-2026-08-01-EXECUTION", Severity: "SEV-1",
		Title:                 "Execution scheduling degradation",
		InternalImpactSummary: "Customers may see delayed execution starts while the affected queue lane is recovered.",
		BroadInternalImpact:   true, SecurityPrivacyImpact: security,
		AffectedComponents: []string{"execution-scheduling", "worker-runtime"},
		AffectedRegions:    []string{"region-one"}, CommunicationsLeadUserID: f.communicationsID,
		StartedAt: f.now, ImpactConfirmedAt: f.now.Add(time.Minute),
	}
	if security {
		input.SecurityPrivacyLeadUserID = &f.securityID
	}
	return input
}

func (f incidentFixture) create(t *testing.T, security bool) Incident {
	t.Helper()
	input := f.createInput(security)
	if !security {
		input.IncidentKey = "INC-2026-08-01-ORDER"
	}
	incident, err := f.service.Create(f.ctx, f.commanderID, input, "create-incident", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return incident
}

func (f incidentFixture) transition(t *testing.T, incident Incident, target string) Incident {
	t.Helper()
	result, err := f.service.Transition(f.ctx, f.commanderID, incident.ID, TransitionInput{
		ExpectedVersion: incident.Version, TargetState: target,
		Reason: "Advance the incident after the required operational evidence is recorded.",
	}, "transition-"+target, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func (f incidentFixture) assertNotification(t *testing.T, incident Incident, expectedCount int, lastKind string) {
	t.Helper()
	if len(incident.InternalNotifications) != expectedCount {
		t.Fatalf("incident notification projection count = %d, want %d", len(incident.InternalNotifications), expectedCount)
	}
	projected := false
	for _, notification := range incident.InternalNotifications {
		if notification.Kind == lastKind && notification.Status == "pending" && notification.Attempts == 0 {
			projected = true
			break
		}
	}
	if !projected {
		t.Fatalf("incident notification projection missing %q: %#v", lastKind, incident.InternalNotifications)
	}
	var messages []persistence.OutboxMessage
	if err := f.db.Where("tenant_id = ? AND topic = ?", f.operatorTenantID, internalIncidentUpdateTopic).
		Order("created_at, id").Find(&messages).Error; err != nil {
		t.Fatal(err)
	}
	if len(messages) != expectedCount {
		t.Fatalf("internal incident notification count = %d, want %d", len(messages), expectedCount)
	}
	message := persistence.OutboxMessage{}
	for _, candidate := range messages {
		if fmt.Sprint(candidate.Payload["kind"]) == lastKind {
			message = candidate
			break
		}
	}
	if message.ID == uuid.Nil {
		t.Fatalf("no %q internal incident notification in %#v", lastKind, messages)
	}
	if message.MessageKey == "" || fmt.Sprint(message.Headers["eventVersion"]) != "1" ||
		message.Headers["delivery"] != "employee-safe-internal-notification" ||
		message.Payload["eventType"] != "incident.internal_update" ||
		message.Payload["incidentId"] != incident.ID.String() || message.Payload["incidentKey"] != incident.IncidentKey ||
		message.Payload["kind"] != lastKind || message.Payload["summary"] == nil || message.Payload["statusBoardOrigin"] != "https://status.example.test" ||
		message.Payload["statusBoardIncidentReference"] == nil {
		t.Fatalf("unsafe or incomplete internal incident notification = %#v", message)
	}
	encoded := strings.ToLower(fmt.Sprint(message.Payload))
	for _, forbidden := range []string{"email", "token", "secret", "credential", "prompt", "password"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("internal incident notification contains forbidden field marker %q: %s", forbidden, encoded)
		}
	}
}

func problemCode(err error) string {
	if err == nil {
		return ""
	}
	var value *problem.Error
	if errors.As(err, &value) {
		return value.Code
	}
	return ""
}
