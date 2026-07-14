package sessions

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/projects"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestSwitchModelPostgresConcurrentCASAllowsOnlyOneWinner(t *testing.T) {
	db := openModelSwitchPostgresDB(t)
	fixture := seedPostgresModelSwitchFixture(t, db)
	previousModel := "gpt-5"
	models := []string{"gpt-5.6", "gpt-6"}
	type result struct {
		requested string
		session   Session
		replayed  bool
		err       error
	}
	start := make(chan struct{})
	results := make(chan result, len(models))
	for index, model := range models {
		index, model := index, model
		go func() {
			<-start
			session, replayed, err := fixture.service.SwitchModelWithIdempotency(
				context.Background(), fixture.principal, fixture.sessionID,
				SwitchModelInput{Model: model, ExpectedModel: &previousModel, ExpectedModelProvided: true},
				"postgres-model-switch-"+string(rune('a'+index)), "postgres-concurrent-switch", "127.0.0.1",
			)
			results <- result{requested: model, session: session, replayed: replayed, err: err}
		}()
	}
	close(start)

	successes := make([]result, 0, 1)
	conflicts := make([]result, 0, 1)
	for range models {
		item := <-results
		if item.err == nil {
			successes = append(successes, item)
			continue
		}
		if modelSwitchProblemCode(item.err) == "session_model_conflict" {
			conflicts = append(conflicts, item)
			continue
		}
		t.Fatalf("unexpected concurrent model switch error for %s: %v", item.requested, item.err)
	}
	if len(successes) != 1 || len(conflicts) != 1 {
		t.Fatalf("concurrent model switch successes/conflicts = %d/%d", len(successes), len(conflicts))
	}
	winner := successes[0]
	if winner.replayed || winner.session.Model == nil || *winner.session.Model != winner.requested ||
		winner.session.LastEventSequence != 1 {
		t.Fatalf("winning model switch = %#v", winner)
	}

	stored := loadPostgresModelSwitchSession(t, db, fixture)
	if stored.Model == nil || *stored.Model != winner.requested || stored.LastEventSequence != 1 ||
		stored.CurrentRuntimeBindingID == nil {
		t.Fatalf("stored Session after concurrent CAS = %#v", stored)
	}
	bindings := loadPostgresModelSwitchBindings(t, db, fixture)
	if len(bindings) != 2 || bindings[0].Revision != 1 || bindings[0].Status != "released" ||
		bindings[1].Revision != 2 || bindings[1].Status != "active" ||
		*stored.CurrentRuntimeBindingID != bindings[1].ID {
		t.Fatalf("bindings after concurrent CAS = %#v, session=%#v", bindings, stored)
	}
	assertPostgresModelSwitchCount(t, db, &persistence.SessionEvent{},
		"tenant_id = ? AND session_id = ? AND event_type = ?", 1,
		fixture.tenantID, fixture.sessionID, "session.model.changed")
	assertPostgresModelSwitchCount(t, db, &persistence.AuditLog{},
		"tenant_id = ? AND resource_id = ? AND action = ?", 1,
		fixture.tenantID, fixture.sessionID, "session.model.changed")
	assertPostgresModelSwitchCount(t, db, &persistence.APIIdempotencyKey{},
		"tenant_id = ? AND actor_id = ? AND idempotency_key IN ?", 1,
		fixture.tenantID, fixture.principal.UserID,
		[]string{"postgres-model-switch-a", "postgres-model-switch-b"})
}

