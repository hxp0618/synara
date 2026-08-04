package executiontargets

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

type ProvisioningOperationExecutor interface {
	Execute(context.Context, ProvisioningClaim) (SSHProvisionResult, error)
}

type ProvisioningReconciler struct {
	targets  *Service
	executor ProvisioningOperationExecutor
	holder   string
	lease    time.Duration
}

func NewProvisioningReconciler(
	targets *Service, executor ProvisioningOperationExecutor, holder string, lease time.Duration,
) (*ProvisioningReconciler, error) {
	holder = strings.TrimSpace(holder)
	if targets == nil || executor == nil || holder == "" || lease < 3*time.Second {
		return nil, errors.New("provisioning reconciler requires a service, executor, holder, and lease of at least three seconds")
	}
	return &ProvisioningReconciler{targets: targets, executor: executor, holder: holder, lease: lease}, nil
}

func (r *ProvisioningReconciler) ReconcileOnce(ctx context.Context) (bool, error) {
	claim, found, err := r.targets.ClaimNextProvisioningOperation(ctx, r.holder, r.lease)
	if err != nil || !found {
		return found, err
	}
	executionContext, cancel := context.WithCancel(ctx)
	defer cancel()
	renewalErrors := make(chan error, 1)
	done := make(chan struct{})
	renewed := make(chan struct{})
	go r.renewClaim(executionContext, cancel, claim, done, renewed, renewalErrors)
	result, executionErr := r.executor.Execute(executionContext, claim)
	close(done)
	<-renewed
	select {
	case renewalErr := <-renewalErrors:
		if renewalErr != nil {
			return true, renewalErr
		}
	default:
	}
	if executionErr == nil {
		_, err = r.targets.CompleteProvisioningSuccess(ctx, claim, result)
		return true, err
	}
	if executionContext.Err() != nil {
		return true, executionErr
	}
	code, message := safeProvisioningFailure(executionErr)
	_, err = r.targets.CompleteProvisioningFailure(context.WithoutCancel(ctx), claim, code, message)
	return true, err
}

func (r *ProvisioningReconciler) renewClaim(
	ctx context.Context, cancel context.CancelFunc, claim ProvisioningClaim, done <-chan struct{}, renewed chan<- struct{}, failures chan<- error,
) {
	defer close(renewed)
	ticker := time.NewTicker(r.lease / 3)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			expires, err := r.targets.RenewProvisioningClaim(ctx, claim, r.lease)
			if err != nil {
				failures <- err
				cancel()
				return
			}
			claim.ClaimExpiresAt = expires
		}
	}
}

func StableProvisioningWorkerInstanceUID(operationID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("polaris-execution-target-provisioning:"+operationID.String()))
}

func safeProvisioningFailure(err error) (string, string) {
	var apiError *problem.Error
	if errors.As(err, &apiError) && apiError.Code != "" && apiError.Message != "" {
		return apiError.Code, apiError.Message
	}
	return "provisioning_failed", "Execution target provisioning failed."
}
