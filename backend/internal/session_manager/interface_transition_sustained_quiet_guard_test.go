package sessionmanager

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Counter-direction guards for the #14 sustained-quiet drain rule (a session
// idle and quiet for the whole staleIdleLimit budget completes the TUI->chat
// drain). These tests pin that the rule cannot kill real work: durable Active
// work, a terminal surface that reports work, and freshly accepted terminal
// input must each keep blocking (or restart the quiet budget) instead of
// letting the drain complete via sustained quiet.

// TestInterfaceTransitionTUIToChatSustainedQuietNeverCompletesActiveWorkWithoutSurfaceProof
// pins case 1's static direction on the capability-free path: a session whose
// durable activity state is Active must never satisfy the sustained-quiet
// rule, even when the adapter offers no terminal activity detector and no
// surface inspector (so nothing but the durable state can veto the drain).
// This complements TestInterfaceTransitionTUIToChatDoesNotTimeOutActiveWork,
// which exercises the same invariant with a surface-capable adapter.
func TestInterfaceTransitionTUIToChatSustainedQuietNeverCompletesActiveWorkWithoutSurfaceProof(t *testing.T) {
	manager, store, runtime, _, _ := newTransitionManager(t, domain.SessionModeTUI)
	useFastInterfaceTransitionTimings(manager)
	manager.agents = singleAgent{agent: transitionAgent{}}
	rec := store.sessions["session-1"]
	rec.Activity = domain.Activity{State: domain.ActivityActive, LastActivityAt: time.Now().Add(-time.Hour)}
	store.sessions["session-1"] = rec
	runtime.aliveByHandle = map[string]bool{"runtime-1": true}
	gate := &transitionInputGate{
		acquired:    make(chan string, 1),
		released:    make(chan string, 1),
		lastInputAt: time.Now(),
	}
	manager.SetTerminalInputGate(gate)

	transition, err := manager.StartInterfaceTransition(context.Background(), "session-1", domain.SessionModeChat, domain.SessionInterfaceTransitionDrain, domain.SessionInterfaceTransitionHistoryStrict)

	if err != nil {
		t.Fatal(err)
	}
	awaitPhase(t, store, transition.ID, domain.SessionInterfaceTransitionDraining)
	time.Sleep(2 * manager.interfaceTransition.staleIdleLimit)
	current, _, err := store.GetSessionInterfaceTransition(context.Background(), transition.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Phase != domain.SessionInterfaceTransitionDraining {
		t.Fatalf("phase = %s, want draining: sustained quiet must not complete active work without surface proof", current.Phase)
	}
	if err := manager.CancelInterfaceTransition(context.Background(), "session-1"); err != nil {
		t.Fatal(err)
	}
	if runtime.destroyed != 0 {
		t.Fatalf("source runtime destroyed %d times while durable state was active", runtime.destroyed)
	}
	select {
	case <-gate.released:
	case <-time.After(time.Second):
		t.Fatal("terminal input gate remained closed after cancellation")
	}
}

// TestInterfaceTransitionTUIToChatSustainedQuietBudgetRestartsWhenWorkBeginsMidDrain
// pins case 1's mid-drain direction: a session that flips to Active partway
// through an unverified-idle window must reset the sustained-quiet budget, so
// the quiet time accumulated before the work began cannot complete the drain
// once the session goes idle again. The surface probe runs on every poll
// regardless of durable state, so the activity flips are driven from the
// drain's own capture sequence (deterministic, on the drain goroutine).
func TestInterfaceTransitionTUIToChatSustainedQuietBudgetRestartsWhenWorkBeginsMidDrain(t *testing.T) {
	manager, store, runtime, _, _ := newTransitionManager(t, domain.SessionModeTUI)
	useFastInterfaceTransitionTimings(manager)
	manager.agents = singleAgent{agent: transitionSurfaceAgent{}}
	now := time.Now()
	rec := store.sessions["session-1"]
	rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-time.Minute)}
	store.sessions["session-1"] = rec
	runtime.aliveByHandle = map[string]bool{"runtime-1": true}
	manager.SetTerminalInputGate(&transitionInputGate{
		acquired:    make(chan string, 1),
		released:    make(chan string, 1),
		lastInputAt: now,
	})

	var mu sync.Mutex
	idleAgainAt := time.Time{}
	runtime.outputForCall = func(call int) string {
		switch call {
		case 5:
			// Real work begins partway through the first quiet window, well
			// before the staleIdleLimit budget could elapse.
			if current, ok := store.sessionGet("session-1"); ok {
				current.Activity = domain.Activity{State: domain.ActivityActive, LastActivityAt: time.Now()}
				store.sessionSet("session-1", current)
			}
		case 40:
			// The work finishes after the original budget would have expired.
			if current, ok := store.sessionGet("session-1"); ok {
				current.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: time.Now()}
				store.sessionSet("session-1", current)
			}
			mu.Lock()
			idleAgainAt = time.Now()
			mu.Unlock()
		}
		return ambiguousTerminalOutput
	}

	transition, err := manager.StartInterfaceTransition(context.Background(), "session-1", domain.SessionModeChat, domain.SessionInterfaceTransitionDrain, domain.SessionInterfaceTransitionHistoryStrict)

	if err != nil {
		t.Fatal(err)
	}
	settled, completedAt := awaitTerminal(t, store, transition.ID)
	mu.Lock()
	idleAt := idleAgainAt
	mu.Unlock()
	if settled.Phase != domain.SessionInterfaceTransitionCompleted {
		t.Fatalf("transition = %+v, want completed once the restarted quiet budget elapses", settled)
	}
	if idleAt.IsZero() {
		t.Fatal("activity never flipped back to idle before the drain settled")
	}
	// The pre-work quiet window must not count toward the post-work budget:
	// completion may only happen a full staleIdleLimit after the session went
	// idle again (allowing a small scheduling margin).
	if earliest := idleAt.Add(manager.interfaceTransition.staleIdleLimit - 20*time.Millisecond); completedAt.Before(earliest) {
		t.Fatalf("drain completed at %s, before the quiet budget could restart at %s: mid-drain active work must reset the sustained-quiet clock", completedAt, earliest)
	}
	if runtime.destroyed != 1 {
		t.Fatalf("source runtime destroyed %d times, want exactly one settle-and-destroy", runtime.destroyed)
	}
}

