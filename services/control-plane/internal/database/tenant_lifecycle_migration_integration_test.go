package database

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/tenancy"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresTenantLifecycleVersionSerializesConcurrentTransitions(t *testing.T) {
	ctx := context.Background()
	db := openPostgresIntegrationDB(t)
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "postgres-tenant-lifecycle-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	service := tenancy.NewService(db)
	tenant, err := service.CreateTenant(ctx, principal, tenancy.CreateTenantInput{
		Slug: "pg-lifecycle-" + uuid.NewString()[:8], Name: "PostgreSQL lifecycle", Status: "active",
	}, "pg-tenant-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errorsByTransition := make(chan error, 2)
	var wait sync.WaitGroup
	for _, target := range []string{"suspended", "closed"} {
		wait.Add(1)
		go func(toStatus string) {
			defer wait.Done()
			<-start
			_, transitionErr := service.TransitionTenant(ctx, principal, tenant.ID, tenancy.TransitionTenantInput{
				ToStatus: toStatus, ExpectedVersion: tenant.LifecycleVersion, Reason: "concurrent lifecycle test",
			}, "pg-tenant-transition-"+toStatus, "127.0.0.1")
			errorsByTransition <- transitionErr
		}(target)
	}
	close(start)
	wait.Wait()
	close(errorsByTransition)

	successes, conflicts := 0, 0
	for transitionErr := range errorsByTransition {
		if transitionErr == nil {
			successes++
			continue
		}
		var apiError *problem.Error
		if errors.As(transitionErr, &apiError) && apiError.Code == "tenant_lifecycle_version_conflict" {
			conflicts++
			continue
		}
		t.Fatalf("unexpected concurrent Tenant lifecycle error: %v", transitionErr)
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent Tenant lifecycle results: successes=%d conflicts=%d", successes, conflicts)
	}
	if err := db.Exec("UPDATE tenants SET lifecycle_version = 0 WHERE id = ?", tenant.ID).Error; err == nil {
		t.Fatal("PostgreSQL accepted a non-positive Tenant lifecycle version")
	}
	if err := db.Exec(`UPDATE tenants
		SET status = 'trialing', trial_expires_at = NULL, suspended_at = NULL, closed_at = NULL
		WHERE id = ?`, tenant.ID).Error; err == nil {
		t.Fatal("PostgreSQL accepted a trialing Tenant without an expiry")
	}
}
