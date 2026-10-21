package hermes

import (
	"context"
	"database/sql"
	"fmt"
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
	// approval flow, so it must be accepted. The launch path resolves the
	// hermes binary, which CI runners do not have, so stub one like the
	// private-query test below.
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "hermes"), []byte("#!/bin/sh\necho 'Hermes Agent v0.22.2+948'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
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

// AO's background title generation spawns a hermes child session per generated
// title, under <data>/background-tasks/title… — a foreign cwd. Those utility
// children are not history continuations and must not block the chat→TUI
// restore; only a same-workspace child (a real compaction fork) may.
func TestGetRestoreCommandIgnoresForeignWorkspaceChildren(t *testing.T) {
	home := t.TempDir()
	workspace := "/workspace"
	data := t.TempDir()
	dir := sessionDir(data, "ao-titled")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "native-home"), []byte(home), 0o600); err != nil {
		t.Fatal(err)
	}
	// Pre-write observed-terminal (the darwin crumb fallback is GOOS-gated);
	// the children rule under test is OS-independent.
	crumbPath := filepath.Join(home, "terminal-sessions", "tty-dev-ttys010")
	if err := os.WriteFile(filepath.Join(dir, "observed-terminal"), []byte(crumbPath), 0o600); err != nil {
		t.Fatal(err)
	}
	// Stub the hermes binary: GetRestoreCommand resolves it on PATH after the
	// children rule, and CI runners have no hermes install.
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "hermes"), []byte("#!/bin/sh\necho 'Hermes Agent v0.22.2+948'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	crumbDir := filepath.Join(home, "terminal-sessions")
	if err := os.MkdirAll(crumbDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(crumbPath, []byte(`{"session_id":"titled-conversation","cwd":"`+workspace+`","ts":20}`), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE sessions(id TEXT PRIMARY KEY, cwd TEXT, parent_session_id TEXT); CREATE TABLE messages(session_id TEXT);
		INSERT INTO sessions VALUES('titled-conversation', ?, NULL);
		INSERT INTO messages VALUES('titled-conversation');
		INSERT INTO sessions VALUES('title-task-1', '/data/background-tasks/title', 'titled-conversation');
		INSERT INTO sessions VALUES('title-task-2', '/data/background-tasks/title', 'titled-conversation')`, workspace); err != nil {
		t.Fatal(err)
	}
	if _, _, err := New().GetRestoreCommand(context.Background(), ports.RestoreConfig{DataDir: data, Session: ports.SessionRef{ID: "ao-titled", WorkspacePath: workspace, Metadata: map[string]string{ports.MetadataKeyAgentSessionID: "titled-conversation"}}}); err != nil {
		t.Fatalf("foreign-cwd children must not block restore: %v", err)
	}
	// A same-workspace child is a real compaction fork and still refuses.
	if _, err := db.ExecContext(t.Context(), `INSERT INTO sessions VALUES('compaction-child', ?, 'titled-conversation')`, workspace); err != nil {
		t.Fatal(err)
	}
	_, _, err = New().GetRestoreCommand(context.Background(), ports.RestoreConfig{DataDir: data, Session: ports.SessionRef{ID: "ao-titled", WorkspacePath: workspace, Metadata: map[string]string{ports.MetadataKeyAgentSessionID: "titled-conversation"}}})
	if err == nil || !strings.Contains(err.Error(), "native history missing") {
		t.Fatalf("same-workspace child must still refuse restore, got %v", err)
	}
}

// restoreFixture prepares an AO session dir with a pre-observed terminal
// crumb and a stubbed hermes binary — the common scaffolding of the restore
// tests. The darwin crumb fallback is GOOS-gated, so tests pre-write
// observed-terminal to exercise OS-independent rules.
func restoreFixture(t *testing.T, aoSession, crumbSession, crumbCwd string) (data, home string) {
	t.Helper()
	data = t.TempDir()
	home = t.TempDir()
	dir := sessionDir(data, aoSession)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "native-home"), []byte(home), 0o600); err != nil {
		t.Fatal(err)
	}
	crumbPath := filepath.Join(home, "terminal-sessions", "tty-dev-ttys010")
	if err := os.MkdirAll(filepath.Dir(crumbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	crumb := fmt.Sprintf(`{"session_id":%q,"cwd":%q,"ts":20}`, crumbSession, crumbCwd)
	if err := os.WriteFile(crumbPath, []byte(crumb), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "observed-terminal"), []byte(crumbPath), 0o600); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "hermes"), []byte("#!/bin/sh\necho 'Hermes Agent v0.22.2+948'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	return data, home
}

// restoreDB creates the minimal native state.db schema used by the restore
// tests and registers its cleanup.
func restoreDB(t *testing.T, home string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(t.Context(), "CREATE TABLE sessions(id TEXT PRIMARY KEY, cwd TEXT, parent_session_id TEXT); CREATE TABLE messages(session_id TEXT)"); err != nil {
		t.Fatal(err)
	}
	return db
}

// Issue #22: a compacted session's same-workspace child chain IS the
// conversation continuing — the restore must adopt the live leaf instead of
// failing on drift, and persist the adoption for the next restore.
func TestRestoreAdoptsCompactionForkLeaf(t *testing.T) {
	workspace := "/workspace"
	data, home := restoreFixture(t, "ao-fork", "fork-parent", workspace)
	db := restoreDB(t, home)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO sessions VALUES('fork-parent', ?, NULL);
		INSERT INTO messages VALUES('fork-parent');
		INSERT INTO messages VALUES('fork-parent');
		INSERT INTO sessions VALUES('fork-child', ?, 'fork-parent');
		INSERT INTO messages VALUES('fork-child')`, workspace, workspace); err != nil {
		t.Fatal(err)
	}
	cmd, ok, err := New().GetRestoreCommand(context.Background(), ports.RestoreConfig{DataDir: data, Session: ports.SessionRef{ID: "ao-fork", WorkspacePath: workspace, Metadata: map[string]string{ports.MetadataKeyAgentSessionID: "fork-parent"}}})
	if err != nil || !ok {
		t.Fatalf("compacted session must restore at the leaf: ok=%v err=%v", ok, err)
	}
	if cmd[len(cmd)-2] != "--resume" || cmd[len(cmd)-1] != "fork-child" {
		t.Fatalf("resume argv tail: %v", cmd)
	}
	note, readErr := os.ReadFile(filepath.Join(sessionDir(data, "ao-fork"), "live-head"))
	if readErr != nil || !strings.Contains(string(note), "resumed at live head fork-child (1 messages since fork)") {
		t.Fatalf("live-head note=%q err=%v", note, readErr)
	}

	// A crumb already rotated to the adopted leaf (compaction rewrites the
	// tty breadcrumb) is the same terminal identity chain, not staleness.
	data, home = restoreFixture(t, "ao-fork-rotated", "fork-child", workspace)
	db = restoreDB(t, home)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO sessions VALUES('fork-parent', ?, NULL);
		INSERT INTO messages VALUES('fork-parent');
		INSERT INTO sessions VALUES('fork-child', ?, 'fork-parent');
		INSERT INTO messages VALUES('fork-child')`, workspace, workspace); err != nil {
		t.Fatal(err)
	}
	cmd, ok, err = New().GetRestoreCommand(context.Background(), ports.RestoreConfig{DataDir: data, Session: ports.SessionRef{ID: "ao-fork-rotated", WorkspacePath: workspace, Metadata: map[string]string{ports.MetadataKeyAgentSessionID: "fork-parent"}}})
	if err != nil || !ok {
		t.Fatalf("leaf-rotated crumb must restore: ok=%v err=%v", ok, err)
	}
	if cmd[len(cmd)-1] != "fork-child" {
		t.Fatalf("resume argv tail: %v", cmd)
	}
}

func TestRestoreAdoptsDeepForkChainTip(t *testing.T) {
	workspace := "/workspace"
	data, home := restoreFixture(t, "ao-deep", "fork-root", workspace)
	db := restoreDB(t, home)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO sessions VALUES('fork-root', ?, NULL);
		INSERT INTO messages VALUES('fork-root');
		INSERT INTO sessions VALUES('fork-mid', ?, 'fork-root');
		INSERT INTO sessions VALUES('fork-tip', ?, 'fork-mid');
		INSERT INTO messages VALUES('fork-tip')`, workspace, workspace, workspace); err != nil {
		t.Fatal(err)
	}
	cmd, ok, err := New().GetRestoreCommand(context.Background(), ports.RestoreConfig{DataDir: data, Session: ports.SessionRef{ID: "ao-deep", WorkspacePath: workspace, Metadata: map[string]string{ports.MetadataKeyAgentSessionID: "fork-root"}}})
	if err != nil || !ok {
		t.Fatalf("deep chain must restore at the tip: ok=%v err=%v", ok, err)
	}
	if cmd[len(cmd)-2] != "--resume" || cmd[len(cmd)-1] != "fork-tip" {
		t.Fatalf("resume argv tail: %v", cmd)
	}
}

