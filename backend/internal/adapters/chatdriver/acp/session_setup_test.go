package acp

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestNormalizeAdditionalDirectories(t *testing.T) {
	root := string(filepath.Separator) + filepath.Join("ws", "root")
	nestedA := filepath.Join(root, "api")
	nestedB := filepath.Join(root, "migrator")
	outside := string(filepath.Separator) + filepath.Join("elsewhere", "repo")

	t.Run("no directories is a no-op regardless of support", func(t *testing.T) {
		for _, supported := range []bool{false, true} {
			dirs, dropped, err := normalizeAdditionalDirectories(root, nil, supported)
			if err != nil || dirs != nil || dropped != 0 {
				t.Fatalf("supported=%v: (%#v, %d, %v)", supported, dirs, dropped, err)
			}
		}
	})

	t.Run("supported passes every unique directory through", func(t *testing.T) {
		dirs, dropped, err := normalizeAdditionalDirectories(root, []string{nestedA, nestedB, nestedA, root}, true)
		if err != nil || dropped != 0 {
			t.Fatalf("unexpected result: (%v, %d, %v)", err, dropped, err)
		}
		// The cwd itself is deduplicated away; nested dirs stay in order.
		if len(dirs) != 2 || dirs[0] != nestedA || dirs[1] != nestedB {
			t.Fatalf("dirs = %#v", dirs)
		}
	})

	t.Run("unsupported drops under-cwd directories without failing", func(t *testing.T) {
		dirs, dropped, err := normalizeAdditionalDirectories(root, []string{nestedA, nestedB, nestedA}, false)
		if err != nil {
			t.Fatalf("under-cwd directories must not fail a non-advertising agent: %v", err)
		}
		if dirs != nil {
			t.Fatalf("dirs = %#v, want nil", dirs)
		}
		if dropped != 2 {
			t.Fatalf("dropped = %d, want 2 (unique under-cwd directories)", dropped)
		}
	})

	t.Run("unsupported still fails for directories outside the cwd", func(t *testing.T) {
		_, _, err := normalizeAdditionalDirectories(root, []string{nestedA, outside}, false)
		if err == nil || !strings.Contains(err.Error(), outside) {
			t.Fatalf("err = %v, want outside directory %q named", err, outside)
		}
		if strings.Contains(err.Error(), nestedA) {
			t.Fatalf("err should not blame the under-cwd directory: %v", err)
		}
	})

	t.Run("relative directories still fail", func(t *testing.T) {
		if _, _, err := normalizeAdditionalDirectories(root, []string{"api"}, true); err == nil ||
			!strings.Contains(err.Error(), "must be absolute") {
			t.Fatalf("err = %v", err)
		}
	})
}

