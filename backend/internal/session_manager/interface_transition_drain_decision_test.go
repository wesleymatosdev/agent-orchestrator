package sessionmanager

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// awaitSourceReopen waits for the source chat's intake-reopen signal. The
// watchdog writes the FAILED phase before cancelling (so the durable code
// wins), which means the abort can legitimately lag the phase observation;
// asserting it immediately is flaky under load.
func awaitSourceReopen(t *testing.T, aborted chan struct{}, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		select {
		case <-aborted:
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(time.Millisecond)
	}
}

// A Chat -> TUI drain whose turn is blocked on an approval must fail the
// transition instead of deadlocking: the drain waits for the turn, the turn
// waits for a decision the frozen Chat UI can never receive. The regression
// the user hit left the transition in draining forever until a manual DELETE
// of the transition row. The controller aborts its own fence before returning,
// so the source stays interactive and the user can answer and retry.
func TestInterfaceTransitionChatToTUIFailsFastWhenDrainBlockedOnDecision(t *testing.T) {
	manager, store, _, chat, log := newTransitionManager(t, domain.SessionModeChat)
	chat.prepareErr = fmt.Errorf("drain handoff: %w", ports.ErrChatHandoffDecisionPending)

	transition, err := manager.StartInterfaceTransition(context.Background(), "session-1",
		domain.SessionModeTUI, domain.SessionInterfaceTransitionDrain, domain.SessionInterfaceTransitionHistoryStrict)
	if err != nil {
		t.Fatal(err)
	}
	settled := awaitTransition(t, store, transition.ID)
	if settled.Phase != domain.SessionInterfaceTransitionFailed {
		t.Fatalf("phase = %s, want failed (error = %s)", settled.Phase, settled.ErrorDetail)
	}
	if settled.ErrorCode != "DRAIN_DECISION_PENDING" {
		t.Fatalf("errorCode = %q, want DRAIN_DECISION_PENDING", settled.ErrorCode)
	}
	if !strings.Contains(settled.ErrorDetail, "answer it in Chat") {
		t.Fatalf("errorDetail = %q, want Chat-specific guidance", settled.ErrorDetail)
	}
	awaitSourceReopen(t, chat.aborted, "failed drain did not reopen the Chat source intake")
	got, _ := store.sessionGet("session-1")
	if got.Mode != domain.SessionModeChat {
		t.Fatalf("failed drain changed mode to %q", got.Mode)
	}
	if got := fmt.Sprint(*log); strings.Contains(got, "stop:chat") || strings.Contains(got, "start:tui") {
		t.Fatalf("failed drain continued past the source: %s", got)
	}
}

// Backstop for the approval deadlock: if detection is missed and the drain
// cannot finish, the watchdog must fail the transition instead of leaving it
// parked in draining silently while the session waits on user input.
func TestInterfaceTransitionDrainWatchdogFailsASilentDrainOnPendingDecision(t *testing.T) {
	manager, store, _, chat, _ := newTransitionManager(t, domain.SessionModeChat)
	manager.interfaceTransition = interfaceTransitionConfig{
		pollInterval:     time.Millisecond,
		decisionWatchdog: 20 * time.Millisecond,
	}
	blocked, _ := store.sessionGet("session-1")
	blocked.Activity = domain.Activity{State: domain.ActivityWaitingInput, LastActivityAt: time.Now()}
	store.sessionSet("session-1", blocked)
	chat.prepareBlock = make(chan struct{})

	transition, err := manager.StartInterfaceTransition(context.Background(), "session-1",
		domain.SessionModeTUI, domain.SessionInterfaceTransitionDrain, domain.SessionInterfaceTransitionHistoryStrict)
	if err != nil {
		t.Fatal(err)
	}
	settled := awaitTransition(t, store, transition.ID)
	if settled.Phase != domain.SessionInterfaceTransitionFailed {
		t.Fatalf("phase = %s, want failed (error = %s)", settled.Phase, settled.ErrorDetail)
	}
	if settled.ErrorCode != "DRAIN_BLOCKED_ON_APPROVAL" {
		t.Fatalf("errorCode = %q, want DRAIN_BLOCKED_ON_APPROVAL", settled.ErrorCode)
	}
	awaitSourceReopen(t, chat.aborted, "watchdog cancellation did not reopen the Chat source intake")
	got, _ := store.sessionGet("session-1")
	if got.Mode != domain.SessionModeChat {
		t.Fatalf("watchdog changed mode to %q", got.Mode)
	}
}

// Cancelling a drain that is stuck waiting on the turn still works and reopens
// the source: the manual escape hatch the user needed must stay available.
func TestCancelInterfaceTransitionDuringBlockedChatDrain(t *testing.T) {
	manager, store, _, chat, _ := newTransitionManager(t, domain.SessionModeChat)
	chat.prepareBlock = make(chan struct{})

	transition, err := manager.StartInterfaceTransition(context.Background(), "session-1",
		domain.SessionModeTUI, domain.SessionInterfaceTransitionDrain, domain.SessionInterfaceTransitionHistoryStrict)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, found, _ := store.GetSessionInterfaceTransition(context.Background(), transition.ID); found ||
			time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := manager.CancelInterfaceTransition(context.Background(), "session-1"); err != nil {
		t.Fatalf("cancel transition: %v", err)
	}
	settled := awaitTransition(t, store, transition.ID)
	if settled.Phase != domain.SessionInterfaceTransitionCancelled {
		t.Fatalf("phase = %s, want cancelled (error = %s)", settled.Phase, settled.ErrorDetail)
	}
	select {
	case <-chat.aborted:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not reopen the Chat source intake")
	}
	got, _ := store.sessionGet("session-1")
	if got.Mode != domain.SessionModeChat {
		t.Fatalf("cancel changed mode to %q", got.Mode)
	}
}

// The watchdog maps through errors.Is, so a plain chat quiesce failure must
// keep the generic code rather than claiming a decision was pending.
func TestInterfaceTransitionChatDrainSourceFailureKeepsGenericCode(t *testing.T) {
	manager, store, _, chat, _ := newTransitionManager(t, domain.SessionModeChat)
	chat.prepareErr = errors.New("provider did not become quiescent")

	transition, err := manager.StartInterfaceTransition(context.Background(), "session-1",
		domain.SessionModeTUI, domain.SessionInterfaceTransitionDrain, domain.SessionInterfaceTransitionHistoryStrict)
	if err != nil {
		t.Fatal(err)
	}
	settled := awaitTransition(t, store, transition.ID)
	if settled.Phase != domain.SessionInterfaceTransitionFailed {
		t.Fatalf("phase = %s, want failed (error = %s)", settled.Phase, settled.ErrorDetail)
	}
	if settled.ErrorCode != "SOURCE_QUIESCE_FAILED" {
		t.Fatalf("errorCode = %q, want SOURCE_QUIESCE_FAILED", settled.ErrorCode)
	}
}