func TestRestoreAdoptsLiveCwdOnDrift(t *testing.T) {
	data, home := restoreFixture(t, "ao-drift", "drifted", "/elsewhere")
	db := restoreDB(t, home)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO sessions VALUES('drifted', '/elsewhere', NULL);
		INSERT INTO messages VALUES('drifted');
		INSERT INTO messages VALUES('drifted')`); err != nil {
		t.Fatal(err)
	}
	cmd, ok, err := New().GetRestoreCommand(context.Background(), ports.RestoreConfig{DataDir: data, Session: ports.SessionRef{ID: "ao-drift", WorkspacePath: "/workspace", Metadata: map[string]string{ports.MetadataKeyAgentSessionID: "drifted"}}})
	if err != nil || !ok {
		t.Fatalf("cwd drift must adopt the live cwd: ok=%v err=%v", ok, err)
	}
	if cmd[3] != "--in" || cmd[4] != "/elsewhere" {
		t.Fatalf("restore workspace argv: %v", cmd)
	}
	if cmd[len(cmd)-2] != "--resume" || cmd[len(cmd)-1] != "drifted" {
		t.Fatalf("resume argv tail: %v", cmd)
	}
	dir := sessionDir(data, "ao-drift")
	note, readErr := os.ReadFile(filepath.Join(dir, "live-head"))
	if readErr != nil || !strings.Contains(string(note), "adopted live cwd /elsewhere for restore") {
		t.Fatalf("live-head note=%q err=%v", note, readErr)
	}
	adopted, readErr := os.ReadFile(filepath.Join(dir, "native-workspace"))
	if readErr != nil || string(adopted) != "/elsewhere" {
		t.Fatalf("native-workspace=%q err=%v", adopted, readErr)
	}
}

func TestRestoreStillRefusesEmptySessionWithoutLeaf(t *testing.T) {
	workspace := "/workspace"
	data, home := restoreFixture(t, "ao-empty", "empty-conversation", workspace)
	db := restoreDB(t, home)
	if _, err := db.ExecContext(t.Context(), "INSERT INTO sessions VALUES('empty-conversation', ?, NULL)", workspace); err != nil {
		t.Fatal(err)
	}
	_, ok, err := New().GetRestoreCommand(context.Background(), ports.RestoreConfig{DataDir: data, Session: ports.SessionRef{ID: "ao-empty", WorkspacePath: workspace, Metadata: map[string]string{ports.MetadataKeyAgentSessionID: "empty-conversation"}}})
	if err == nil || ok || !strings.Contains(err.Error(), "native history missing") {
		t.Fatalf("empty session without leaf must refuse: ok=%v err=%v", ok, err)
	}
}
