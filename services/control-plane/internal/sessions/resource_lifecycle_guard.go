package sessions

import (
	"time"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func RequireSessionWithinAbsoluteLifetime(
	session persistence.AgentSession,
	now time.Time,
) error {
	if session.AbsoluteExpiresAt != nil && !session.AbsoluteExpiresAt.After(now.UTC()) {
		return problem.New(
			409,
			"session_absolute_expired",
			"The Session reached its absolute lifetime and cannot start a new Execution.",
		)
	}
	return nil
}