func TestSwitchModelPostgresLinearizesWithCreateTurn(t *testing.T) {
	db := openModelSwitchPostgresDB(t)
	fixture := seedPostgresModelSwitchFixture(t, db)
	previousModel := "gpt-5"
	nextModel := "gpt-5.6"
	type switchResult struct {
		session  Session
		replayed bool
		err      error
	}
	type turnResult struct {
		turn     Turn
		replayed bool
		err      error
	}
	start := make(chan struct{})
	switchResults := make(chan switchResult, 1)
	turnResults := make(chan turnResult, 1)
	go func() {
		<-start
		session, replayed, err := fixture.service.SwitchModelWithIdempotency(
			context.Background(), fixture.principal, fixture.sessionID,
			SwitchModelInput{Model: nextModel, ExpectedModel: &previousModel, ExpectedModelProvided: true},
			"postgres-linear-switch", "postgres-linear-switch", "127.0.0.1",
		)
		switchResults <- switchResult{session: session, replayed: replayed, err: err}
	}()
	go func() {
		<-start
		turn, replayed, err := fixture.service.CreateTurnWithIdempotency(
			context.Background(), fixture.principal, fixture.sessionID,
			CreateTurnInput{InputText: "linearize model switch with Turn creation"},
			"postgres-linear-turn", "postgres-linear-turn", "127.0.0.1",
		)
		turnResults <- turnResult{turn: turn, replayed: replayed, err: err}
	}()
	close(start)
	switchOutcome := <-switchResults
	turnOutcome := <-turnResults
	if turnOutcome.err != nil || turnOutcome.replayed {
		t.Fatalf("concurrent CreateTurn = %#v", turnOutcome)
	}
	if switchOutcome.err != nil && modelSwitchProblemCode(switchOutcome.err) != "session_execution_active" {
		t.Fatalf("concurrent SwitchModel error = %v", switchOutcome.err)
	}

	var execution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND turn_id = ?", fixture.tenantID, turnOutcome.turn.ID).
		Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	stored := loadPostgresModelSwitchSession(t, db, fixture)
	bindings := loadPostgresModelSwitchBindings(t, db, fixture)
	if execution.ProviderRuntimeBindingID == nil || stored.CurrentRuntimeBindingID == nil ||
		*execution.ProviderRuntimeBindingID != *stored.CurrentRuntimeBindingID {
		t.Fatalf("Turn did not use the linearized current binding: execution=%#v session=%#v", execution, stored)
	}
	assertPostgresModelSwitchCount(t, db, &persistence.AgentExecution{},
		"tenant_id = ? AND session_id = ? AND status IN ?", 1,
		fixture.tenantID, fixture.sessionID, activeSessionExecutionStatuses)

	var modelEventCount int64
	if err := db.Model(&persistence.SessionEvent{}).
		Where("tenant_id = ? AND session_id = ? AND event_type = ?", fixture.tenantID, fixture.sessionID, "session.model.changed").
		Count(&modelEventCount).Error; err != nil {
		t.Fatal(err)
	}
	if switchOutcome.err == nil {
		if switchOutcome.replayed || switchOutcome.session.Model == nil || *switchOutcome.session.Model != nextModel ||
			stored.Model == nil || *stored.Model != nextModel || modelEventCount != 1 {
			t.Fatalf("SwitchModel-first linearization is inconsistent: switch=%#v session=%#v events=%d", switchOutcome, stored, modelEventCount)
		}
		if len(bindings) != 2 || bindings[0].Status != "released" || bindings[1].Status != "active" ||
			bindings[1].Revision != 2 || *execution.ProviderRuntimeBindingID != bindings[1].ID {
			t.Fatalf("SwitchModel-first bindings = %#v execution=%#v", bindings, execution)
		}
		var modelEvent, turnEvent persistence.SessionEvent
		if err := db.Where("tenant_id = ? AND session_id = ? AND event_type = ?", fixture.tenantID, fixture.sessionID, "session.model.changed").
			Take(&modelEvent).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Where("tenant_id = ? AND session_id = ? AND event_type = ?", fixture.tenantID, fixture.sessionID, "turn.created").
			Take(&turnEvent).Error; err != nil {
			t.Fatal(err)
		}
		if modelEvent.Sequence >= turnEvent.Sequence {
			t.Fatalf("SwitchModel-first event order = model:%d turn:%d", modelEvent.Sequence, turnEvent.Sequence)
		}
		return
	}

	if stored.Model == nil || *stored.Model != previousModel || modelEventCount != 0 {
		t.Fatalf("CreateTurn-first linearization is inconsistent: switch=%#v session=%#v events=%d", switchOutcome, stored, modelEventCount)
	}
	if len(bindings) != 1 || bindings[0].Status != "active" || bindings[0].Revision != 1 ||
		*execution.ProviderRuntimeBindingID != bindings[0].ID {
		t.Fatalf("CreateTurn-first bindings = %#v execution=%#v", bindings, execution)
	}
}

