package supportaccess

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/tenancy"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/postgresisolation"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresConcurrentSupportApprovalHasOneWinner(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_DATABASE_URL is not configured")
	}
	databaseURL = postgresisolation.URL(t, databaseURL)
	ctx := context.Background()
	firstDB, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	firstSQL, err := firstDB.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = firstSQL.Close() })
	if err := database.Migrate(ctx, firstDB, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, firstDB, platform.ProfilePersonal, "postgres-support-access-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	supportUser := persistence.User{ID: uuid.New(), Email: "pg-support@example.com", DisplayName: "PG Support", Status: "active", EmailVerifiedAt: &now}
	secondAdmin := persistence.User{ID: uuid.New(), Email: "pg-admin@example.com", DisplayName: "PG Admin", Status: "active", EmailVerifiedAt: &now}
	customerUser := persistence.User{ID: uuid.New(), Email: "pg-customer@example.com", DisplayName: "PG Customer", Status: "active", EmailVerifiedAt: &now}
	for _, user := range []persistence.User{supportUser, secondAdmin, customerUser} {
		if err := firstDB.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
	}
	joinedAt := now
	for _, membership := range []persistence.TenantMembership{
		{TenantID: domain.TenantID, UserID: supportUser.ID, Role: "security_admin", Status: "active", JoinedAt: &joinedAt},
		{TenantID: domain.TenantID, UserID: secondAdmin.ID, Role: "admin", Status: "active", JoinedAt: &joinedAt},
	} {
		if err := firstDB.Create(&membership).Error; err != nil {
			t.Fatal(err)
		}
	}
	customerPrincipal := identity.Principal{UserID: customerUser.ID}
	customerTenant, err := tenancy.NewService(firstDB).CreateTenant(ctx, customerPrincipal, tenancy.CreateTenantInput{
		Slug: "pg-support-customer-" + uuid.NewString()[:8], Name: "PG Support Customer", PlanCode: "free", Status: "active",
	}, "pg-support-customer-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	alternateAuthorityTenant, err := tenancy.NewService(firstDB).CreateTenant(ctx, customerPrincipal, tenancy.CreateTenantInput{
		Slug: "pg-support-authority-" + uuid.NewString()[:8], Name: "PG Alternate Authority", PlanCode: "free", Status: "active",
	}, "pg-support-authority-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	customerPrincipal.ActiveTenantID = &customerTenant.ID
	firstService := NewService(firstDB, domain.TenantID)
	if _, err := firstService.UpdatePolicy(ctx, customerPrincipal, customerTenant.ID, UpdatePolicyInput{
		SupportAccessEnabled: true, ExpectedVersion: 0,
		Reason: "Enable PostgreSQL support approval concurrency verification.",
	}, "pg-support-policy", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	supportPrincipal := identity.Principal{UserID: supportUser.ID, ActiveTenantID: &domain.TenantID}
	grant, err := firstService.Request(ctx, supportPrincipal, RequestInput{
		TenantID: customerTenant.ID, Reason: "Investigate a PostgreSQL concurrency incident in read-only mode.",
		RequestedDurationSeconds: 30 * 60,
	}, "pg-support-request", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	secondDB, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	secondSQL, err := secondDB.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondSQL.Close() })
	services := []*Service{firstService, NewService(secondDB, domain.TenantID)}
	principals := []identity.Principal{
		{UserID: domain.UserID, ActiveTenantID: &domain.TenantID},
		{UserID: secondAdmin.ID, ActiveTenantID: &domain.TenantID},
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for index := range services {
		wait.Add(1)
		go func(service *Service, principal identity.Principal) {
			defer wait.Done()
			<-start
			_, approveErr := service.Approve(ctx, principal, grant.ID, DecisionInput{
				ExpectedVersion: 1, Reason: "Approve the bounded read-only support investigation request.",
			}, "pg-support-approve-"+principal.UserID.String(), "127.0.0.1")
			results <- approveErr
		}(services[index], principals[index])
	}
	close(start)
	wait.Wait()
	close(results)
	successes, conflicts := 0, 0
	for approveErr := range results {
		if approveErr == nil {
			successes++
			continue
		}
		var apiError *problem.Error
		if errors.As(approveErr, &apiError) && apiError.Code == "support_access_version_conflict" {
			conflicts++
			continue
		}
		t.Fatalf("unexpected concurrent approval error: %v", approveErr)
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent support approvals: successes=%d conflicts=%d", successes, conflicts)
	}
	var persisted persistence.SupportAccessGrant
	if err := firstDB.Where("id = ?", grant.ID).Take(&persisted).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.Status != "active" || persisted.Version != 2 || persisted.DecidedBy == nil {
		t.Fatalf("unexpected PostgreSQL support grant: %#v", persisted)
	}
	if persisted.OperatorTenantID == nil || *persisted.OperatorTenantID != domain.TenantID {
		t.Fatalf("Support Access grant did not retain immutable Platform authority: %#v", persisted)
	}
	if err := firstDB.Model(&persistence.SupportAccessGrant{}).Where("id = ?", grant.ID).
		Update("operator_tenant_id", alternateAuthorityTenant.ID).Error; err == nil {
		t.Fatal("PostgreSQL allowed Support Access operator authority mutation")
	}
	invalidAuthorityTenantID := alternateAuthorityTenant.ID
	if err := firstDB.Create(&persistence.SupportAccessGrant{
		ID: uuid.New(), TenantID: customerTenant.ID, OperatorTenantID: &invalidAuthorityTenantID,
		RequesterUserID: supportUser.ID, Status: "pending", Version: 1,
		Reason:                   "This direct row lacks membership in the claimed Platform authority.",
		RequestedDurationSeconds: 30 * 60, RequestedAt: now,
	}).Error; err == nil {
		t.Fatal("PostgreSQL allowed a Support Access grant without current Platform authority")
	}
	var activeCount int64
	if err := firstDB.Model(&persistence.SupportAccessGrant{}).
		Where("tenant_id = ? AND requester_user_id = ? AND status = ?", customerTenant.ID, supportUser.ID, "active").
		Count(&activeCount).Error; err != nil {
		t.Fatal(err)
	}
	if activeCount != 1 {
		t.Fatalf("active Support Access grant rows = %d, want 1", activeCount)
	}
	if err := firstDB.Model(&persistence.TenantMembership{}).
		Where("tenant_id = ? AND user_id = ?", domain.TenantID, supportUser.ID).
		Update("status", "suspended").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := authorization.NewAuthorizer(firstDB).RequireTenant(
		ctx, supportUser.ID, customerTenant.ID, authorization.TenantRead,
	); err == nil {
		t.Fatal("PostgreSQL authorization retained Support Access after Platform offboarding")
	}
}
