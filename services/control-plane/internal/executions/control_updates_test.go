package executions

import (
	"context"
	"testing"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
)

// The combined pull must return exactly what the two separate pulls returned,
// from a single lease-verified snapshot.
func TestPullControlUpdatesReturnsBothDeliveryKinds(t *testing.T) {
	ctx := context.Background()
	_, service, fixture := setupSQLiteRecoveryService(t)

	_, worker := registerWorkerModeTestWorker(
		t, service, fixture.TargetID, fixture.TargetKind, "control-updates", WorkerModeGeneralPool,
	)
	claim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID,
		TargetKind:        fixture.TargetKind,
		ExecutionID:       &fixture.ExecutionID,
	}, "control-updates-claim")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Value.Lease == nil {
		t.Fatal("claim lease is nil")
	}
	lease := LeaseInput{
		TenantID:   fixture.TenantID,
		Generation: claim.Value.Lease.Generation,
		LeaseToken: claim.Value.Lease.LeaseToken,
	}
	if _, err := service.Start(ctx, worker, fixture.ExecutionID, lease, "control-updates-start"); err != nil {
		t.Fatal(err)
	}

	// An empty Execution yields empty, non-nil sets rather than an error.
	empty, err := service.PullControlUpdates(ctx, worker, fixture.ExecutionID, PullControlUpdatesInput{
		LeaseInput: lease,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.ControlCommands) != 0 || len(empty.InteractionResolutions) != 0 {
		t.Fatalf("idle pull returned deliveries: %#v", empty)
	}

	principal := identity.Principal{UserID: fixture.UserID, ActiveTenantID: &fixture.TenantID}
	requested, err := service.RequestInterrupt(
		ctx, principal, fixture.SessionID, "control-updates-interrupt", "control-updates-interrupt", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}

	updates, err := service.PullControlUpdates(ctx, worker, fixture.ExecutionID, PullControlUpdatesInput{
		LeaseInput: lease,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(updates.ControlCommands) != 1 || updates.ControlCommands[0].CommandID != requested.Value.CommandID {
		t.Fatalf("combined pull did not return the Control command: %#v", updates.ControlCommands)
	}

	// The combined result must agree with the endpoints it replaces.
	commands, err := service.PullControlCommands(ctx, worker, fixture.ExecutionID, PullControlCommandsInput{
		LeaseInput: lease,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != len(updates.ControlCommands) ||
		commands[0].CommandID != updates.ControlCommands[0].CommandID {
		t.Fatalf("combined pull disagrees with PullControlCommands: %#v vs %#v", updates.ControlCommands, commands)
	}
	resolutions, err := service.PullInteractionResolutions(ctx, worker, fixture.ExecutionID,
		PullInteractionResolutionsInput{LeaseInput: lease})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolutions) != len(updates.InteractionResolutions) {
		t.Fatalf("combined pull disagrees with PullInteractionResolutions: %#v vs %#v",
			updates.InteractionResolutions, resolutions)
	}
}

// The combined pull is lease-scoped exactly like the endpoints it replaces:
// a superseded generation must not read another generation's deliveries.
func TestPullControlUpdatesFencesStaleGeneration(t *testing.T) {
	ctx := context.Background()
	_, service, fixture := setupSQLiteRecoveryService(t)

	_, worker := registerWorkerModeTestWorker(
		t, service, fixture.TargetID, fixture.TargetKind, "control-updates-fence", WorkerModeGeneralPool,
	)
	claim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID,
		TargetKind:        fixture.TargetKind,
		ExecutionID:       &fixture.ExecutionID,
	}, "control-updates-fence-claim")
	if err != nil {
		t.Fatal(err)
	}
	lease := LeaseInput{
		TenantID:   fixture.TenantID,
		Generation: claim.Value.Lease.Generation,
		LeaseToken: claim.Value.Lease.LeaseToken,
	}

	stale := lease
	stale.Generation = lease.Generation + 1
	if _, err := service.PullControlUpdates(ctx, worker, fixture.ExecutionID, PullControlUpdatesInput{
		LeaseInput: stale,
	}); err == nil {
		t.Fatal("combined pull accepted a superseded generation")
	}

	invalidToken := lease
	invalidToken.LeaseToken = "not-the-lease-token"
	if _, err := service.PullControlUpdates(ctx, worker, fixture.ExecutionID, PullControlUpdatesInput{
		LeaseInput: invalidToken,
	}); err == nil {
		t.Fatal("combined pull accepted an invalid lease token")
	}
}

func TestPullControlUpdatesRejectsOutOfRangeLimits(t *testing.T) {
	ctx := context.Background()
	_, service, fixture := setupSQLiteRecoveryService(t)

	_, worker := registerWorkerModeTestWorker(
		t, service, fixture.TargetID, fixture.TargetKind, "control-updates-limit", WorkerModeGeneralPool,
	)
	claim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID,
		TargetKind:        fixture.TargetKind,
		ExecutionID:       &fixture.ExecutionID,
	}, "control-updates-limit-claim")
	if err != nil {
		t.Fatal(err)
	}
	lease := LeaseInput{
		TenantID:   fixture.TenantID,
		Generation: claim.Value.Lease.Generation,
		LeaseToken: claim.Value.Lease.LeaseToken,
	}
	for _, input := range []PullControlUpdatesInput{
		{LeaseInput: lease, ControlCommandLimit: 101},
		{LeaseInput: lease, ControlCommandLimit: -1},
		{LeaseInput: lease, InteractionResolutionLimit: 101},
		{LeaseInput: lease, InteractionResolutionLimit: -1},
	} {
		if _, err := service.PullControlUpdates(ctx, worker, fixture.ExecutionID, input); err == nil {
			t.Fatalf("combined pull accepted an out-of-range limit: %#v", input)
		}
	}
}