// TestInterfaceTransitionTUIToChatSustainedQuietBudgetRestartsWhenSurfaceTurnsBusyMidDrain
// pins case 2's mid-drain direction: a terminal surface that reports active
// work partway through the drain must veto the sustained-quiet completion from
// then on, even though the durable activity row still says Idle. This
// complements TestInterfaceTransitionTUIToChatTreatsCurrentSurfaceActivityAsBusy,
// which only exercises a surface that is busy from the first observation.
func TestInterfaceTransitionTUIToChatSustainedQuietBudgetRestartsWhenSurfaceTurnsBusyMidDrain(t *testing.T) {
	manager, store, runtime, _, _ := newTransitionManager(t, domain.SessionModeTUI)
	useFastInterfaceTransitionTimings(manager)
	manager.agents = singleAgent{agent: transitionSurfaceAgent{}}
	now := time.Now()
	rec := store.sessions["session-1"]
	rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-time.Minute)}
	store.sessions["session-1"] = rec
	runtime.aliveByHandle = map[string]bool{"runtime-1": true}
	runtime.outputForCall = func(call int) string {
		if call <= 3 {
			return ambiguousTerminalOutput
		}
		return activeTerminalOutput
	}
	gate := &transitionInputGate{
		acquired:    make(chan string, 1),
		released:    make(chan string, 1),
		lastInputAt: now,
	}
	manager.SetTerminalInputGate(gate)

	transition, err := manager.StartInterfaceTransition(context.Background(), "session-1", domain.SessionModeChat, domain.SessionInterfaceTransitionDrain, domain.SessionInterfaceTransitionHistoryStrict)

	if err != nil {
		t.Fatal(err)
	}
	awaitPhase(t, store, transition.ID, domain.SessionInterfaceTransitionDraining)
	time.Sleep(3 * manager.interfaceTransition.staleIdleLimit)
	current, _, err := store.GetSessionInterfaceTransition(context.Background(), transition.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Phase != domain.SessionInterfaceTransitionDraining {
		t.Fatalf("phase = %s, want draining: a busy terminal surface must veto sustained quiet even when the durable row is idle", current.Phase)
	}
	if err := manager.CancelInterfaceTransition(context.Background(), "session-1"); err != nil {
		t.Fatal(err)
	}
	if runtime.destroyed != 0 {
		t.Fatalf("source runtime destroyed %d times while the surface reported active work", runtime.destroyed)
	}
	select {
	case <-gate.released:
	case <-time.After(time.Second):
		t.Fatal("terminal input gate remained closed after cancellation")
	}
}

