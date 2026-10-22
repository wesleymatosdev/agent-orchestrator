package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// hermesRollbackForker is the session manager's hermes rollback glue.
type hermesRollbackForker interface {
	RollbackHermesToFork(ctx context.Context, id domain.SessionID, turnID, cutPrompt string, turnsAfterInclusive int) (int, error)
}

// hermesRollbackStore is the durable projection read the route needs to locate
// the cut on AO's side.
type hermesRollbackStore interface {
	GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error)
	ConversationForSession(ctx context.Context, session domain.SessionID) (domain.ConversationRecord, error)
	LoadConversationSnapshot(ctx context.Context, conversationID string) (store.ConversationSnapshot, error)
}

// hermesRollbackRoute adapts the hermes fork glue to Chat's rollback hook.
//
// The cut is resolved from AO's durable projection — the named turn's prompt
// text plus how many rollbackable turns sit at or after it — and everything
// else (fork, row discard, identity re-point, liveness) is the glue's job.
// Non-hermes sessions answer handled=false so the ordinary controller rollback
// serves them unchanged.
func hermesRollbackRoute(
	st hermesRollbackStore,
	fork hermesRollbackForker,
	log *slog.Logger,
) func(ctx context.Context, id domain.SessionID, turnID string) (int, bool, error) {
	return func(ctx context.Context, id domain.SessionID, turnID string) (int, bool, error) {
		rec, found, err := st.GetSession(ctx, id)
		if err != nil {
			return 0, true, err
		}
		if !found || rec.Harness != domain.HarnessHermes ||
			domain.NormalizeSessionMode(rec.Mode) != domain.SessionModeChat {
			return 0, false, nil
		}

		cutPrompt, turnsAfter, err := hermesRollbackCut(st, id, turnID)
		if err != nil {
			return 0, true, err
		}
		discarded, err := fork.RollbackHermesToFork(ctx, id, turnID, cutPrompt, turnsAfter)
		switch {
		case err == nil:
			log.Info("hermes rollback forked at turn", "session", id, "turn", turnID,
				"discarded", discarded, "turnsAfter", turnsAfter)
			return discarded, true, nil
		case errors.Is(err, sessionmanager.ErrRollbackNotApplicable):
			return 0, false, nil
		case errors.Is(err, sessionmanager.ErrRollbackBusy):
			return 0, true, fmt.Errorf("%w: hermes is mid-turn", chatsvc.ErrTurnRunning)
		case errors.Is(err, sessionmanager.ErrRollbackUnavailable):
			return 0, true, chatsvc.ErrRollbackUnsupported
		default:
			// The hermes adapter's cut errors are cut-resolution refusals
			// (missing session, pre-compaction target, prompt drift): the turn
			// the user named cannot be undone on the native transcript.
			return 0, true, fmt.Errorf("%w: %w", chatsvc.ErrTurnNotRollbackable, err)
		}
	}
}

// hermesRollbackCut resolves the cut from AO's durable projection: the cut
// turn must exist on the conversation, carry a provider turn id, and still be
// visible; its user prompt text is what the hermes adapter verifies against
// the native transcript.
func hermesRollbackCut(st hermesRollbackStore, id domain.SessionID, turnID string) (string, int, error) {
	conversation, err := st.ConversationForSession(context.Background(), id)
	if err != nil {
		return "", 0, fmt.Errorf("%w: %s has no conversation", chatsvc.ErrTurnNotRollbackable, id)
	}
	rows, err := st.LoadConversationSnapshot(context.Background(), conversation.ID)
	if err != nil {
		return "", 0, err
	}
	cut := -1
	for i, turn := range rows.Turns {
		if turn.ID == turnID {
			cut = i
			break
		}
	}
	if cut < 0 {
		return "", 0, fmt.Errorf("%w: %s", chatsvc.ErrTurnNotRollbackable, turnID)
	}
	turn := rows.Turns[cut]
	if turn.ProviderTurnID == "" || turn.RolledBackAt != nil {
		// The provider never accepted this turn, or it is already discarded:
		// the same refusals the controller path applies before touching the
		// provider.
		return "", 0, fmt.Errorf("%w: %s", chatsvc.ErrTurnNotRollbackable, turnID)
	}
	cutPrompt := ""
	for _, msg := range rows.Messages {
		if msg.TurnID == turnID && msg.Role == domain.MessageRoleUser && strings.TrimSpace(msg.Text) != "" {
			cutPrompt = msg.Text
			break
		}
	}
	if strings.TrimSpace(cutPrompt) == "" {
		return "", 0, fmt.Errorf("%w: %s carries no prompt text", chatsvc.ErrTurnNotRollbackable, turnID)
	}
	turnsAfter := 0
	for _, later := range rows.Turns[cut:] {
		if later.RolledBackAt == nil && later.ProviderTurnID != "" {
			turnsAfter++
		}
	}
	return cutPrompt, turnsAfter, nil
}
