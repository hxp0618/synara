package executions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestRenewProviderCredentialAccessLockedSkipsLeaseWithoutGrant(t *testing.T) {
	service := &Service{}

	access, updates, err := service.renewProviderCredentialAccessLocked(
		context.Background(),
		nil,
		persistence.AgentExecution{},
		persistence.WorkerLease{},
		time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if access != nil || len(updates) != 0 {
		t.Fatalf("renew without an attached Provider Credential Grant returned %#v / %#v", access, updates)
	}
}

func TestProviderCredentialAccessProjectionClampsRenewalClockRollback(t *testing.T) {
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	activityAt := now.Add(-time.Minute)
	issuedAt := now.Add(-10 * time.Minute)
	currentRenewedAt := now.Add(45 * time.Second)
	currentExpiresAt := currentRenewedAt.Add(5 * time.Minute)
	refreshDeadlineAt := activityAt.Add(15 * time.Minute)
	grantID := uuid.New()
	service := &Service{
		leaseTTL:                    30 * time.Second,
		providerCredentialAccessTTL: 5 * time.Minute,
	}

	projection := service.computeProviderCredentialAccessProjection(
		now,
		persistence.ExecutionProviderCredentialGrant{ID: grantID},
		providerCredentialAccessSessionSnapshot{
			ResourceState:              "waiting",
			MeaningfulActivitySequence: 11,
			MeaningfulActivityAt:       activityAt,
			WaitingKeepAliveSeconds:    900,
		},
		persistence.WorkerLease{
			ProviderCredentialGrantID:           &grantID,
			ProviderCredentialAccessSerial:      int64AccessPtr(7),
			ProviderCredentialActivitySequence:  int64AccessPtr(11),
			ProviderCredentialActivityAt:        timeAccessPtr(activityAt),
			ProviderCredentialAccessIssuedAt:    timeAccessPtr(issuedAt),
			ProviderCredentialAccessRenewedAt:   timeAccessPtr(currentRenewedAt),
			ProviderCredentialAccessExpiresAt:   timeAccessPtr(currentExpiresAt),
			ProviderCredentialRefreshDeadlineAt: timeAccessPtr(refreshDeadlineAt),
		},
		&persistence.ProviderCredential{},
		false,
		false,
	)

	if projection.Access.Status != ProviderCredentialAccessStatusActive {
		t.Fatalf("status = %q, want active", projection.Access.Status)
	}
	if !projection.Access.RenewedAt.Equal(currentRenewedAt) {
		t.Fatalf("renewedAt = %s, want %s", projection.Access.RenewedAt, currentRenewedAt)
	}
	if !projection.Access.ExpiresAt.Equal(currentExpiresAt) {
		t.Fatalf("expiresAt = %s, want %s", projection.Access.ExpiresAt, currentExpiresAt)
	}
	if projection.Access.RenewAfterAt == nil {
		t.Fatal("renewAfterAt was not populated for active access")
	}
	if projection.Access.RenewAfterAt.Before(projection.Access.RenewedAt) ||
		!projection.Access.RenewAfterAt.Before(projection.Access.ExpiresAt) {
		t.Fatalf("renewAfterAt = %s, renewedAt = %s, expiresAt = %s", *projection.Access.RenewAfterAt, projection.Access.RenewedAt, projection.Access.ExpiresAt)
	}
	if len(projection.Updates) != 0 {
		t.Fatalf("clock rollback should not persist a lease update: %#v", projection.Updates)
	}
}

func TestProviderCredentialAccessProjectionClampsActivityAtForNewerSequence(t *testing.T) {
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	currentActivityAt := now.Add(-time.Minute)
	currentRenewedAt := now.Add(-30 * time.Second)
	currentExpiresAt := now.Add(4 * time.Minute)
	refreshDeadlineAt := now.Add(9 * time.Minute)
	grantID := uuid.New()
	service := &Service{
		leaseTTL:                    30 * time.Second,
		providerCredentialAccessTTL: 5 * time.Minute,
	}

	projection := service.computeProviderCredentialAccessProjection(
		now,
		persistence.ExecutionProviderCredentialGrant{ID: grantID},
		providerCredentialAccessSessionSnapshot{
			ResourceState:              "waiting",
			MeaningfulActivitySequence: 12,
			MeaningfulActivityAt:       now.Add(-2 * time.Minute),
			WaitingKeepAliveSeconds:    900,
		},
		persistence.WorkerLease{
			ProviderCredentialGrantID:           &grantID,
			ProviderCredentialAccessSerial:      int64AccessPtr(4),
			ProviderCredentialActivitySequence:  int64AccessPtr(11),
			ProviderCredentialActivityAt:        timeAccessPtr(currentActivityAt),
			ProviderCredentialAccessIssuedAt:    timeAccessPtr(now.Add(-10 * time.Minute)),
			ProviderCredentialAccessRenewedAt:   timeAccessPtr(currentRenewedAt),
			ProviderCredentialAccessExpiresAt:   timeAccessPtr(currentExpiresAt),
			ProviderCredentialRefreshDeadlineAt: timeAccessPtr(refreshDeadlineAt),
		},
		&persistence.ProviderCredential{},
		false,
		false,
	)

	if projection.Access.ActivitySequence != 12 {
		t.Fatalf("activitySequence = %d, want 12", projection.Access.ActivitySequence)
	}
	if !projection.Access.ActivityAt.Equal(currentActivityAt) {
		t.Fatalf("activityAt = %s, want clamped %s", projection.Access.ActivityAt, currentActivityAt)
	}
	if projection.Access.Serial != 5 {
		t.Fatalf("serial = %d, want 5", projection.Access.Serial)
	}
	updateActivityAt, ok := projection.Updates["provider_credential_activity_at"].(time.Time)
	if !ok || !updateActivityAt.Equal(currentActivityAt) {
		t.Fatalf("persisted activity_at = %#v, want %s", projection.Updates["provider_credential_activity_at"], currentActivityAt)
	}
	updateSequence, ok := projection.Updates["provider_credential_activity_sequence"].(int64)
	if !ok || updateSequence != 12 {
		t.Fatalf("persisted activity_sequence = %#v, want 12", projection.Updates["provider_credential_activity_sequence"])
	}
}

func TestProviderCredentialAccessProjectionPreservesExpiryOnTTLContraction(t *testing.T) {
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	currentActivityAt := now.Add(-time.Minute)
	currentRenewedAt := now.Add(-time.Minute)
	currentExpiresAt := now.Add(4 * time.Minute)
	refreshDeadlineAt := now.Add(9 * time.Minute)
	grantID := uuid.New()
	service := &Service{
		leaseTTL:                    30 * time.Second,
		providerCredentialAccessTTL: 2 * time.Minute,
	}

	projection := service.computeProviderCredentialAccessProjection(
		now,
		persistence.ExecutionProviderCredentialGrant{ID: grantID},
		providerCredentialAccessSessionSnapshot{
			ResourceState:              "waiting",
			MeaningfulActivitySequence: 11,
			MeaningfulActivityAt:       currentActivityAt,
			WaitingKeepAliveSeconds:    900,
		},
		persistence.WorkerLease{
			ProviderCredentialGrantID:           &grantID,
			ProviderCredentialAccessSerial:      int64AccessPtr(4),
			ProviderCredentialActivitySequence:  int64AccessPtr(11),
			ProviderCredentialActivityAt:        timeAccessPtr(currentActivityAt),
			ProviderCredentialAccessIssuedAt:    timeAccessPtr(now.Add(-10 * time.Minute)),
			ProviderCredentialAccessRenewedAt:   timeAccessPtr(currentRenewedAt),
			ProviderCredentialAccessExpiresAt:   timeAccessPtr(currentExpiresAt),
			ProviderCredentialRefreshDeadlineAt: timeAccessPtr(refreshDeadlineAt),
		},
		&persistence.ProviderCredential{},
		false,
		false,
	)

	if !projection.Access.ExpiresAt.Equal(currentExpiresAt) {
		t.Fatalf("expiresAt = %s, want preserved %s", projection.Access.ExpiresAt, currentExpiresAt)
	}
	if projection.Access.Serial != 5 {
		t.Fatalf("serial = %d, want 5", projection.Access.Serial)
	}
	updateExpiresAt, ok := projection.Updates["provider_credential_access_expires_at"].(time.Time)
	if !ok || !updateExpiresAt.Equal(currentExpiresAt) {
		t.Fatalf("persisted expires_at = %#v, want %s", projection.Updates["provider_credential_access_expires_at"], currentExpiresAt)
	}
}

func TestProviderCredentialAccessProjectionReturnsExpiredOnElapsedHardCapWithoutUpdates(t *testing.T) {
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	currentActivityAt := now.Add(-time.Minute)
	currentRenewedAt := now.Add(-30 * time.Second)
	currentExpiresAt := now.Add(4 * time.Minute)
	refreshDeadlineAt := now.Add(9 * time.Minute)
	absoluteExpiresAt := now.Add(-time.Second)
	grantID := uuid.New()
	service := &Service{
		leaseTTL:                    30 * time.Second,
		providerCredentialAccessTTL: 5 * time.Minute,
	}

	projection := service.computeProviderCredentialAccessProjection(
		now,
		persistence.ExecutionProviderCredentialGrant{ID: grantID},
		providerCredentialAccessSessionSnapshot{
			ResourceState:              "waiting",
			MeaningfulActivitySequence: 11,
			MeaningfulActivityAt:       currentActivityAt,
			WaitingKeepAliveSeconds:    900,
			AbsoluteExpiresAt:          &absoluteExpiresAt,
		},
		persistence.WorkerLease{
			ProviderCredentialGrantID:           &grantID,
			ProviderCredentialAccessSerial:      int64AccessPtr(6),
			ProviderCredentialActivitySequence:  int64AccessPtr(11),
			ProviderCredentialActivityAt:        timeAccessPtr(currentActivityAt),
			ProviderCredentialAccessIssuedAt:    timeAccessPtr(now.Add(-10 * time.Minute)),
			ProviderCredentialAccessRenewedAt:   timeAccessPtr(currentRenewedAt),
			ProviderCredentialAccessExpiresAt:   timeAccessPtr(currentExpiresAt),
			ProviderCredentialRefreshDeadlineAt: timeAccessPtr(refreshDeadlineAt),
		},
		&persistence.ProviderCredential{},
		false,
		false,
	)

	if projection.Access.Status != ProviderCredentialAccessStatusExpired {
		t.Fatalf("status = %q, want expired", projection.Access.Status)
	}
	if !projection.Access.ExpiresAt.Equal(absoluteExpiresAt) {
		t.Fatalf("expiresAt = %s, want hard cap %s", projection.Access.ExpiresAt, absoluteExpiresAt)
	}
	if projection.Access.HardExpiresAt == nil || !projection.Access.HardExpiresAt.Equal(absoluteExpiresAt) {
		t.Fatalf("hardExpiresAt = %#v, want %s", projection.Access.HardExpiresAt, absoluteExpiresAt)
	}
	if len(projection.Updates) != 0 {
		t.Fatalf("elapsed hard cap should not persist a backward lease update: %#v", projection.Updates)
	}
}

func TestProviderCredentialAccessProjectionRefreshWindowClosesAndSemanticActivityReopensIt(t *testing.T) {
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	activityAt := now.Add(-16 * time.Minute)
	refreshDeadlineAt := activityAt.Add(15 * time.Minute)
	currentExpiresAt := now.Add(2 * time.Minute)
	grantID := uuid.New()
	service := &Service{
		leaseTTL:                    30 * time.Second,
		providerCredentialAccessTTL: 5 * time.Minute,
	}
	lease := persistence.WorkerLease{
		ProviderCredentialGrantID:           &grantID,
		ProviderCredentialAccessSerial:      int64AccessPtr(8),
		ProviderCredentialActivitySequence:  int64AccessPtr(11),
		ProviderCredentialActivityAt:        timeAccessPtr(activityAt),
		ProviderCredentialAccessIssuedAt:    timeAccessPtr(now.Add(-20 * time.Minute)),
		ProviderCredentialAccessRenewedAt:   timeAccessPtr(now.Add(-2 * time.Minute)),
		ProviderCredentialAccessExpiresAt:   timeAccessPtr(currentExpiresAt),
		ProviderCredentialRefreshDeadlineAt: timeAccessPtr(refreshDeadlineAt),
	}
	credential := &persistence.ProviderCredential{}

	closed := service.computeProviderCredentialAccessProjection(
		now,
		persistence.ExecutionProviderCredentialGrant{ID: grantID},
		providerCredentialAccessSessionSnapshot{
			ResourceState: "waiting", MeaningfulActivitySequence: 11,
			MeaningfulActivityAt: activityAt, WaitingKeepAliveSeconds: 900,
		},
		lease,
		credential,
		false,
		false,
	)
	if closed.Access.Status != ProviderCredentialAccessStatusRefreshWindowClosed ||
		closed.Access.Serial != 8 || !closed.Access.ExpiresAt.Equal(currentExpiresAt) ||
		len(closed.Updates) != 0 {
		t.Fatalf("closed refresh projection = %#v, updates=%#v", closed.Access, closed.Updates)
	}

	reopened := service.computeProviderCredentialAccessProjection(
		now,
		persistence.ExecutionProviderCredentialGrant{ID: grantID},
		providerCredentialAccessSessionSnapshot{
			ResourceState: "waiting", MeaningfulActivitySequence: 12,
			MeaningfulActivityAt: now, WaitingKeepAliveSeconds: 900,
		},
		lease,
		credential,
		false,
		false,
	)
	if reopened.Access.Status != ProviderCredentialAccessStatusActive ||
		reopened.Access.Serial != 9 || reopened.Access.ActivitySequence != 12 ||
		!reopened.Access.ExpiresAt.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("reopened refresh projection = %#v", reopened.Access)
	}
	if reopened.Updates["provider_credential_access_serial"] != int64(9) ||
		reopened.Updates["provider_credential_activity_sequence"] != int64(12) {
		t.Fatalf("reopened durable updates = %#v", reopened.Updates)
	}
}

