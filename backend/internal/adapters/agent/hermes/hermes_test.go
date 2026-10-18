package hermes

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestStandingInstructionsPreserveNativeOverlay(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERMES_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("agent:\n  system_prompt: Native profile instructions\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"HERMES_EPHEMERAL_SYSTEM_PROMPT": "Native invocation instructions"}
	cfg := ports.WorkspaceHookConfig{DataDir: t.TempDir(), SessionID: "ao-1", Env: env, SystemPrompt: "AO standing instructions"}
	if err := New().GetAgentHooks(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if got := env["HERMES_EPHEMERAL_SYSTEM_PROMPT"]; got != "Native invocation instructions\n\nAO standing instructions" {
		t.Fatalf("got %q", got)
	}
	if err := New().GetAgentHooks(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if strings.Count(env["HERMES_EPHEMERAL_SYSTEM_PROMPT"], "AO standing instructions") != 1 {
		t.Fatal("overlay duplicated")
	}
	env = map[string]string{}
	cfg.Env = env
	if err := New().GetAgentHooks(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(env["HERMES_EPHEMERAL_SYSTEM_PROMPT"], "Native profile instructions") {
		t.Fatal("lost native profile")
	}
}
func TestExpansionInUnrelatedConfigKeysDoesNotBlockLaunch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERMES_HOME", home)
	config := "mcp_servers:\n  rebase:\n    headers:\n      Authorization: Bearer ${MCP_REBASE_KEY}\nagent:\n  system_prompt: Native profile instructions\n"
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	cfg := ports.WorkspaceHookConfig{DataDir: t.TempDir(), SessionID: "ao-1", Env: env, SystemPrompt: "AO standing instructions"}
	if err := New().GetAgentHooks(context.Background(), cfg); err != nil {
		t.Fatalf("unrelated ${...} must not block launch: %v", err)
	}
}

func TestExpansionInInstructionKeysStillRefused(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERMES_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("agent:\n  system_prompt: expanded ${MY_PROMPT_VAR}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	cfg := ports.WorkspaceHookConfig{DataDir: t.TempDir(), SessionID: "ao-1", Env: env, SystemPrompt: "AO standing instructions"}
	if err := New().GetAgentHooks(context.Background(), cfg); err == nil {
		t.Fatal("expansion inside agent instructions must stay refused")
	}
}
func TestRestoreRefusesUnobservedIdentity(t *testing.T) {
	for _, id := range []string{"", "latest", "some title", "../../state.db"} {
		_, ok, err := New().GetRestoreCommand(context.Background(), ports.RestoreConfig{Session: ports.SessionRef{Metadata: map[string]string{ports.MetadataKeyAgentSessionID: id}}})
		if ok || err == nil {
			t.Fatalf("id %q: ok=%v err=%v", id, ok, err)
		}
	}
}
func TestLaunchRejectsUnsupportedPermissionsAndRestrictions(t *testing.T) {
	for _, cfg := range []ports.LaunchConfig{{Permissions: ports.PermissionModeAcceptEdits}, {AllowedTools: []string{"read"}}} {
		if _, err := New().GetLaunchCommand(context.Background(), cfg); err == nil {
			t.Fatal("unsupported control accepted")
		}
	}
	// Fork: "auto" is the standalone-spawn default and maps to hermes' native
	// approval flow, so it must be accepted.
	if _, err := New().GetLaunchCommand(context.Background(), ports.LaunchConfig{Permissions: ports.PermissionModeAuto}); err != nil {
		t.Fatalf("auto mode rejected: %v", err)
	}
}
func TestActivityCurrentComposerAndDecision(t *testing.T) {
	for _, tt := range []struct{ screen, want string }{{"☤ ❯ msg=interrupt · /queue · Ctrl+C cancel", "active"}, {"☤ ❯ \n", "idle"}, {"☤ ❯ \nAllow this command?\n❯ [1] Allow once", "blocked"}, {"unrelated text", ""}} {
		state, ok := New().DetectTerminalActivity(tt.screen)
		if string(state) != tt.want || ok != (tt.want != "") {
			t.Fatalf("%q got %s/%v", tt.screen, state, ok)
		}
	}
}

func TestLeadingDashTaskStaysInPrivateQueryFile(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "hermes"), []byte("#!/bin/sh\necho 'Hermes Agent v0.22.2+948'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	data := t.TempDir()
	cmd, err := New().GetLaunchCommand(context.Background(), ports.LaunchConfig{DataDir: data, SessionID: "ao-1", WorkspacePath: "/workspace", Prompt: "--yolo is task data"})
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range cmd {
		if arg == "--yolo" {
			t.Fatal("task became option")
		}
	}
	b, err := os.ReadFile(filepath.Join(data, "agents", "hermes", "ao-1", "initial-query.txt"))
	if err != nil || string(b) != "--yolo is task data" {
		t.Fatalf("query=%q err=%v", b, err)
	}
}
func TestPersonalityPreservesNativePrecedence(t *testing.T) {
	overlay := nativeOverlay(map[string]any{"display": map[string]any{"personality": "technical"}, "agent": map[string]any{"system_prompt": "ignored native manual overlay"}})
	if overlay != builtinPersonalities["technical"] {
		t.Fatalf("overlay=%q", overlay)
	}
	overlay = nativeOverlay(map[string]any{"display": map[string]any{"personality": "custom"}, "personalities": map[string]any{"custom": "first"}, "agent": map[string]any{"personalities": map[string]any{"custom": map[string]any{"system_prompt": "native", "tone": "gentle"}}}})
	if overlay != "native\nTone: gentle" {
		t.Fatalf("overlay=%q", overlay)
	}
}

func TestNativeModalChromeBlocks(t *testing.T) {
	for _, screen := range []string{"Dangerous Command\n❯ 1. Allow once\n⚠ ❯", "? ❯"} {
		state, ok := New().DetectTerminalActivity(screen)
		if !ok || state != "blocked" {
			t.Fatalf("modal admitted: %q %s", screen, state)
		}
	}
}
func TestStickyProfileFailsClosed(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "active_profile"), []byte("work"), 0o600); err != nil {
		t.Fatal(err)
	}
	if New().GetAgentHooks(context.Background(), ports.WorkspaceHookConfig{DataDir: t.TempDir(), SessionID: "test", Env: map[string]string{"HERMES_HOME": home}, SystemPrompt: "AO"}) == nil {
		t.Fatal("sticky profile silently overridden")
	}
}

func TestNativeItalicPlaceholderAndDraft(t *testing.T) {
	state, ok := New().DetectTerminalActivity("────────────────────────────────────────\n❯ \x1b[38;5;136;3mPlan a feature\x1b[m\n────────────────────────────────────────")
	if !ok || state != "idle" {
		t.Fatalf("native placeholder %s %v", state, ok)
	}
	if _, ok := New().DetectTerminalActivity("────────────────────────────────────────\n❯ Plan a feature\n────────────────────────────────────────"); ok {
		t.Fatal("human draft accepted")
	}
}

func TestRestoreRefusesNativeRotationAfterObservation(t *testing.T) {
	data := t.TempDir()
	dir := sessionDir(data, "ao-rotation")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "native-home"), []byte(home), 0o600); err != nil {
		t.Fatal(err)
	}
	crumb := filepath.Join(home, "native-terminal-fixture")
	if err := os.WriteFile(crumb, []byte(`{"session_id":"new-conversation","cwd":"/workspace","ts":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "observed-terminal"), []byte(crumb), 0o600); err != nil {
		t.Fatal(err)
	}
	_, ok, err := New().GetRestoreCommand(context.Background(), ports.RestoreConfig{DataDir: data, Session: ports.SessionRef{ID: "ao-rotation", WorkspacePath: "/workspace", Metadata: map[string]string{ports.MetadataKeyAgentSessionID: "old-conversation"}}})
	if err == nil || ok || !strings.Contains(err.Error(), "rotated") {
		t.Fatalf("stale conversation restore: %v %v", ok, err)
	}
}

func TestRestoreResolvesDarwinTerminalCrumbWithoutObservationFile(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin terminal-sessions crumb fallback")
	}
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "hermes"), []byte("#!/bin/sh\necho 'Hermes Agent v0.22.2+948'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	workspace := t.TempDir()
	home := t.TempDir()
	data := t.TempDir()
	dir := sessionDir(data, "ao-darwin-restore")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "native-home"), []byte(home), 0o600); err != nil {
		t.Fatal(err)
	}
	crumbDir := filepath.Join(home, "terminal-sessions")
	if err := os.MkdirAll(crumbDir, 0o700); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(crumbDir, "tty-dev-ttys009")
	if err := os.WriteFile(current, []byte(`{"session_id":"current-conversation","cwd":"`+workspace+`","ts":20}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Same native session observed from another workspace, older timestamp:
	// must not outrank the workspace-scoped crumb.
	if err := os.WriteFile(filepath.Join(crumbDir, "tty-dev-ttys008"), []byte(`{"session_id":"current-conversation","cwd":"/elsewhere","ts":10}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(crumbDir, "tty-dev-ttys007"), []byte(`{"session_id":"other-conversation","cwd":"`+workspace+`","ts":30}`), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(), "CREATE TABLE sessions(id TEXT PRIMARY KEY, cwd TEXT, parent_session_id TEXT); CREATE TABLE messages(session_id TEXT); INSERT INTO sessions VALUES('current-conversation', ?, NULL); INSERT INTO messages VALUES('current-conversation')", workspace); err != nil {
		t.Fatal(err)
	}
	cmd, ok, err := New().GetRestoreCommand(context.Background(), ports.RestoreConfig{DataDir: data, Session: ports.SessionRef{ID: "ao-darwin-restore", WorkspacePath: workspace, Metadata: map[string]string{ports.MetadataKeyAgentSessionID: "current-conversation"}}})
	if err != nil || !ok {
		t.Fatalf("darwin crumb restore: %v %v", ok, err)
	}
	if cmd[len(cmd)-2] != "--resume" || cmd[len(cmd)-1] != "current-conversation" {
		t.Fatalf("resume argv tail: %v", cmd)
	}
	observed, readErr := os.ReadFile(filepath.Join(dir, "observed-terminal"))
	if readErr != nil || strings.TrimSpace(string(observed)) != current {
		t.Fatalf("observed-terminal=%q err=%v", observed, readErr)
	}

	// A session whose native id has no terminal crumb stays fail-closed.
	data = t.TempDir()
	dir = sessionDir(data, "ao-darwin-missing")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "native-home"), []byte(home), 0o600); err != nil {
		t.Fatal(err)
	}
	_, ok, err = New().GetRestoreCommand(context.Background(), ports.RestoreConfig{DataDir: data, Session: ports.SessionRef{ID: "ao-darwin-missing", WorkspacePath: workspace, Metadata: map[string]string{ports.MetadataKeyAgentSessionID: "unobserved-conversation"}}})
	if err == nil || ok || !strings.Contains(err.Error(), "not observed") {
		t.Fatalf("unobserved restore: %v %v", ok, err)
	}
}
