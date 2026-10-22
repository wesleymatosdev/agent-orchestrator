package acp

import (
	"context"
	"strings"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// newReentryTestConversation starts one fake provider conversation and drains
// the initial controller-ready notification, leaving an idle (parked) session.
func newReentryTestConversation(t *testing.T) *conversation {
	t.Helper()
	cfg := Config{Launch: func(context.Context, LaunchConfig) (Launch, error) { return Launch{Command: "fake"}, nil }}
	d := New(cfg, nil)
	d.useTestProcess(fakeSpawn(&fakeAgent{}))
	conv, err := d.Start(context.Background(), ports.ChatStartConfig{WorkspacePath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conv.Close() })
	_ = nextEvent(t, conv.Events())
	return conv.(*conversation)
}

// reentryChunk pushes one out-of-turn agent message chunk — the ACP shape of a
// background subagent result re-entering an idle session (issue #15).
func reentryChunk(t *testing.T, conv *conversation, text string) {
	t.Helper()
	if err := conv.processUpdate(acpsdk.SessionNotification{
		SessionId: acpsdk.SessionId(conv.ProviderConversationID()),
		Update:    acpsdk.UpdateAgentMessageText(text),
	}); err != nil {
		t.Fatal(err)
	}
}

func drainReentryEvents(t *testing.T, conv *conversation) []ports.ChatEvent {
	t.Helper()
	var events []ports.ChatEvent
	for {
		select {
		case event := <-conv.Events():
			events = append(events, event)
		default:
			return events
		}
	}
}

// TestACPReentryOutputAdoptedAsProviderTurn reproduces issue #15's delivery
// half: a background result that re-enters an idle session must be adopted as
// a provider-initiated turn — attributed, visible, and hibernation-blocking —
// instead of arriving with an empty provider turn that nothing durable groups.
func TestACPReentryOutputAdoptedAsProviderTurn(t *testing.T) {
	conv := newReentryTestConversation(t)

	reentryChunk(t, conv, "reviewer A finished: 2 findings\n")

	events := drainReentryEvents(t, conv)
	var started *ports.ChatEvent
	var delta *ports.ChatEvent
	for i := range events {
		switch events[i].Kind {
		case ports.ChatEventTurnStarted:
			started = &events[i]
		case ports.ChatEventMessageDelta:
			delta = &events[i]
		case ports.ChatEventControllerState:
			if events[i].ControllerState != ports.ChatControllerBusy {
				t.Fatalf("controller state after reentry adoption = %v, want busy", events[i].ControllerState)
			}
		}
	}
	if started == nil {
		t.Fatalf("no turn started for provider-initiated output; events = %#v", events)
	}
	if !strings.HasPrefix(started.ProviderTurnID, reentryTurnIDPrefix) {
		t.Fatalf("reentry turn id = %q, want prefix %q", started.ProviderTurnID, reentryTurnIDPrefix)
	}
	if delta == nil {
		t.Fatalf("reentry output was not delivered as a message delta; events = %#v", events)
	}
	if delta.ProviderTurnID != started.ProviderTurnID {
		t.Fatalf("reentry delta attributed to turn %q, want %q", delta.ProviderTurnID, started.ProviderTurnID)
	}
	if delta.Delta != "reviewer A finished: 2 findings\n" {
		t.Fatalf("reentry delta text = %q", delta.Delta)
	}

	conv.mu.Lock()
	active, reentry := conv.activeTurn, conv.reentryTurn
	conv.mu.Unlock()
	if active != started.ProviderTurnID || reentry != started.ProviderTurnID {
		t.Fatalf("conversation turn tracking = (%q, %q), want (%q, %q)",
			active, reentry, started.ProviderTurnID, started.ProviderTurnID)
	}
	if canSleep, err := ports.ChatProviderHibernator(conv).CanHibernate(context.Background()); err != nil || canSleep {
		t.Fatalf("CanHibernate with an open reentry turn = %v, %v; want false", canSleep, err)
	}

	// A quiet close settles the turn and restores the idle session.
	conv.finishReentryTurn(domain.TurnStateCompleted)
	settled := drainReentryEvents(t, conv)
	var completed *ports.ChatEvent
	var message *ports.ChatEvent
	var ready bool
	for i := range settled {
		switch settled[i].Kind {
		case ports.ChatEventTurnCompleted:
			completed = &settled[i]
		case ports.ChatEventMessageCompleted:
			message = &settled[i]
		case ports.ChatEventControllerState:
			ready = ready || settled[i].ControllerState == ports.ChatControllerReady
		}
	}
	if completed == nil || completed.ProviderTurnID != started.ProviderTurnID ||
		completed.TurnState != domain.TurnStateCompleted {
		t.Fatalf("reentry turn completion = %#v; events = %#v", completed, settled)
	}
	if message == nil || message.ProviderTurnID != started.ProviderTurnID ||
		message.Text != "reviewer A finished: 2 findings\n" {
		t.Fatalf("reentry message not settled visibly: %#v", message)
	}
	if !ready {
		t.Fatalf("controller never returned to ready after the reentry turn settled: %#v", settled)
	}
	conv.mu.Lock()
	stillActive, stillReentry := conv.activeTurn, conv.reentryTurn
	conv.mu.Unlock()
	if stillActive != "" || stillReentry != "" {
		t.Fatalf("reentry turn not released: active=%q reentry=%q", stillActive, stillReentry)
	}
	if canSleep, err := ports.ChatProviderHibernator(conv).CanHibernate(context.Background()); err != nil || !canSleep {
		t.Fatalf("CanHibernate after the reentry turn settled = %v, %v; want true", canSleep, err)
	}
}

// TestACPReentryTurnSettlesWhenQuiet pins the automatic close: ACP has no
// out-of-turn completion notification, so the adopted turn must settle itself
// once the provider's reentry output has gone quiet, releasing the session for
// the next dispatched prompt.
func TestACPReentryTurnSettlesWhenQuiet(t *testing.T) {
	origDelay := reentrySettleDelay
	reentrySettleDelay = 120 * time.Millisecond
	defer func() { reentrySettleDelay = origDelay }()

	conv := newReentryTestConversation(t)
	reentryChunk(t, conv, "first result\n")

	var turnID string
	for _, event := range drainReentryEvents(t, conv) {
		if event.Kind == ports.ChatEventTurnStarted {
			turnID = event.ProviderTurnID
		}
	}
	if turnID == "" {
		t.Fatal("reentry output did not start a provider-initiated turn")
	}

	// Continued reentry output re-arms the quiet timer before it can fire.
	time.Sleep(reentrySettleDelay / 2)
	reentryChunk(t, conv, "second result\n")
	for _, event := range drainReentryEvents(t, conv) {
		if event.Kind == ports.ChatEventTurnCompleted {
			t.Fatalf("reentry turn settled while output was still flowing: %#v", event)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	var completed *ports.ChatEvent
	for completed == nil {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatal("reentry turn never settled after the provider went quiet")
		}
		select {
		case event := <-conv.Events():
			if event.Kind == ports.ChatEventTurnCompleted {
				completed = &event
			}
		case <-time.After(remaining):
		}
	}
	if completed.ProviderTurnID != turnID || completed.TurnState != domain.TurnStateCompleted {
		t.Fatalf("quiet settle = %#v, want completed turn %q", completed, turnID)
	}
	conv.mu.Lock()
	active := conv.activeTurn
	conv.mu.Unlock()
	if active != "" {
		t.Fatalf("activeTurn = %q after quiet settle, want released", active)
	}

	// The session accepts a dispatched turn again.
	if _, err := conv.SendTurn(context.Background(), ports.ChatUserMessage{Text: "next round"}); err != nil {
		t.Fatalf("SendTurn after reentry settle: %v", err)
	}
}

// TestACPReentryTurnInterruptSettlesLocally verifies a user interrupt of an
// open reentry turn settles it locally — AO owns no prompt on the wire for
// provider-initiated work — and does not wedge the session busy.
func TestACPReentryTurnInterruptSettlesLocally(t *testing.T) {
	conv := newReentryTestConversation(t)
	reentryChunk(t, conv, "partial result\n")
	_ = drainReentryEvents(t, conv)
	conv.mu.Lock()
	turnID := conv.reentryTurn
	conv.mu.Unlock()
	if turnID == "" {
		t.Fatal("no reentry turn to interrupt")
	}

	if err := conv.Interrupt(context.Background(), turnID); err != nil {
		t.Fatalf("Interrupt reentry turn: %v", err)
	}
	var completed *ports.ChatEvent
	for _, event := range drainReentryEvents(t, conv) {
		if event.Kind == ports.ChatEventTurnCompleted {
			completed = &event
		}
	}
	if completed == nil || completed.ProviderTurnID != turnID ||
		completed.TurnState != domain.TurnStateInterrupted {
		t.Fatalf("interrupted reentry turn = %#v", completed)
	}
	conv.mu.Lock()
	active := conv.activeTurn
	conv.mu.Unlock()
	if active != "" {
		t.Fatalf("activeTurn = %q after interrupt, want released", active)
	}
}

// TestACPReentryDoesNotAdoptPreparedOrInTurnOutput keeps the adoption scoped:
// output that belongs to a dispatched turn (prepared or running) must never
// mint a second provider turn.
func TestACPReentryDoesNotAdoptPreparedOrInTurnOutput(t *testing.T) {
	conv := newReentryTestConversation(t)

	// In-turn output: attributed to the running turn, no reentry adoption.
	conv.mu.Lock()
	conv.activeTurn = "turn-1"
	conv.mu.Unlock()
	reentryChunk(t, conv, "in-turn output\n")
	conv.mu.Lock()
	conv.activeTurn = ""
	conv.mu.Unlock()
	for _, event := range drainReentryEvents(t, conv) {
		if event.Kind == ports.ChatEventTurnStarted {
			t.Fatalf("in-turn output started a reentry turn: %#v", event)
		}
	}

	// Prepared (dispatch pending) output: no adoption either.
	ref, err := conv.SendTurn(context.Background(), ports.ChatUserMessage{Text: "queued work"})
	if err != nil {
		t.Fatal(err)
	}
	reentryChunk(t, conv, "stray chunk during dispatch\n")
	for _, event := range drainReentryEvents(t, conv) {
		if event.Kind == ports.ChatEventTurnStarted && event.ProviderTurnID != ref.ProviderTurnID {
			t.Fatalf("output during dispatch adopted a reentry turn: %#v", event)
		}
	}
	conv.mu.Lock()
	reentry := conv.reentryTurn
	conv.mu.Unlock()
	if reentry != "" {
		t.Fatalf("reentry turn adopted while a dispatch was pending: %q", reentry)
	}
	conv.DiscardDeferredTurn(ref.ProviderTurnID)
}

// TestACPReentryToolOutputKeepsTurnOpen verifies tool-call-shaped reentry
// traffic — hermes streams background subagent tool calls out of turn too — is
// attributed to the adopted turn.
func TestACPReentryToolOutputKeepsTurnOpen(t *testing.T) {
	conv := newReentryTestConversation(t)

	reentryChunk(t, conv, "continuation begins\n")
	var turnID string
	for _, event := range drainReentryEvents(t, conv) {
		if event.Kind == ports.ChatEventTurnStarted {
			turnID = event.ProviderTurnID
		}
	}
	if turnID == "" {
		t.Fatal("reentry chunk did not adopt a turn")
	}

	if err := conv.processUpdate(acpsdk.SessionNotification{
		SessionId: acpsdk.SessionId(conv.ProviderConversationID()),
		Update: acpsdk.SessionUpdate{ToolCall: &acpsdk.SessionUpdateToolCall{
			SessionUpdate: "tool_call", ToolCallId: "bg-tool", Title: "Summarize findings",
			Kind: acpsdk.ToolKindExecute, Status: acpsdk.ToolCallStatusPending,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	var toolEvent *ports.ChatEvent
	for _, event := range drainReentryEvents(t, conv) {
		if event.Kind == ports.ChatEventActivityStarted && event.ActivityKind == domain.ActivityKindCommand {
			event := event
			toolEvent = &event
		}
	}
	if toolEvent == nil {
		t.Fatal("reentry tool call produced no activity")
	}
	if toolEvent.ProviderTurnID != turnID {
		t.Fatalf("reentry tool activity attributed to %q, want %q", toolEvent.ProviderTurnID, turnID)
	}
}
