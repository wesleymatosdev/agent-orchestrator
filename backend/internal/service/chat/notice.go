package chat

import (
	"context"
	"errors"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// NotifyReportBatch makes a settled worker-report batch visible in the session's
// conversation without dispatching a provider turn. The daemon-authored notice
// rides the conversation CDC pipeline, so connected clients render the report
// card in realtime while the orchestrator agent stays untouched; agent-side
// injection remains piggyback's job on the next owner turn. Idempotent per
// conversation through clientMessageID.
func (s *Service) NotifyReportBatch(ctx context.Context, id domain.SessionID, text, clientMessageID string) (bool, error) {
	if s == nil || s.store == nil {
		return false, errors.New("chat: store is required")
	}
	if _, err := s.requireChatSession(ctx, id); err != nil {
		return false, err
	}
	conversation, err := s.store.ConversationForSession(ctx, id)
	if err != nil {
		return false, fmt.Errorf("report notice %s: conversation: %w", id, err)
	}
	created, err := s.store.AppendConversationNotice(ctx, conversation.ID, id, domain.ConversationMessage{
		ID:              s.newID(),
		Text:            text,
		Origin:          domain.MessageOriginAutomation,
		ClientMessageID: clientMessageID,
	}, s.now())
	if err != nil {
		return false, fmt.Errorf("report notice %s: %w", id, err)
	}
	return created, nil
}