// TestInterfaceTransitionTUIToChatSustainedQuietWaitsOutTheInputSettleWindow
// pins case 3: terminal input accepted right as the drain gate closed must not
// let the drain treat evidence gathered inside the input settle window as
// proof, and the drain must spend the full sustained-quiet budget rather than
// completing on the settle window alone. The idle fact predates the accepted
// input, so only the quiet budget can complete this drain at all.
func TestInterfaceTransitionTUIToChatSustainedQuietWaitsOutTheInputSettleWindow(t *testing.T) {
	manager, store, runtime, _, _ := newTransitionManager(t, domain.SessionModeTUI)
	useFastInterfaceTransitionTimings(manager)
	manager.agents = singleAgent{agent: transitionDetectorAgent{}}
	now := time.Now()
	rec := store.sessions["session-1"]
	rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-time.Minute)}
	store.sessions["session-1"] = rec
	runtime.aliveByHandle = map[string]bool{"runtime-1": true}
	runtime.outputs = []string{ambiguousTerminalOutput}
	manager.SetTerminalInputGate(&transitionInputGate{
		acquired:    make(chan string, 1),
		released:    make(chan string, 1),
		lastInputAt: now,
	})

	startedAt := time.Now()
	transition, err := manager.StartInterfaceTransition(context.Background(), "session-1", domain.SessionModeChat, domain.SessionInterfaceTransitionDrain, domain.SessionInterfaceTransitionHistoryStrict)

	if err != nil {
		t.Fatal(err)
	}
	settled, _ := awaitTerminal(t, store, transition.ID)
	if settled.Phase != domain.SessionInterfaceTransitionCompleted {
		t.Fatalf("transition = %+v, want completed via the sustained-quiet budget", settled)
	}
	// Evidence must respect the input settle window on the legacy detector
	// path: no capture before the accepted input has settled.
	if len(runtime.outputCallTimes) == 0 {
		t.Fatal("terminal detector was never consulted")
	}
	if firstCapture := runtime.outputCallTimes[0]; firstCapture.Before(now.Add(manager.interfaceTransition.idleSettle)) {
		t.Fatalf("first terminal capture at %s, before the accepted input settled at %s",
			firstCapture, now.Add(manager.interfaceTransition.idleSettle))
	}
	// Completion must come from the full quiet budget (staleIdleLimit), not
	// from the much shorter settle window elapsing.
	if elapsed := time.Since(startedAt); elapsed < manager.interfaceTransition.staleIdleLimit-15*time.Millisecond {
		t.Fatalf("drain completed after %s, before the sustained-quiet budget of %s elapsed", elapsed, manager.interfaceTransition.staleIdleLimit)
	}
	if runtime.destroyed != 1 {
		t.Fatalf("source runtime destroyed %d times, want exactly one settle-and-destroy", runtime.destroyed)
	}
}

// sessionGet/sessionSet provide locked access to the fake session map for
// mutations that run on the drain goroutine (from outputForCall callbacks).
func (s *transitionStore) sessionGet(id domain.SessionID) (domain.SessionRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.sessions[id]
	return r, ok
}

func (s *transitionStore) sessionSet(id domain.SessionID, rec domain.SessionRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = rec
}

// awaitPhase waits until the transition reaches the given phase.
func awaitPhase(t *testing.T, store *transitionStore, id string, phase domain.SessionInterfaceTransitionPhase) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		current, ok, err := store.GetSessionInterfaceTransition(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if ok && current.Phase == phase {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("interface transition never reached phase %s", phase)
}

// awaitTerminal waits until the transition reaches any terminal phase and also
// reports when that was observed, so tests can assert on completion timing.
func awaitTerminal(t *testing.T, store *transitionStore, id string) (domain.SessionInterfaceTransition, time.Time) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		current, ok, err := store.GetSessionInterfaceTransition(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if ok && current.Phase.Terminal() {
			return current, time.Now()
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("interface transition did not settle, phase = %s", current.Phase)
		}
		time.Sleep(time.Millisecond)
	}
}
