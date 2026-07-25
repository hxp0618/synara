package sessions

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func TestCreateTurnPostgresConcurrentQuotaAllowsOnlyOneActiveExecution(t *testing.T) {
	db := openModelSwitchPostgresDB(t)
	fixture := seedPostgresModelSwitchFixture(t, db)

	maxExecutions := 1
	if err := db.Create(&persistence.TenantQuota{
		TenantID: fixture.tenantID, MaxConcurrentExecutions: &maxExecutions, UpdatedBy: fixture.principal.UserID,
	}).Error; err != nil {
		t.Fatal(err)
	}

	secondSessionID := createPostgresQuotaSession(t, db, fixture, "postgres-quota-second")
	sessionIDs := []uuid.UUID{fixture.sessionID, secondSessionID}

	type result struct {
		sessionID uuid.UUID
		turn      Turn
		err       error
	}

	start := make(chan struct{})
	results := make(chan result, len(sessionIDs))
	for index, sessionID := range sessionIDs {
		index, sessionID := index, sessionID
		go func() {
			<-start
			turn, err := fixture.service.CreateTurn(
				context.Background(),
				fixture.principal,
				sessionID,
				CreateTurnInput{InputText: "postgres quota turn"},
				"postgres-quota-turn-"+string(rune('a'+index)),
				"127.0.0.1",
			)
			results <- result{sessionID: sessionID, turn: turn, err: err}
		}()
	}
	close(start)

	successes := make([]result, 0, 1)
	quotaFailures := make([]result, 0, 1)
	for range sessionIDs {
		outcome := <-results
		switch {
		case outcome.err == nil:
			successes = append(successes, outcome)
		case executionQuotaProblemCode(outcome.err) == "execution_quota_exceeded":
			quotaFailures = append(quotaFailures, outcome)
		default:
			t.Fatalf("unexpected concurrent CreateTurn outcome for session %s: %#v", outcome.sessionID, outcome)
		}
	}

	if len(successes) != 1 || len(quotaFailures) != 1 {
		t.Fatalf("concurrent quota outcomes successes/failures = %d/%d", len(successes), len(quotaFailures))
	}
	if successes[0].turn.ID == uuid.Nil {
		t.Fatalf("successful CreateTurn omitted turn id: %#v", successes[0])
	}

	assertPostgresQuotaExecutionCount(
		t,
		db,
		"tenant_id = ? AND status IN ?",
		1,
		fixture.tenantID,
		activeSessionExecutionStatuses,
	)
	assertPostgresQuotaExecutionCount(
		t,
		db,
		"tenant_id = ?",
		1,
		fixture.tenantID,
	)
	assertPostgresQuotaExecutionCount(
		t,
		db,
		"tenant_id = ? AND session_id = ?",
		1,
		fixture.tenantID,
		successes[0].sessionID,
	)
	assertPostgresQuotaExecutionCount(
		t,
		db,
		"tenant_id = ? AND session_id = ?",
		0,
		fixture.tenantID,
		quotaFailures[0].sessionID,
	)
}

func createPostgresQuotaSession(
	t *testing.T,
	db *gorm.DB,
	fixture tenantExecutionPolicyFixture,
	title string,
) uuid.UUID {
	t.Helper()

	sessionID := uuid.New()
	if err := db.Create(&persistence.AgentSession{
		ID: sessionID, TenantID: fixture.tenantID, OrganizationID: fixture.organizationID,
		ProjectID: fixture.projectID, CreatedBy: fixture.principal.UserID, Title: title,
		Status: "active", Visibility: "private", Provider: "codex", ExecutionTargetID: fixture.executionTargetID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	return sessionID
}

func assertPostgresQuotaExecutionCount(
	t *testing.T,
	db *gorm.DB,
	query string,
	want int64,
	args ...any,
) {
	t.Helper()

	var count int64
	if err := db.Model(&persistence.AgentExecution{}).Where(query, args...).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("execution count for %q = %d, want %d", query, count, want)
	}
}

func executionQuotaProblemCode(err error) string {
	var apiError *problem.Error
	if errors.As(err, &apiError) {
		return apiError.Code
	}
	return ""
}
