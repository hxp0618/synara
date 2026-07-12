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

func (s *Service) loadWorkload(ctx context.Context, tx *gorm.DB, execution persistence.AgentExecution) (Workload, error) {
	var row struct {
		TenantID                 uuid.UUID  `gorm:"column:tenant_id"`
		OrganizationID           uuid.UUID  `gorm:"column:organization_id"`
		ProjectID                uuid.UUID  `gorm:"column:project_id"`
		SessionID                uuid.UUID  `gorm:"column:session_id"`
		TurnID                   uuid.UUID  `gorm:"column:turn_id"`
		SessionTitle             string     `gorm:"column:session_title"`
		Provider                 string     `gorm:"column:provider"`
		ProviderRuntimeBindingID *uuid.UUID `gorm:"column:provider_runtime_binding_id"`
		RemoteWorkspaceID        *uuid.UUID `gorm:"column:remote_workspace_id"`
		WorkerManifestID         *uuid.UUID `gorm:"column:worker_manifest_id"`
		Model                    *string    `gorm:"column:model"`
		ProviderCredentialID     *uuid.UUID `gorm:"column:provider_credential_id"`
		InputText                string     `gorm:"column:input_text"`
		RuntimeMode              string     `gorm:"column:runtime_mode"`
		InteractionMode          string     `gorm:"column:interaction_mode"`
		RepositoryURL            *string    `gorm:"column:repository_url"`
		DefaultBranch            string     `gorm:"column:default_branch"`
	}
	err := tx.WithContext(ctx).Table("agent_executions AS e").
		Select(`e.tenant_id, s.organization_id, s.project_id, e.session_id, e.turn_id,
			s.title AS session_title, COALESCE(e.provider, s.provider) AS provider,
			e.provider_runtime_binding_id, e.remote_workspace_id, e.worker_manifest_id,
			s.model, s.provider_credential_id, t.input_text, t.runtime_mode, t.interaction_mode,
			p.repository_url, p.default_branch`).
		Joins("JOIN agent_sessions AS s ON s.tenant_id = e.tenant_id AND s.id = e.session_id").
		Joins("JOIN agent_turns AS t ON t.tenant_id = e.tenant_id AND t.session_id = e.session_id AND t.id = e.turn_id").
		Joins("JOIN projects AS p ON p.tenant_id = s.tenant_id AND p.id = s.project_id").
		Where("e.tenant_id = ? AND e.id = ?", execution.TenantID, execution.ID).
		Take(&row).Error
	if err != nil {
		return Workload{}, problem.Wrap(500, "execution_workload_load_failed", "Failed to load the execution workload.", err)
	}
	history, err := loadConversationHistory(ctx, tx, execution)
	if err != nil {
		return Workload{}, err
	}
	return Workload{
		TenantID: row.TenantID, OrganizationID: row.OrganizationID, ProjectID: row.ProjectID,
		SessionID: row.SessionID, TurnID: row.TurnID, SessionTitle: row.SessionTitle,
		Provider: row.Provider, ProviderRuntimeBindingID: row.ProviderRuntimeBindingID,
		RemoteWorkspaceID: row.RemoteWorkspaceID, WorkerManifestID: row.WorkerManifestID,
		Model: row.Model, ProviderCredentialID: row.ProviderCredentialID, InputText: row.InputText,
		RuntimeMode: row.RuntimeMode, InteractionMode: row.InteractionMode,
		RepositoryURL: row.RepositoryURL, DefaultBranch: row.DefaultBranch,
		ConversationHistory: history,
	}, nil
}

const (
	conversationHistoryEventLimit = 500
	conversationHistoryByteLimit  = 256 << 10
)

func loadConversationHistory(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
) ([]ConversationMessage, error) {
	var current persistence.SessionEvent
	err := tx.WithContext(ctx).
		Select("tenant_id", "session_id", "sequence").
		Where("tenant_id = ? AND session_id = ? AND execution_id = ? AND event_type = ?",
			execution.TenantID, execution.SessionID, execution.ID, "turn.created").
		Take(&current).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, problem.Wrap(500, "execution_history_cursor_load_failed", "Failed to locate the current Turn in Session history.", err)
	}
	var events []persistence.SessionEvent
	if err := tx.WithContext(ctx).
		Where("tenant_id = ? AND session_id = ? AND sequence < ? AND event_type IN ?",
			execution.TenantID, execution.SessionID, current.Sequence,
			[]string{"turn.created", "turn.steer-requested", "runtime.output.delta"}).
		Order("sequence DESC").Limit(conversationHistoryEventLimit).Find(&events).Error; err != nil {
		return nil, problem.Wrap(500, "execution_history_load_failed", "Failed to load Session conversation history.", err)
	}
	messages := make([]ConversationMessage, 0, len(events))
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		role, key := "", ""
		switch event.EventType {
		case "turn.created":
			role, key = "user", "inputText"
		case "turn.steer-requested":
			role, key = "user", "inputText"
		case "runtime.output.delta":
			role, key = "assistant", "text"
		}
		text, _ := event.Payload[key].(string)
		text = strings.TrimSpace(text)
		if role == "" || text == "" {
			continue
		}
		if role == "assistant" && len(messages) > 0 && messages[len(messages)-1].Role == role {
			messages[len(messages)-1].Text += text
			continue
		}
		messages = append(messages, ConversationMessage{Role: role, Text: text})
	}
	return boundConversationHistory(messages, conversationHistoryByteLimit), nil
}

func boundConversationHistory(messages []ConversationMessage, byteLimit int) []ConversationMessage {
	if byteLimit <= 0 || len(messages) == 0 {
		return nil
	}
	start, total := len(messages), 0
	for index := len(messages) - 1; index >= 0; index-- {
		size := len(messages[index].Role) + len(messages[index].Text)
		if total+size > byteLimit {
			break
		}
		total += size
		start = index
	}
	if start == len(messages) {
		last := messages[len(messages)-1]
		if len(last.Text) > byteLimit {
			last.Text = last.Text[len(last.Text)-byteLimit:]
		}
		return []ConversationMessage{last}
	}
	return append([]ConversationMessage(nil), messages[start:]...)
}
