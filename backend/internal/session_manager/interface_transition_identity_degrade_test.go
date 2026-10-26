package sessionmanager

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// TestInterfaceTransitionHermesWithoutIdentityPairDegradesToFreshChat covers
// the upstream-hermes-main runtime: the installed adapter never declares a
// compatible TUI+Chat identity pair, so the strict gate used to refuse every
// TUI→Chat switch. The switch must instead degrade to a fresh Chat start
// through the same durable transition record, marking the row
// identity-reset and delivering an honest boundary turn into the new Chat.
func TestInterfaceTransitionHermesWithoutIdentityPairDegradesToFreshChat(t *testing.T) {
	manager, store, runtime, chat, _ := newTransitionManager(t, domain.SessionModeTUI)
	manager.agents = singleAgent{agent: fakeAgent{}}
	useFastInterfaceTransitionTimings(manager)
	rec := store.sessions["session-1"]
	rec.Harness = domain.HarnessHermes
	store.sessions["session-1"] = rec

	status, err := manager.InterfaceTransitionStatus(context.Background(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if !status.Supported {
		t.Fatalf("hermes without an identity pair must degrade, not refuse: %+v", status)
	}

	transition, err := manager.StartInterfaceTransition(context.Background(), "session-1",
		domain.SessionModeChat, domain.SessionInterfaceTransitionDrain, domain.SessionInterfaceTransitionHistoryStrict)
	if err != nil {
		t.Fatal(err)
	}
	settled := awaitTransition(t, store, transition.ID)
	if settled.Phase != domain.SessionInterfaceTransitionCompleted {
		t.Fatalf("phase = %s, code = %s, error = %s", settled.Phase, settled.ErrorCode, settled.ErrorDetail)
	}
	if settled.DegradedKind != domain.SessionInterfaceTransitionDegradedIdentityReset {
		t.Fatalf("degradedKind = %q, want identity-reset", settled.DegradedKind)
	}
	if settled.NativeConversationID != "" || chat.start.ProviderConversationID != "" {
		t.Fatalf("degraded handoff did not start Chat fresh: transition=%q target=%q",
			settled.NativeConversationID, chat.start.ProviderConversationID)
	}
	current := store.sessions["session-1"]
	if current.Mode != domain.SessionModeChat {
		t.Fatalf("mode = %s, want chat", current.Mode)
	}
	if runtime.created != 0 {
		t.Fatalf("terminal runtime created %d times while switching to Chat", runtime.created)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		pending, listErr := store.ListPendingSessionInterfaceTransitionMessages(context.Background(), transition.ID)
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(pending) == 0 {
			messages := store.messages[transition.ID]
			if len(messages) != 1 || messages[0].Message != degradedHandoffMarkerText {
				t.Fatalf("marker messages = %+v, want exactly the fresh-start boundary", messages)
			}
			if got := chat.relayMessages; len(got) != 1 || got[0] != degradedHandoffMarkerText {
				t.Fatalf("chat relay = %v, want the fresh-start boundary", got)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("degraded marker was not delivered: pending=%+v", store.messages[transition.ID])
}

// TestInterfaceTransitionHermesWithDeclaredPairKeepsStrictHandoff pins the
// fork-patched hermes (identity chain present): the handoff resumes the native
// conversation as before and records no degradation.
func TestInterfaceTransitionHermesWithDeclaredPairKeepsStrictHandoff(t *testing.T) {
	manager, store, _, chat, _ := newTransitionManager(t, domain.SessionModeTUI)
	useFastInterfaceTransitionTimings(manager)
	rec := store.sessions["session-1"]
	rec.Harness = domain.HarnessHermes
	store.sessions["session-1"] = rec

	transition, err := manager.StartInterfaceTransition(context.Background(), "session-1",
		domain.SessionModeChat, domain.SessionInterfaceTransitionDrain, domain.SessionInterfaceTransitionHistoryStrict)
	if err != nil {
		t.Fatal(err)
	}
	settled := awaitTransition(t, store, transition.ID)
	if settled.Phase != domain.SessionInterfaceTransitionCompleted {
		t.Fatalf("phase = %s, code = %s, error = %s", settled.Phase, settled.ErrorCode, settled.ErrorDetail)
	}
	if settled.DegradedKind != "" {
		t.Fatalf("degradedKind = %q, want empty for a declared identity pair", settled.DegradedKind)
	}
	if settled.NativeConversationID != "native-1" || chat.start.ProviderConversationID != "native-1" {
		t.Fatalf("declared-pair handoff changed: transition=%q target=%q",
			settled.NativeConversationID, chat.start.ProviderConversationID)
	}
	if messages := store.messages[transition.ID]; len(messages) != 0 {
		t.Fatalf("strict handoff delivered %d marker messages, want none", len(messages))
	}
}

// TestInterfaceTransitionHermesChatToTUIStillRequiresHandoffCapability keeps
// the degrade scoped to the TUI→Chat boundary: a Chat→TUI switch without a
// declared identity pair still fails closed.
func TestInterfaceTransitionHermesChatToTUIStillRequiresHandoffCapability(t *testing.T) {
	manager, store, _, chat, _ := newTransitionManager(t, domain.SessionModeChat)
	manager.agents = singleAgent{agent: fakeAgent{}}
	rec := store.sessions["session-1"]
	rec.Harness = domain.HarnessHermes
	store.sessions["session-1"] = rec

	_, err := manager.StartInterfaceTransition(context.Background(), "session-1",
		domain.SessionModeTUI, domain.SessionInterfaceTransitionDrain, domain.SessionInterfaceTransitionHistoryStrict)
	if !errors.Is(err, ErrInterfaceHandoffUnsupported) {
		t.Fatalf("error = %v, want ErrInterfaceHandoffUnsupported", err)
	}
	if len(store.transitions) != 0 || len(chat.relayMessages) != 0 {
		t.Fatal("unsupported Chat→TUI handoff mutated session or controllers")
	}
}
