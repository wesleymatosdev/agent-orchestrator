package daemon

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type fakeRollbackStore struct {
	session      domain.SessionRecord
	found        bool
	conversation domain.ConversationRecord
	rows         store.ConversationSnapshot
	getErr       error
}

func (f *fakeRollbackStore) GetSession(context.Context, domain.SessionID) (domain.SessionRecord, bool, error) {
	return f.session, f.found, f.getErr
}

func (f *fakeRollbackStore) ConversationForSession(_ context.Context, _ domain.SessionID) (domain.ConversationRecord, error) {
	return f.conversation, nil
}

func (f *fakeRollbackStore) LoadConversationSnapshot(context.Context, string) (store.ConversationSnapshot, error) {
	return f.rows, nil
}

type fakeRollbackFork struct {
	gotTurnID, gotPrompt string
	gotTurnsAfter        int
	discarded            int
	err                  error
}

func (f *fakeRollbackFork) RollbackHermesToFork(
	_ context.Context, _ domain.SessionID, turnID, cutPrompt string, turnsAfterInclusive int,
) (int, error) {
	f.gotTurnID, f.gotPrompt, f.gotTurnsAfter = turnID, cutPrompt, turnsAfterInclusive
	return f.discarded, f.err
}

func rollbackRouteFixture(harness domain.AgentHarness) (*fakeRollbackStore, *fakeRollbackFork, func(context.Context, domain.SessionID, string) (int, bool, error)) {
	st := &fakeRollbackStore{
		session: domain.SessionRecord{
			ID:      "mer-1",
			Harness: harness,
			Mode:    domain.SessionModeChat,
			Metadata: domain.SessionMetadata{
				AgentSessionID:         "native-1",
				ProviderConversationID: "native-1",
			},
		},
		found:        true,
		conversation: domain.ConversationRecord{ID: "conv-1"},
		rows: store.ConversationSnapshot{
			Turns: []domain.ConversationTurn{
				{ID: "t-1", ProviderTurnID: "p-1"},
				{ID: "t-2", ProviderTurnID: "p-2"},
				{ID: "t-3", ProviderTurnID: "p-3"},
				{ID: "t-4", ProviderTurnID: "p-4", RolledBackAt: stampedTimePtr()},
			},
			Messages: []domain.ConversationMessage{
				{ID: "m-1", TurnID: "t-1", Role: domain.MessageRoleUser, Text: "first ask"},
				{ID: "m-2", TurnID: "t-2", Role: domain.MessageRoleUser, Text: "second ask"},
				{ID: "m-3", TurnID: "t-3", Role: domain.MessageRoleUser, Text: "third ask"},
			},
		},
	}
	fork := &fakeRollbackFork{discarded: 2}
	return st, fork, hermesRollbackRoute(st, fork, discardLogger())
}

func stampedTimePtr() *time.Time {
	at := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	return &at
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestHermesRollbackRouteResolvesTheCutFromTheDurableProjection(t *testing.T) {
	st, fork, route := rollbackRouteFixture(domain.HarnessHermes)
	_ = st

	discarded, handled, err := route(context.Background(), "mer-1", "t-2")
	if err != nil || !handled || discarded != 2 {
		t.Fatalf("route = %d, %v, %v", discarded, handled, err)
	}
	if fork.gotTurnID != "t-2" || fork.gotPrompt != "second ask" || fork.gotTurnsAfter != 2 {
		t.Fatalf("fork got turn=%q prompt=%q after=%d", fork.gotTurnID, fork.gotPrompt, fork.gotTurnsAfter)
	}
}

func TestHermesRollbackRouteFallsThroughForOtherHarnesses(t *testing.T) {
	_, _, route := rollbackRouteFixture(domain.HarnessClaudeCode)
	if _, handled, err := route(context.Background(), "mer-1", "t-2"); handled || err != nil {
		t.Fatalf("handled=%v err=%v; want fall-through", handled, err)
	}
}

func TestHermesRollbackRouteRefusesUnknownAndUnprovideredTurns(t *testing.T) {
	for name, turnID := range map[string]string{
		"unknown":  "t-missing",
		"rollback": "t-4",
	} {
		_, fork, route := rollbackRouteFixture(domain.HarnessHermes)
		_, handled, err := route(context.Background(), "mer-1", turnID)
		if !handled || !errors.Is(err, chatsvc.ErrTurnNotRollbackable) {
			t.Fatalf("%s: handled=%v err=%v", name, handled, err)
		}
		if fork.gotTurnID != "" {
			t.Fatalf("%s: glue must not be consulted", name)
		}
	}
}

func TestHermesRollbackRouteMapsGlueSentinels(t *testing.T) {
	_, fork, route := rollbackRouteFixture(domain.HarnessHermes)
	fork.err = sessionmanager.ErrRollbackBusy
	if _, handled, err := route(context.Background(), "mer-1", "t-2"); !handled || !errors.Is(err, chatsvc.ErrTurnRunning) {
		t.Fatalf("busy: handled=%v err=%v", handled, err)
	}

	_, fork, route = rollbackRouteFixture(domain.HarnessHermes)
	fork.err = sessionmanager.ErrRollbackNotApplicable
	if _, handled, err := route(context.Background(), "mer-1", "t-2"); handled || err != nil {
		t.Fatalf("not applicable: handled=%v err=%v", handled, err)
	}

	_, fork, route = rollbackRouteFixture(domain.HarnessHermes)
	fork.err = errors.New("native transcript drift")
	_, handled, err := route(context.Background(), "mer-1", "t-2")
	if !handled || !errors.Is(err, chatsvc.ErrTurnNotRollbackable) {
		t.Fatalf("cut refusal: handled=%v err=%v", handled, err)
	}
}
