package sessionmanager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type fakeHermesFork struct {
	gotDataDir, gotJobID, gotRecorded, gotPrompt string
	gotTurnsAfter                                int
	child                                        string
	forkErr                                      error
	noted                                        []string
}

func (f *fakeHermesFork) ForkAtCut(
	ctx context.Context, dataDir, aoSessionID, recordedAgentSessionID, cutPrompt string, turnsAfterInclusive int,
) (string, error) {
	if f.forkErr != nil {
		return "", f.forkErr
	}
	f.gotDataDir, f.gotJobID, f.gotRecorded, f.gotPrompt, f.gotTurnsAfter =
		dataDir, aoSessionID, recordedAgentSessionID, cutPrompt, turnsAfterInclusive
	if f.child == "" {
		f.child = "fork-child"
	}
	return f.child, nil
}

func (f *fakeHermesFork) RecordRollbackNote(dataDir, aoSessionID, childID string, droppedTurns int) error {
	dir := filepath.Join(dataDir, "agents", "hermes", aoSessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f.noted = append(f.noted, childID)
	return os.WriteFile(filepath.Join(dir, "live-head"), []byte(childID), 0o600)
}

func rollbackFixture(t *testing.T, mutate func(*domain.SessionRecord)) (*Manager, *fakeStore, *fakeHermesFork) {
	t.Helper()
	fork := &fakeHermesFork{}
	m, st, _ := newChatManager(&recordingLauncher{})
	m.hermesFork = fork
	m.dataDir = t.TempDir()
	st.rollbackTurnsDiscarded = 3
	seedChatResumeSession(st, domain.ActivityIdle)
	rec := st.sessions["mer-1"]
	rec.Harness = domain.HarnessHermes
	rec.Metadata.AgentSessionID = "native-1"
	if mutate != nil {
		mutate(&rec)
	}
	st.sessions["mer-1"] = rec
	st.conversations["mer-1"] = domain.ConversationRecord{ID: "conv-1"}
	return m, st, fork
}

func TestRollbackHermesToForkForksDiscardsAndRePoints(t *testing.T) {
	m, st, fork := rollbackFixture(t, nil)

	discarded, err := m.RollbackHermesToFork(context.Background(), "mer-1", "turn-9", "second ask", 2)
	if err != nil {
		t.Fatalf("RollbackHermesToFork: %v", err)
	}
	if discarded != 3 {
		t.Fatalf("discarded = %d", discarded)
	}
	if fork.gotDataDir != m.dataDir || fork.gotJobID != "mer-1" || fork.gotRecorded != "native-1" ||
		fork.gotPrompt != "second ask" || fork.gotTurnsAfter != 2 {
		t.Fatalf("fork args: %+v", fork)
	}
	rec := st.sessions["mer-1"]
	if rec.Metadata.AgentSessionID != "fork-child" || rec.Metadata.ProviderConversationID != "fork-child" {
		t.Fatalf("provider identity not re-pointed: %+v", rec.Metadata)
	}
	if rec.Metadata.AgentSessionIDLaunchID != "" ||
		rec.Metadata.ConversationCheckpointNativeID != "" ||
		rec.Metadata.ConversationCheckpointTurnID != "" {
		t.Fatalf("stale identity/checkpoint facts survived: %+v", rec.Metadata)
	}
	if len(st.rollbackTurns) != 1 || st.rollbackTurns[0].conversationID != "conv-1" || st.rollbackTurns[0].turnID != "turn-9" {
		t.Fatalf("RollbackTurns calls: %+v", st.rollbackTurns)
	}
	note, err := os.ReadFile(filepath.Join(m.dataDir, "agents", "hermes", "mer-1", "live-head"))
	if err != nil || string(note) != "fork-child" {
		t.Fatalf("rollback note: %q err=%v", note, err)
	}
}

func TestRollbackHermesToForkParksALiveController(t *testing.T) {
	m, _, fork := rollbackFixture(t, nil)
	hibernator := &recordingHibernator{recordingLauncher: &recordingLauncher{live: true}}
	m.chat = hibernator

	if _, err := m.RollbackHermesToFork(context.Background(), "mer-1", "turn-9", "ask", 1); err != nil {
		t.Fatalf("RollbackHermesToFork: %v", err)
	}
	if len(hibernator.calls) != 1 || hibernator.calls[0] != domain.SessionID("mer-1") {
		t.Fatalf("hibernated %v, want [mer-1]", hibernator.calls)
	}
	if fork.gotTurnsAfter != 1 {
		t.Fatalf("fork ran with turnsAfter=%d; want the park to precede it", fork.gotTurnsAfter)
	}
}

func TestRollbackHermesToForkRefusesWhenTheHostSurvivesParking(t *testing.T) {
	m, _, fork := rollbackFixture(t, nil)
	hibernator := &recordingHibernator{recordingLauncher: &recordingLauncher{live: true}}
	m.chat = hibernator
	// A launcher whose controller stays live after the park: work in flight
	// kept the host running, so the fork must not proceed.
	m.chat = &hostAliveLauncher{hibernator}

	_, err := m.RollbackHermesToFork(context.Background(), "mer-1", "turn-9", "ask", 1)
	if !errors.Is(err, ErrRollbackHostAlive) {
		t.Fatalf("err = %v, want ErrRollbackHostAlive", err)
	}
	if fork.gotTurnsAfter != 0 {
		t.Fatal("the fork must not run while the host is alive")
	}
}

// hostAliveLauncher keeps reporting a live controller after hibernation.
type hostAliveLauncher struct {
	*recordingHibernator
}

func (l *hostAliveLauncher) HasLiveChatController(domain.SessionID) bool { return true }

func TestRollbackHermesToForkRefusesNonHermesAndBusySessions(t *testing.T) {
	for name, mutate := range map[string]func(*domain.SessionRecord){
		"codex":     func(rec *domain.SessionRecord) { rec.Harness = domain.HarnessCodex },
		"tui":       func(rec *domain.SessionRecord) { rec.Mode = domain.SessionModeTUI },
		"no-native": func(rec *domain.SessionRecord) { rec.Metadata.AgentSessionID = "" },
		"active":    func(rec *domain.SessionRecord) { rec.Activity.State = domain.ActivityActive },
		"blocked":   func(rec *domain.SessionRecord) { rec.Activity.State = domain.ActivityBlocked },
	} {
		m, _, _ := rollbackFixture(t, mutate)
		_, err := m.RollbackHermesToFork(context.Background(), "mer-1", "turn-9", "ask", 1)
		want := ErrRollbackNotApplicable
		if name == "active" || name == "blocked" {
			want = ErrRollbackBusy
		}
		if !errors.Is(err, want) {
			t.Fatalf("%s: err = %v, want %v", name, err, want)
		}
	}
}

func TestRollbackHermesToForkNeedsTheForkCapabilityAndStopsOnForkFailure(t *testing.T) {
	m, _, _ := rollbackFixture(t, nil)
	m.hermesFork = nil
	if _, err := m.RollbackHermesToFork(context.Background(), "mer-1", "turn-9", "ask", 1); !errors.Is(err, ErrRollbackUnavailable) {
		t.Fatalf("err = %v, want ErrRollbackUnavailable", err)
	}

	m2, _, fork := rollbackFixture(t, nil)
	fork.forkErr = errors.New("cut unresolved")
	if _, err := m2.RollbackHermesToFork(context.Background(), "mer-1", "turn-9", "ask", 1); err == nil {
		t.Fatal("fork failure must surface")
	}
}
