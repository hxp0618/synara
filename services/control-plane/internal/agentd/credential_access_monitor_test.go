package agentd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

func TestProviderCredentialAccessMonitorResetsDeadlineOnActiveRenewal(t *testing.T) {
	grantID := uuid.New()
	base := time.Now().UTC()
	ctx, cancel := context.WithCancelCause(context.Background())
	updates := make(chan providerCredentialAccessUpdate, 1)
	initial := testProviderCredentialAccess(grantID, providerCredentialAccessStatusActive, 1, 1, base, 80*time.Millisecond)
	done := startProviderCredentialAccessMonitor(
		ctx,
		grantID,
		initial,
		updates,
		cancel,
		time.Now,
	)
	defer cancel(nil)

	time.Sleep(30 * time.Millisecond)
	renewed := testProviderCredentialAccess(grantID, providerCredentialAccessStatusActive, 2, 2, time.Now().UTC(), 220*time.Millisecond)
	renewed.IssuedAt = initial.IssuedAt
	updates <- providerCredentialAccessUpdate{
		access: pointerProviderCredentialAccess(renewed),
	}

	select {
	case err := <-done:
		t.Fatalf("monitor expired before the renewed access deadline: %v", err)
	case <-time.After(110 * time.Millisecond):
	}
	if cause := context.Cause(ctx); cause != nil {
		t.Fatalf("renewed access still canceled the runner: %v", cause)
	}
	cancel(nil)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("monitor returned unexpected shutdown error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("monitor did not stop after natural runner cancellation")
	}
}

func TestProviderCredentialAccessMonitorAllowsRefreshWindowClosedToReopen(t *testing.T) {
	grantID := uuid.New()
	ctx, cancel := context.WithCancelCause(context.Background())
	updates := make(chan providerCredentialAccessUpdate, 2)
	initial := testProviderCredentialAccess(grantID, providerCredentialAccessStatusActive, 3, 4, time.Now().UTC(), 200*time.Millisecond)
	done := startProviderCredentialAccessMonitor(
		ctx,
		grantID,
		initial,
		updates,
		cancel,
		time.Now,
	)
	defer cancel(nil)

	time.Sleep(25 * time.Millisecond)
	closed := testProviderCredentialAccess(grantID, providerCredentialAccessStatusRefreshWindowClosed, 4, 4, time.Now().UTC(), 160*time.Millisecond)
	closed.IssuedAt = initial.IssuedAt
	closed.ActivityAt = initial.ActivityAt
	closed.ExpiresAt = initial.ExpiresAt
	updates <- providerCredentialAccessUpdate{
		access: pointerProviderCredentialAccess(closed),
	}
	time.Sleep(25 * time.Millisecond)
	if cause := context.Cause(ctx); cause != nil {
		t.Fatalf("refresh-window-closed incorrectly canceled the runner: %v", cause)
	}
	reopened := testProviderCredentialAccess(grantID, providerCredentialAccessStatusActive, 5, 5, time.Now().UTC(), 220*time.Millisecond)
	reopened.IssuedAt = initial.IssuedAt
	updates <- providerCredentialAccessUpdate{access: pointerProviderCredentialAccess(reopened)}

	select {
	case err := <-done:
		t.Fatalf("re-opened access still canceled the runner: %v", err)
	case <-time.After(120 * time.Millisecond):
	}
	if cause := context.Cause(ctx); cause != nil {
		t.Fatalf("re-opened access still canceled the runner: %v", cause)
	}
	cancel(nil)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("monitor returned unexpected shutdown error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("monitor did not stop after cancellation")
	}
}

func TestProviderCredentialAccessMonitorRejectsGrantSerialAndActivityRollback(t *testing.T) {
	grantID := uuid.New()
	otherGrantID := uuid.New()
	tests := []struct {
		name   string
		update ProviderCredentialAccess
	}{
		{
			name:   "grant mismatch",
			update: testProviderCredentialAccess(otherGrantID, providerCredentialAccessStatusActive, 2, 2, time.Now().UTC(), 200*time.Millisecond),
		},
		{
			name:   "serial rollback",
			update: testProviderCredentialAccess(grantID, providerCredentialAccessStatusActive, 0, 2, time.Now().UTC(), 200*time.Millisecond),
		},
		{
			name:   "activity rollback",
			update: testProviderCredentialAccess(grantID, providerCredentialAccessStatusActive, 2, 0, time.Now().UTC(), 200*time.Millisecond),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			updates := make(chan providerCredentialAccessUpdate, 1)
			done := startProviderCredentialAccessMonitor(
				ctx,
				grantID,
				testProviderCredentialAccess(grantID, providerCredentialAccessStatusActive, 1, 1, time.Now().UTC(), 200*time.Millisecond),
				updates,
				cancel,
				time.Now,
			)
			updates <- providerCredentialAccessUpdate{access: pointerProviderCredentialAccess(tc.update)}

			select {
			case err := <-done:
				requireRunnerFailureCode(t, err, "credential_invalid")
			case <-time.After(time.Second):
				t.Fatal("monitor did not reject the rollback update")
			}
			requireRunnerFailureCode(t, context.Cause(ctx), "credential_invalid")
		})
	}
}

