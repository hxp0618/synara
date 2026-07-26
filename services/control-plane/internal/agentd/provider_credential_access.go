package agentd

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

const (
	providerCredentialAccessStatusActive              = "active"
	providerCredentialAccessStatusRefreshWindowClosed = "refresh-window-closed"
	providerCredentialAccessStatusUnavailable         = "credential-unavailable"
	providerCredentialAccessStatusExpired             = "expired"
)

type providerCredentialAccessUpdate struct {
	access *ProviderCredentialAccess
	err    error
}

func startProviderCredentialAccessMonitor(
	ctx context.Context,
	expectedGrantID uuid.UUID,
	initial ProviderCredentialAccess,
	updates <-chan providerCredentialAccessUpdate,
	cancelRunner context.CancelCauseFunc,
	now func() time.Time,
) <-chan error {
	done := make(chan error, 1)
	go func() {
		defer close(done)
		current := initial
		timer := time.NewTimer(providerCredentialAccessDurationUntil(now, current.ExpiresAt))
		defer timer.Stop()
		report := func(err error) {
			select {
			case done <- err:
			default:
			}
		}
		for {
			select {
			case <-ctx.Done():
				report(nil)
				return
			case <-timer.C:
				err := providerCredentialAccessExpiredFailure()
				cancelRunner(err)
				report(err)
				return
			case update, ok := <-updates:
				if !ok {
					if ctx.Err() != nil {
						report(nil)
						return
					}
					err := providerCredentialAccessUnavailableFailureWithMessage(
						"The Provider Credential access lease renewal stream closed during execution.",
					)
					cancelRunner(err)
					report(err)
					return
				}
				if update.err != nil {
					cancelRunner(update.err)
					report(update.err)
					return
				}
				if update.access == nil {
					err := providerCredentialAccessInvalidFailure(
						"The Provider Credential access lease metadata disappeared during execution.",
					)
					cancelRunner(err)
					report(err)
					return
				}
				if err := validateUpdatedProviderCredentialAccess(expectedGrantID, current, *update.access, now()); err != nil {
					cancelRunner(err)
					report(err)
					return
				}
				current = *update.access
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(providerCredentialAccessDurationUntil(now, current.ExpiresAt))
			}
		}
	}()
	return done
}

func startProviderCredentialAccessLeaseBridge(
	ctx context.Context,
	leaseUpdates <-chan executions.Lease,
) <-chan providerCredentialAccessUpdate {
	updates := make(chan providerCredentialAccessUpdate, 1)
	go func() {
		defer close(updates)
		for {
			select {
			case <-ctx.Done():
				return
			case renewed, ok := <-leaseUpdates:
				if !ok {
					return
				}
				access, err := providerCredentialAccessFromLease(renewed)
				if err != nil {
					publishLatestProviderCredentialAccessUpdate(updates, providerCredentialAccessUpdate{
						err: providerCredentialAccessInvalidFailure(
							"The Provider Credential access lease metadata in the renewal response is invalid.",
						),
					})
					return
				}
				publishLatestProviderCredentialAccessUpdate(updates, providerCredentialAccessUpdate{access: access})
			}
		}
	}()
	return updates
}

func providerCredentialAccessDurationUntil(now func() time.Time, deadline time.Time) time.Duration {
	duration := deadline.Sub(now())
	if duration <= 0 {
		return time.Nanosecond
	}
	return duration
}

func publishLatestProviderCredentialAccessUpdate(
	updates chan providerCredentialAccessUpdate,
	update providerCredentialAccessUpdate,
) {
	select {
	case updates <- update:
	default:
		select {
		case <-updates:
		default:
		}
		select {
		case updates <- update:
		default:
		}
	}
}

func validateInitialProviderCredentialAccess(
	expectedGrantID uuid.UUID,
	access *ProviderCredentialAccess,
	now time.Time,
) error {
	if access == nil {
		return providerCredentialAccessInvalidFailure("The Provider Credential access lease metadata is missing.")
	}
	if err := validateProviderCredentialAccessShape(expectedGrantID, *access, now); err != nil {
		return err
	}
	if strings.TrimSpace(access.Status) != providerCredentialAccessStatusActive {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease must start in an active state.",
		)
	}
	return nil
}