// TestACPDriverStartDropsUnderCwdAdditionalDirectoriesForNonAdvertisingAgent
// is the workspace-project regression: hermes-style agents do not advertise
// the additional_directories session capability, and AO workspace projects
// only ever pass child worktrees created under the session workspace root.
// Those requests must degrade (the cwd already covers them) instead of
// failing the session with CHAT_CONTROLLER_FAILED.
func TestACPDriverStartDropsUnderCwdAdditionalDirectoriesForNonAdvertisingAgent(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "api")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	agent := &fakeAgent{}
	driver := New(Config{
		Harness: domain.HarnessHermes,
		Capabilities: ports.ChatCapabilities{
			ports.ChatCapabilityDiffs: true,
		},
		Probe: func(context.Context) error { return nil },
		Launch: func(context.Context, LaunchConfig) (Launch, error) {
			return Launch{Command: "fake"}, nil
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	driver.useTestProcess(fakeSpawn(agent))

	conv, err := driver.Start(t.Context(), ports.ChatStartConfig{
		SessionID:             "mr-hermes",
		WorkspacePath:         root,
		AdditionalDirectories: []string{nested},
	})
	if err != nil {
		t.Fatalf("Start with under-cwd additional directories: %v", err)
	}
	defer conv.Close()
	_ = nextEvent(t, conv.Events())

	agent.mu.Lock()
	newParams := agent.newParams
	agent.mu.Unlock()
	if len(newParams.AdditionalDirectories) != 0 {
		t.Fatalf("session/new additional directories = %#v, want none sent", newParams.AdditionalDirectories)
	}
}

// TestACPDriverStartStillFailsOnOutsideAdditionalDirectoriesWithoutSupport
// keeps the capability contract for directories the cwd genuinely does not
// cover: a non-advertising agent plus an external directory must still fail.
func TestACPDriverStartStillFailsOnOutsideAdditionalDirectoriesWithoutSupport(t *testing.T) {
	agent := &fakeAgent{}
	driver := New(Config{
		Harness: domain.HarnessHermes,
		Probe:   func(context.Context) error { return nil },
		Launch: func(context.Context, LaunchConfig) (Launch, error) {
			return Launch{Command: "fake"}, nil
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	driver.useTestProcess(fakeSpawn(agent))

	_, err := driver.Start(t.Context(), ports.ChatStartConfig{
		SessionID:             "mr-hermes",
		WorkspacePath:         t.TempDir(),
		AdditionalDirectories: []string{t.TempDir()},
	})
	if err == nil || !strings.Contains(err.Error(), "does not support additional workspace directories") {
		t.Fatalf("Start err = %v, want the additional-directories failure", err)
	}
}

// TestACPDriverResumeDropsUnderCwdAdditionalDirectoriesForNonAdvertisingAgent
// covers the session/resume path (driver.resume), which shares the normalize
// helper with Start.
func TestACPDriverResumeDropsUnderCwdAdditionalDirectoriesForNonAdvertisingAgent(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "api")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	agent := &fakeAgent{}
	driver := New(Config{
		Harness: domain.HarnessHermes,
		Probe:   func(context.Context) error { return nil },
		Launch: func(context.Context, LaunchConfig) (Launch, error) {
			return Launch{Command: "fake"}, nil
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	driver.useTestProcess(fakeSpawn(agent))

	conv, err := driver.Resume(t.Context(), ports.ChatResumeConfig{
		SessionID:              "mr-hermes",
		WorkspacePath:          root,
		ProviderConversationID: "provider-session-1",
		AdditionalDirectories:  []string{nested},
	})
	if err != nil {
		t.Fatalf("Resume with under-cwd additional directories: %v", err)
	}
	defer conv.Close()

	agent.mu.Lock()
	resumeParams := agent.resumeParams
	agent.mu.Unlock()
	if len(resumeParams.AdditionalDirectories) != 0 {
		t.Fatalf("session/resume additional directories = %#v, want none sent", resumeParams.AdditionalDirectories)
	}
}

// TestACPIsolatedForkDropsUnderCwdAdditionalDirectoriesForNonAdvertisingAgent
// covers the fork path, which normalizes the same list before session/fork.
func TestACPIsolatedForkDropsUnderCwdAdditionalDirectoriesForNonAdvertisingAgent(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "api")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	agent := &fakeAgent{forkID: "child", capabilities: &acpsdk.AgentCapabilities{
		SessionCapabilities: acpsdk.SessionCapabilities{
			Fork:   &acpsdk.SessionForkCapabilities{},
			Resume: &acpsdk.SessionResumeCapabilities{},
		},
	}}
	driver := forkTestDriver(agent)
	cfg := ports.ChatStartConfig{
		SessionID:             "side-host",
		WorkspacePath:         root,
		DataDir:               t.TempDir(),
		Permissions:           ports.PermissionModeDefault,
		ProviderScopeID:       "side",
		AdditionalDirectories: []string{nested},
	}
	child, err := driver.ForkIntoHost(t.Context(), "owned:parent", "", cfg)
	if err != nil {
		t.Fatalf("ForkIntoHost with under-cwd additional directories: %v", err)
	}
	defer func() { _ = child.Close() }()

	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.forkParams.AdditionalDirectories) != 0 {
		t.Fatalf("session/fork additional directories = %#v, want none sent", agent.forkParams.AdditionalDirectories)
	}
}

// TestACPIsolatedForkStillFailsOnOutsideAdditionalDirectoriesWithoutSupport
// pins the outside-cwd half of the contract on the fork path.
func TestACPIsolatedForkStillFailsOnOutsideAdditionalDirectoriesWithoutSupport(t *testing.T) {
	agent := &fakeAgent{forkID: "child", capabilities: &acpsdk.AgentCapabilities{
		SessionCapabilities: acpsdk.SessionCapabilities{
			Fork:   &acpsdk.SessionForkCapabilities{},
			Resume: &acpsdk.SessionResumeCapabilities{},
		},
	}}
	driver := forkTestDriver(agent)
	_, err := driver.ForkIntoHost(t.Context(), "owned:parent", "", ports.ChatStartConfig{
		SessionID:             "side-host",
		WorkspacePath:         t.TempDir(),
		DataDir:               t.TempDir(),
		Permissions:           ports.PermissionModeDefault,
		ProviderScopeID:       "side",
		AdditionalDirectories: []string{t.TempDir()},
	})
	if err == nil || !strings.Contains(err.Error(), "does not support additional workspace directories") {
		t.Fatalf("ForkIntoHost err = %v, want the additional-directories failure", err)
	}
	if agent.forkCalls != 0 {
		t.Fatal("sent an unsupported fork request")
	}
}