func TestProviderCredentialHardExpiryUsesEarliestBoundWithoutExtendingFrozenCap(t *testing.T) {
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	frozenHardCap := now.Add(5 * time.Minute)
	sessionLater := now.Add(10 * time.Minute)
	sessionEarlier := now.Add(3 * time.Minute)
	credentialLater := now.Add(15 * time.Minute)
	credentialEarlier := now.Add(2 * time.Minute)

	got := providerCredentialHardExpiry(
		&sessionLater,
		&frozenHardCap,
		&persistence.ProviderCredential{ExpiresAt: timeAccessPtr(credentialLater)},
	)
	if got == nil || !got.Equal(frozenHardCap) {
		t.Fatalf("hard expiry extended the frozen cap: %#v", got)
	}

	got = providerCredentialHardExpiry(
		&sessionEarlier,
		&frozenHardCap,
		&persistence.ProviderCredential{ExpiresAt: timeAccessPtr(credentialLater)},
	)
	if got == nil || !got.Equal(sessionEarlier) {
		t.Fatalf("hard expiry did not clamp to the earlier session bound: %#v", got)
	}

	got = providerCredentialHardExpiry(
		&sessionLater,
		&frozenHardCap,
		&persistence.ProviderCredential{ExpiresAt: timeAccessPtr(credentialEarlier)},
	)
	if got == nil || !got.Equal(credentialEarlier) {
		t.Fatalf("hard expiry did not clamp to the earlier credential bound: %#v", got)
	}
}

func int64AccessPtr(value int64) *int64 {
	return &value
}

func timeAccessPtr(value time.Time) *time.Time {
	return &value
}