func validateUpdatedProviderCredentialAccess(
	expectedGrantID uuid.UUID,
	current ProviderCredentialAccess,
	next ProviderCredentialAccess,
	now time.Time,
) error {
	if err := validateProviderCredentialAccessShape(expectedGrantID, next, now); err != nil {
		return err
	}
	switch strings.TrimSpace(next.Status) {
	case providerCredentialAccessStatusUnavailable:
		return providerCredentialAccessUnavailableFailure()
	case providerCredentialAccessStatusExpired:
		return providerCredentialAccessExpiredFailure()
	}
	if next.Serial < current.Serial {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease serial rolled back during execution.",
		)
	}
	if !next.IssuedAt.Equal(current.IssuedAt) {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease issuance timestamp changed during execution.",
		)
	}
	if next.ActivitySequence < current.ActivitySequence {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease activity sequence rolled back during execution.",
		)
	}
	if next.ActivitySequence == current.ActivitySequence && !next.ActivityAt.Equal(current.ActivityAt) {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease activity timestamp changed without a newer semantic event.",
		)
	}
	if next.ActivitySequence > current.ActivitySequence && next.ActivityAt.Before(current.ActivityAt) {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease activity timestamp rolled back during execution.",
		)
	}
	if next.RenewedAt.Before(current.RenewedAt) || next.ExpiresAt.Before(current.ExpiresAt) {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease renewal window rolled back during execution.",
		)
	}
	if providerCredentialHardExpiryExtended(current.HardExpiresAt, next.HardExpiresAt) {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease hard expiry was extended during execution.",
		)
	}
	if next.Serial == current.Serial &&
		(next.ActivitySequence != current.ActivitySequence ||
			!next.ActivityAt.Equal(current.ActivityAt) ||
			!next.RenewedAt.Equal(current.RenewedAt) ||
			!next.ExpiresAt.Equal(current.ExpiresAt) ||
			!next.RefreshDeadlineAt.Equal(current.RefreshDeadlineAt) ||
			!optionalTimesEqual(next.HardExpiresAt, current.HardExpiresAt)) {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease changed durable state without advancing its serial.",
		)
	}
	return nil
}

func providerCredentialHardExpiryExtended(current, next *time.Time) bool {
	if current == nil {
		return false
	}
	return next == nil || next.After(*current)
}

func optionalTimesEqual(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func validateProviderCredentialAccessShape(
	expectedGrantID uuid.UUID,
	access ProviderCredentialAccess,
	now time.Time,
) error {
	status := strings.TrimSpace(access.Status)
	if expectedGrantID == uuid.Nil || access.GrantID == uuid.Nil || access.GrantID != expectedGrantID {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease no longer matches the expected grant.",
		)
	}
	switch status {
	case providerCredentialAccessStatusActive, providerCredentialAccessStatusRefreshWindowClosed:
	case providerCredentialAccessStatusUnavailable:
		return providerCredentialAccessUnavailableFailure()
	case providerCredentialAccessStatusExpired:
		return providerCredentialAccessExpiredFailure()
	default:
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease status is invalid.",
		)
	}
	if access.Serial <= 0 || access.ActivitySequence < 0 {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease sequence metadata is invalid.",
		)
	}
	if access.IssuedAt.IsZero() || access.RenewedAt.IsZero() || access.ExpiresAt.IsZero() ||
		access.ActivityAt.IsZero() || access.RefreshDeadlineAt.IsZero() {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease deadlines are incomplete.",
		)
	}
	if access.RefreshDeadlineAt.Before(access.ActivityAt) {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease refresh deadline is invalid.",
		)
	}
	if access.RenewedAt.Before(access.IssuedAt) {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease renewal timestamp is invalid.",
		)
	}
	if !access.ExpiresAt.After(access.RenewedAt) || !access.ExpiresAt.After(now) {
		return providerCredentialAccessExpiredFailure()
	}
	if access.RenewAfterAt == nil || access.RenewAfterAt.IsZero() {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease renewAfter deadline is invalid.",
		)
	}
	if access.RenewAfterAt.Before(access.RenewedAt) || !access.RenewAfterAt.Before(access.ExpiresAt) {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease renewAfter deadline is invalid.",
		)
	}
	if access.HardExpiresAt != nil && access.HardExpiresAt.Before(access.ExpiresAt) {
		return providerCredentialAccessInvalidFailure(
			"The Provider Credential access lease hard expiry is invalid.",
		)
	}
	return nil
}

func providerCredentialAccessInvalidFailure(message string) error {
	return &runnerFailure{
		code: "credential_invalid", message: message,
		requiresNewExecution: true, requiresUserAction: true,
		canReconstructFromHistory: true, canMoveWorker: true,
	}
}

func providerCredentialAccessUnavailableFailure() error {
	return providerCredentialAccessUnavailableFailureWithMessage(
		"The Provider Credential access lease became unavailable during execution.",
	)
}

func providerCredentialAccessUnavailableFailureWithMessage(message string) error {
	return &runnerFailure{
		code:                 "credential_unavailable",
		message:              message,
		requiresNewExecution: true, requiresUserAction: true,
		canReconstructFromHistory: true, canMoveWorker: true,
	}
}

func providerCredentialAccessExpiredFailure() error {
	return &runnerFailure{
		code:                 "credential_expired",
		message:              "The Provider Credential access lease expired during execution.",
		requiresNewExecution: true, requiresUserAction: true,
		canReconstructFromHistory: true, canMoveWorker: true,
	}
}

func providerCredentialAccessFromLease(lease executions.Lease) (*ProviderCredentialAccess, error) {
	if lease.ProviderCredentialAccess == nil {
		return nil, errors.New("Provider Credential access metadata is missing from the renewed Lease")
	}
	return lease.ProviderCredentialAccess, nil
}