func TestProviderCredentialAccessMonitorCancelsOnExpiry(t *testing.T) {
	grantID := uuid.New()
	ctx, cancel := context.WithCancelCause(context.Background())
	done := startProviderCredentialAccessMonitor(
		ctx,
		grantID,
		testProviderCredentialAccess(grantID, providerCredentialAccessStatusActive, 7, 9, time.Now().UTC(), 40*time.Millisecond),
		make(chan providerCredentialAccessUpdate),
		cancel,
		time.Now,
	)
	select {
	case err := <-done:
		requireRunnerFailureCode(t, err, "credential_expired")
	case <-time.After(time.Second):
		t.Fatal("monitor did not cancel the runner after local access expiry")
	}
	requireRunnerFailureCode(t, context.Cause(ctx), "credential_expired")
}

func TestProviderCredentialAccessMonitorFailsWhenRenewalStreamCloses(t *testing.T) {
	grantID := uuid.New()
	ctx, cancel := context.WithCancelCause(context.Background())
	updates := make(chan providerCredentialAccessUpdate)
	done := startProviderCredentialAccessMonitor(
		ctx,
		grantID,
		testProviderCredentialAccess(grantID, providerCredentialAccessStatusActive, 10, 12, time.Now().UTC(), 200*time.Millisecond),
		updates,
		cancel,
		time.Now,
	)
	close(updates)

	select {
	case err := <-done:
		requireRunnerFailureCode(t, err, "credential_unavailable")
	case <-time.After(time.Second):
		t.Fatal("monitor did not fail after the renewal stream closed")
	}
	requireRunnerFailureCode(t, context.Cause(ctx), "credential_unavailable")
}

func TestProviderCredentialAccessLeaseBridgeRejectsMissingAccessMetadata(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	leaseUpdates := make(chan executions.Lease, 1)
	updates := startProviderCredentialAccessLeaseBridge(ctx, leaseUpdates)
	leaseUpdates <- executions.Lease{ExecutionID: uuid.New(), Generation: 1}

	select {
	case update := <-updates:
		requireRunnerFailureCode(t, update.err, "credential_invalid")
		if update.access != nil {
			t.Fatalf("missing access metadata produced an access update: %#v", update.access)
		}
	case <-time.After(time.Second):
		t.Fatal("access bridge did not reject missing renewal metadata")
	}
}

func testProviderCredentialAccess(
	grantID uuid.UUID,
	status string,
	serial, activitySequence int64,
	base time.Time,
	expiresAfter time.Duration,
) ProviderCredentialAccess {
	issuedAt := base.Add(-time.Minute)
	activityAt := base
	renewedAt := base.Add(5 * time.Millisecond)
	refreshDeadlineAt := base.Add(expiresAfter / 2)
	renewAfterAt := base.Add(expiresAfter / 4)
	expiresAt := base.Add(expiresAfter)
	return ProviderCredentialAccess{
		GrantID:           grantID,
		Serial:            serial,
		Status:            status,
		ActivitySequence:  activitySequence,
		ActivityAt:        activityAt,
		IssuedAt:          issuedAt,
		RenewedAt:         renewedAt,
		ExpiresAt:         expiresAt,
		RefreshDeadlineAt: refreshDeadlineAt,
		RenewAfterAt:      &renewAfterAt,
	}
}

func pointerProviderCredentialAccess(value ProviderCredentialAccess) *ProviderCredentialAccess {
	return &value
}

func requireRunnerFailureCode(t *testing.T, err error, code string) {
	t.Helper()
	var failure *runnerFailure
	if !errors.As(err, &failure) || failure.code != code {
		t.Fatalf("runner failure = %T %[1]v, want code %q", err, code)
	}
}
