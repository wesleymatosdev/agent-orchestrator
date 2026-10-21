package acp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestACPDriverUsesProviderHibernationCheck(t *testing.T) {
	for _, configured := range []bool{false, true} {
		cfg := Config{Launch: func(context.Context, LaunchConfig) (Launch, error) { return Launch{Command: "fake"}, nil }}
		var gotID acpsdk.SessionId
		ready := false
		var checkErr error
		if configured {
			cfg.HistoryNotice = func(string) bool { return true }
			cfg.CanHibernate = func(_ context.Context, _ *acpsdk.ClientSideConnection, id acpsdk.SessionId) (bool, error) {
				gotID = id
				return ready, checkErr
			}
		}
		d := New(cfg, nil)
		d.useTestProcess(fakeSpawn(&fakeAgent{}))
		conv, err := d.Start(context.Background(), ports.ChatStartConfig{WorkspacePath: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		if (conv.(*conversation).historyNotice != nil) != configured {
			t.Fatalf("configured=%v history notice not wired", configured)
		}
		h := conv.(ports.ChatProviderHibernator)
		canSleep, err := h.CanHibernate(context.Background())
		if err != nil || canSleep == configured {
			t.Fatalf("configured=%v CanHibernate=%v, %v", configured, canSleep, err)
		}
		if configured {
			if string(gotID) != conv.ProviderConversationID() {
				t.Fatalf("check session ID=%q, want %q", gotID, conv.ProviderConversationID())
			}
			ready = true
			if canSleep, err = h.CanHibernate(context.Background()); err != nil || !canSleep {
				t.Fatalf("ended task CanHibernate=%v, %v", canSleep, err)
			}
			checkErr = errors.New("native registry unavailable")
			if _, err = h.CanHibernate(context.Background()); !errors.Is(err, checkErr) {
				t.Fatalf("native check error=%v, want %v", err, checkErr)
			}
		}
		_ = conv.Close()
	}
}

// newHibernationTestConversation starts one fake provider conversation for the
// hibernation-gate tests.
func newHibernationTestConversation(t *testing.T) *conversation {
	t.Helper()
	cfg := Config{Launch: func(context.Context, LaunchConfig) (Launch, error) { return Launch{Command: "fake"}, nil }}
	d := New(cfg, nil)
	d.useTestProcess(fakeSpawn(&fakeAgent{}))
	conv, err := d.Start(context.Background(), ports.ChatStartConfig{WorkspacePath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conv.Close() })
	return conv.(*conversation)
}

// TestACPConversationDefersHibernationBehindProviderInitiatedOutput pins the
// #15 reentry lease: output the provider produces with no AO turn in flight is
// detached work re-entering the session, and hibernation must wait for it to
// go quiet instead of severing the delivery.
func TestACPConversationDefersHibernationBehindProviderInitiatedOutput(t *testing.T) {
	conv := newHibernationTestConversation(t)
	hibernator := ports.ChatProviderHibernator(conv)

	if canSleep, err := hibernator.CanHibernate(context.Background()); err != nil || !canSleep {
		t.Fatalf("idle conversation CanHibernate=%v, %v; want hibernatable", canSleep, err)
	}

	notification := acpsdk.SessionNotification{
		SessionId: acpsdk.SessionId(conv.ProviderConversationID()),
		Update:    acpsdk.UpdateAgentMessageText("background reviewer finished"),
	}
	if err := conv.processUpdate(notification); err != nil {
		t.Fatal(err)
	}
	conv.mu.Lock()
	leased := !conv.providerInitiatedOutputAt.IsZero()
	reentry := conv.reentryTurn
	conv.mu.Unlock()
	if !leased || reentry == "" {
		t.Fatalf("post-turn provider output adopted=%q lease-armed=%v; want an open reentry turn", reentry, leased)
	}
	if canSleep, err := hibernator.CanHibernate(context.Background()); err != nil || canSleep {
		t.Fatalf("CanHibernate right after provider-initiated output=%v, %v; want false", canSleep, err)
	}

	// The lease ends with the adopted turn: once the reentry stream settles,
	// the session is hibernatable again without waiting out a timer.
	conv.finishReentryTurn(domain.TurnStateCompleted)
	if canSleep, err := hibernator.CanHibernate(context.Background()); err != nil || !canSleep {
		t.Fatalf("CanHibernate after the reentry turn settled=%v, %v; want hibernatable", canSleep, err)
	}

	// Output that cannot be adopted (a dispatch pending when it arrives) still
	// arms the quiet-time lease on its own.
	conv.mu.Lock()
	conv.providerInitiatedOutputAt = time.Now()
	conv.mu.Unlock()
	if canSleep, err := hibernator.CanHibernate(context.Background()); err != nil || canSleep {
		t.Fatalf("CanHibernate with a fresh unadopted lease=%v, %v; want false", canSleep, err)
	}
	conv.mu.Lock()
	conv.providerInitiatedOutputAt = time.Now().Add(-providerInitiatedQuietLimit)
	conv.mu.Unlock()
	if canSleep, err := hibernator.CanHibernate(context.Background()); err != nil || !canSleep {
		t.Fatalf("CanHibernate after the quiet limit=%v, %v; want hibernatable", canSleep, err)
	}
}

// TestACPConversationTurnOutputDoesNotArmReentryLease keeps the lease scoped to
// provider-initiated output: chunks belonging to an AO-dispatched turn are
// ordinary turn traffic and must not keep the provider awake afterwards.
func TestACPConversationTurnOutputDoesNotArmReentryLease(t *testing.T) {
	conv := newHibernationTestConversation(t)

	conv.mu.Lock()
	conv.activeTurn = "turn-1"
	conv.mu.Unlock()
	notification := acpsdk.SessionNotification{
		SessionId: acpsdk.SessionId(conv.ProviderConversationID()),
		Update:    acpsdk.UpdateAgentMessageText("in-turn output"),
	}
	if err := conv.processUpdate(notification); err != nil {
		t.Fatal(err)
	}
	conv.mu.Lock()
	armed := !conv.providerInitiatedOutputAt.IsZero()
	conv.activeTurn = ""
	conv.mu.Unlock()
	if armed {
		t.Fatal("output belonging to an AO turn armed the reentry lease")
	}
	if canSleep, err := ports.ChatProviderHibernator(conv).CanHibernate(context.Background()); err != nil || !canSleep {
		t.Fatalf("CanHibernate=%v, %v; want hibernatable", canSleep, err)
	}
}

// TestACPConversationNewTurnClearsReentryLease proves a new AO-dispatched turn
// supersedes earlier provider-initiated output: the lease is cleared when the
// turn actually starts, so it never outlives the evidence it was built from.
func TestACPConversationNewTurnClearsReentryLease(t *testing.T) {
	conv := newHibernationTestConversation(t)

	conv.mu.Lock()
	conv.providerInitiatedOutputAt = time.Now()
	conv.mu.Unlock()
	if canSleep, err := ports.ChatProviderHibernator(conv).CanHibernate(context.Background()); err != nil || canSleep {
		t.Fatalf("CanHibernate=%v, %v; want false while the lease is fresh", canSleep, err)
	}

	turn, err := conv.SendTurn(context.Background(), ports.ChatUserMessage{Text: "next round"})
	if err != nil {
		t.Fatal(err)
	}
	if err := conv.StartDeferredTurn(turn.ProviderTurnID); err != nil {
		t.Fatal(err)
	}
	conv.mu.Lock()
	armed := !conv.providerInitiatedOutputAt.IsZero()
	conv.mu.Unlock()
	if armed {
		t.Fatal("new turn start left the reentry lease armed")
	}
	_ = conv.Interrupt(context.Background(), turn.ProviderTurnID)
}
