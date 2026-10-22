package sessionmanager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// HermesForker is the hermes adapter capability a rollback needs: create a
// fork-at-point leaf on the native transcript without touching existing
// history rows, and record the adoption note.
type HermesForker interface {
	ForkAtCut(ctx context.Context, dataDir, aoSessionID, recordedAgentSessionID, cutPrompt string, turnsAfterInclusive int) (string, error)
	RecordRollbackNote(dataDir, aoSessionID, childID string, droppedTurns int) error
}

var (
	// ErrRollbackBusy refuses a rollback while the session's agent is mid-turn
	// or parked on an approval: discarding history it is still writing into (or
	// about to act on) would leave its memory and AO's timeline describing
	// different conversations.
	ErrRollbackBusy = errors.New("hermes rollback: the session is mid-turn")
	// ErrRollbackNotApplicable reports the session is not a hermes chat with a
	// recorded native conversation, so the fork path must not touch it and the
	// ordinary controller rollback should answer instead.
	ErrRollbackNotApplicable = errors.New("hermes rollback: not a hermes chat session")
	// ErrRollbackUnavailable reports the daemon was wired without the hermes
	// fork capability.
	ErrRollbackUnavailable = errors.New("hermes rollback: adapter capability is not wired")
	// ErrRollbackHostAlive reports the live provider host could not be parked:
	// hibernating it found work in flight (a queued message, a pending
	// approval, background work), so forking under it would race a writer and
	// leave its warm memory describing the pre-rollback conversation.
	ErrRollbackHostAlive = errors.New("hermes rollback: the provider host is still running")
)

// rollbackConversationStore is the narrow read the rollback needs to find the
// session's conversation row.
type rollbackConversationStore interface {
	ConversationForSession(ctx context.Context, session domain.SessionID) (domain.ConversationRecord, error)
}

// RollbackHermesToFork maps a rollback-at-message onto hermes' own rewind
// semantics: fork the native transcript at the cut (never mutating existing
// rows), discard AO's rows from the cut onward, re-point AO's recorded
// provider identity at the new leaf, and leave the session stopped — the next
// message auto-wakes it on the new leaf through the ordinary hibernation wake,
// whose resume replays the child transcript into a fresh provider host.
//
// cutPrompt is the cut turn's prompt text and turnsAfterInclusive is how many
// of AO's rollbackable turns sit at or after the cut; the hermes adapter
// verifies both against the native transcript and fails closed on drift.
//
// Sessions that are not hermes chats answer ErrRollbackNotApplicable so the
// caller can fall through to the ordinary controller rollback.
func (m *Manager) RollbackHermesToFork(
	ctx context.Context,
	id domain.SessionID,
	turnID, cutPrompt string,
	turnsAfterInclusive int,
) (int, error) {
	if m.hermesFork == nil {
		return 0, ErrRollbackUnavailable
	}
	rec, found, err := m.store.GetSession(ctx, id)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf("%w: session %s is unknown", ErrRollbackNotApplicable, id)
	}
	if rec.Harness != domain.HarnessHermes ||
		domain.NormalizeSessionMode(rec.Mode) != domain.SessionModeChat ||
		rec.IsTerminated ||
		rec.Metadata.AgentSessionID == "" {
		return 0, ErrRollbackNotApplicable
	}
	if rec.Activity.State == domain.ActivityActive || rec.Activity.State == domain.ActivityBlocked {
		return 0, ErrRollbackBusy
	}
	conversations, ok := m.store.(rollbackConversationStore)
	if !ok {
		return 0, fmt.Errorf("hermes rollback %s: conversation storage is unavailable", id)
	}
	conversation, err := conversations.ConversationForSession(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("hermes rollback %s: conversation: %w", id, err)
	}

	// A live controller still holds the old conversation in memory, and its
	// host process is a concurrent writer on the native store. Park it first,
	// unconditionally: the controller stops, the host shuts down, and the
	// durable hibernation marker makes the next message wake the session
	// through the ordinary resume path — on the new leaf, because the recorded
	// provider identity below now names it. The recheck is the contract: a
	// hibernation that found work in flight did NOT stop the host, and forking
	// under it would race a writer and orphan its warm context.
	live := m.chat != nil && m.chat.HasLiveChatController(id)
	if live {
		hibernator, canHibernate := m.chat.(interface {
			HibernateChatForUpdate(ctx context.Context, id domain.SessionID) (bool, error)
		})
		if !canHibernate {
			return 0, fmt.Errorf("hermes rollback %s: %w (chat launcher cannot park the host)", id, ErrRollbackHostAlive)
		}
		if _, err := hibernator.HibernateChatForUpdate(ctx, id); err != nil {
			return 0, fmt.Errorf("hermes rollback %s: park the live host: %w", id, err)
		}
		if m.chat.HasLiveChatController(id) {
			return 0, fmt.Errorf("hermes rollback %s: %w (work in flight kept it running)", id, ErrRollbackHostAlive)
		}
	}

	child, err := m.hermesFork.ForkAtCut(ctx, m.dataDir, string(id), rec.Metadata.AgentSessionID, cutPrompt, turnsAfterInclusive)
	if err != nil {
		return 0, fmt.Errorf("hermes rollback %s: %w", id, err)
	}

	// Provider fork committed; AO's rows follow. A failure here leaves the
	// disagreement repairable (the parent transcript is untouched and the note
	// below names the leaf), the same contract the controller rollback has.
	discarded, err := m.store.RollbackTurns(ctx, conversation.ID, turnID, time.Now())
	if err != nil {
		return 0, fmt.Errorf("hermes rollback %s: discard AO rows: %w", id, err)
	}

	// Re-point the recorded identity: resume and restore both read these, so
	// the next wake and the next native restore land on the child. The launch
	// identity proof and the checkpoint facts named the old conversation and
	// cannot survive it.
	rec.Metadata.AgentSessionID = child
	rec.Metadata.ProviderConversationID = child
	rec.Metadata.AgentSessionIDLaunchID = ""
	rec.Metadata.ConversationCheckpointNativeID = ""
	rec.Metadata.ConversationCheckpointTurnID = ""
	if err := m.store.UpdateSession(ctx, rec); err != nil {
		return 0, fmt.Errorf("hermes rollback %s: re-point provider identity: %w", id, err)
	}
	// Best effort: the note is inspectability, not correctness.
	_ = m.hermesFork.RecordRollbackNote(m.dataDir, string(id), child, turnsAfterInclusive)
	return discarded, nil
}