func openModelSwitchPostgresDB(t *testing.T) *gorm.DB {
	t.Helper()
	databaseURL := os.Getenv("SYNARA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_DATABASE_URL is not configured")
	}
	db, err := database.Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := database.Migrate(context.Background(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	return db
}

func seedPostgresModelSwitchFixture(t *testing.T, db *gorm.DB) tenantExecutionPolicyFixture {
	t.Helper()
	now := time.Now().UTC()
	userID := uuid.New()
	tenantID := uuid.New()
	organizationID := uuid.New()
	projectID := uuid.New()
	targetID := uuid.New()
	sessionID := uuid.New()
	bindingID := uuid.New()
	previousModel := "gpt-5"
	slug := "model-switch-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	if err := db.Transaction(func(tx *gorm.DB) error {
		models := []any{
			&persistence.User{
				ID: userID, Email: uuid.NewString() + "@example.com", DisplayName: "Model switch PostgreSQL",
				Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
			},
			&persistence.Tenant{
				ID: tenantID, Slug: slug, Name: "Model switch PostgreSQL", Status: "active",
				PlanCode: "free", Region: "default", Settings: map[string]any{}, CreatedBy: userID,
				CreatedAt: now, UpdatedAt: now,
			},
			&persistence.TenantMembership{
				TenantID: tenantID, UserID: userID, Role: "owner", Status: "active",
				JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
			},
			&persistence.Organization{
				ID: organizationID, TenantID: tenantID, Slug: "root", Name: "Root", Kind: "root",
				Status: "active", Settings: map[string]any{}, CreatedBy: userID, CreatedAt: now, UpdatedAt: now,
			},
			&persistence.OrganizationMembership{
				TenantID: tenantID, OrganizationID: organizationID, UserID: userID,
				Role: "owner", Status: "active", CreatedAt: now, UpdatedAt: now,
			},
			&persistence.Project{
				ID: projectID, TenantID: tenantID, OrganizationID: organizationID,
				Name: "Model switch project", DefaultBranch: "main", Visibility: "organization",
				CreatedBy: userID, CreatedAt: now, UpdatedAt: now,
			},
			&persistence.ExecutionTarget{
				ID: targetID, TenantID: &tenantID, OrganizationID: &organizationID,
				Kind: "local", Name: "model-switch-local", Status: "active",
				ConfigurationEncrypted: []byte{}, Capabilities: enabledProviderPolicyTestCapabilities(),
				CreatedAt: now, UpdatedAt: now,
			},
			&persistence.AgentSession{
				ID: sessionID, TenantID: tenantID, OrganizationID: organizationID, ProjectID: projectID,
				CreatedBy: userID, Title: "Model switch Session", Status: "active", Visibility: "private",
				Provider: "codex", Model: &previousModel, ExecutionTargetID: targetID,
				ProviderResumeCursorState: "absent", CreatedAt: now, UpdatedAt: now,
			},
			&persistence.ProviderRuntimeBinding{
				ID: bindingID, TenantID: tenantID, SessionID: sessionID, Provider: "codex",
				Revision: 1, Status: "active", ResumeStrategy: "authoritative-history",
				CreatedAt: now, UpdatedAt: now,
			},
		}
		for _, model := range models {
			if err := tx.Create(model).Error; err != nil {
				return err
			}
		}
		return tx.Model(&persistence.AgentSession{}).
			Where("tenant_id = ? AND id = ?", tenantID, sessionID).
			Update("current_runtime_binding_id", bindingID).Error
	}); err != nil {
		t.Fatalf("seed PostgreSQL model switch fixture: %v", err)
	}
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	targetService := executiontargets.NewService(db, profile, nil)
	fixture := tenantExecutionPolicyFixture{
		db: db, service: NewService(db, projects.NewService(db), targetService),
		principal: identity.Principal{UserID: userID, ActiveTenantID: &tenantID},
		tenantID:  tenantID, organizationID: organizationID, projectID: projectID,
		sessionID: sessionID, executionTargetID: targetID,
	}
	seedSessionCapabilityWorker(t, fixture, sessionCapabilityManifestOptions{})
	return fixture
}

func loadPostgresModelSwitchSession(
	t *testing.T,
	db *gorm.DB,
	fixture tenantExecutionPolicyFixture,
) persistence.AgentSession {
	t.Helper()
	var session persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	return session
}

func loadPostgresModelSwitchBindings(
	t *testing.T,
	db *gorm.DB,
	fixture tenantExecutionPolicyFixture,
) []persistence.ProviderRuntimeBinding {
	t.Helper()
	bindings := make([]persistence.ProviderRuntimeBinding, 0)
	if err := db.Where("tenant_id = ? AND session_id = ?", fixture.tenantID, fixture.sessionID).
		Order("revision").Find(&bindings).Error; err != nil {
		t.Fatal(err)
	}
	return bindings
}

func assertPostgresModelSwitchCount(
	t *testing.T,
	db *gorm.DB,
	model any,
	query string,
	want int64,
	args ...any,
) {
	t.Helper()
	var count int64
	if err := db.Model(model).Where(query, args...).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("%T count = %d, want %d", model, count, want)
	}
}

func modelSwitchProblemCode(err error) string {
	var apiError *problem.Error
	if errors.As(err, &apiError) {
		return apiError.Code
	}
	return ""
}
