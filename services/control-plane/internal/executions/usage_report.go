package executions

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

type ExecutionUsageReportInput struct {
	LeaseInput
	ReportSequence      int64 `json:"reportSequence"`
	NetworkIngressBytes int64 `json:"networkIngressBytes"`
	NetworkEgressBytes  int64 `json:"networkEgressBytes"`
}

type ExecutionUsageReportResult struct {
	ExecutionID         uuid.UUID `json:"executionId"`
	Generation          int64     `json:"generation"`
	ReportSequence      int64     `json:"reportSequence"`
	NetworkIngressBytes int64     `json:"networkIngressBytes"`
	NetworkEgressBytes  int64     `json:"networkEgressBytes"`
}

func (s *Service) ReportExecutionUsage(
	ctx context.Context,
	worker persistence.WorkerInstance,
	executionID uuid.UUID,
	input ExecutionUsageReportInput,
	requestID string,
) (OperationResult[ExecutionUsageReportResult], error) {
	if input.ReportSequence < 1 || input.NetworkIngressBytes < 0 || input.NetworkEgressBytes < 0 {
		return OperationResult[ExecutionUsageReportResult]{}, problem.New(400, "invalid_execution_usage_report", "Usage report sequence and network byte counters must be non-negative.")
	}
	return runExecutionIdempotent(ctx, s, worker, requestID, "execution.usage_report", executionID, input.LeaseInput, struct {
		ExecutionID uuid.UUID                 `json:"executionId"`
		Input       ExecutionUsageReportInput `json:"input"`
	}{executionID, input}, 200, func(tx *gorm.DB) (ExecutionUsageReportResult, error) {
		_, execution, err := s.lockLease(ctx, tx, worker, executionID, input.LeaseInput, true)
		if err != nil {
			return ExecutionUsageReportResult{}, err
		}
		var current persistence.ExecutionUsageSummary
		loadErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND execution_id = ? AND generation = ?", execution.TenantID, execution.ID, execution.Generation).
			Take(&current).Error
		if errors.Is(loadErr, gorm.ErrRecordNotFound) {
			provider := "unknown"
			if execution.Provider != nil && strings.TrimSpace(*execution.Provider) != "" {
				provider = strings.TrimSpace(*execution.Provider)
			}
			current = persistence.ExecutionUsageSummary{
				TenantID: execution.TenantID, ExecutionID: execution.ID, Generation: execution.Generation,
				SessionID: execution.SessionID, TurnID: execution.TurnID, Provider: provider,
				CurrencyCode: "USD", NetworkIngressBytes: input.NetworkIngressBytes,
				NetworkEgressBytes: input.NetworkEgressBytes, NetworkReportSequence: input.ReportSequence,
			}
			if err := tx.WithContext(ctx).Create(&current).Error; err != nil {
				return ExecutionUsageReportResult{}, problem.Wrap(409, "execution_usage_create_failed", "Execution Usage could not be created.", err)
			}
		} else if loadErr != nil {
			return ExecutionUsageReportResult{}, problem.Wrap(500, "execution_usage_load_failed", "Execution Usage could not be loaded.", loadErr)
		} else {
			if input.ReportSequence <= current.NetworkReportSequence {
				return ExecutionUsageReportResult{}, problem.New(409, "execution_usage_report_sequence_conflict", "Usage report sequence must increase monotonically.")
			}
			if input.NetworkIngressBytes < current.NetworkIngressBytes || input.NetworkEgressBytes < current.NetworkEgressBytes {
				return ExecutionUsageReportResult{}, problem.New(409, "execution_usage_counter_regressed", "Network Usage counters cannot decrease within an Execution generation.")
			}
			result := tx.WithContext(ctx).Model(&persistence.ExecutionUsageSummary{}).
				Where("tenant_id = ? AND execution_id = ? AND generation = ? AND network_report_sequence = ?", execution.TenantID, execution.ID, execution.Generation, current.NetworkReportSequence).
				Updates(map[string]any{
					"network_ingress_bytes":   input.NetworkIngressBytes,
					"network_egress_bytes":    input.NetworkEgressBytes,
					"network_report_sequence": input.ReportSequence,
				})
			if result.Error != nil || result.RowsAffected != 1 {
				return ExecutionUsageReportResult{}, problem.Wrap(409, "execution_usage_report_sequence_conflict", "Usage report changed; retry with a fresh sequence.", result.Error)
			}
		}
		return ExecutionUsageReportResult{
			ExecutionID: execution.ID, Generation: execution.Generation, ReportSequence: input.ReportSequence,
			NetworkIngressBytes: input.NetworkIngressBytes, NetworkEgressBytes: input.NetworkEgressBytes,
		}, nil
	})
}
