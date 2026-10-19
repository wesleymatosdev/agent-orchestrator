// Package hermesacp binds the user's own Hermes installation to AO's reusable
// ACP Chat transport.
package hermesacp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	acpdriver "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/nativeacp"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// hermesACPVersionTimeout bounds the `hermes acp --version` admission probe.
const hermesACPVersionTimeout = 10 * time.Second

// outputLanguageInstruction pins every hermes chat session to English. Hermes
// sessions commonly run on the Z.ai/GLM models, which narrate tool calls in
// their native Chinese style; that narration flows verbatim into the AO chat
// transcript and into edit/permission approval cards, so a reader who cannot
// read Chinese cannot tell what they are approving. This is a harness-level
// constant: no per-project configuration has to remember it.
const outputLanguageInstruction = "Output language: all user-visible output (narration, tool-call text, permission and edit approval requests, diff context, commit and PR text) must be in English. Never output Chinese."

// cardLabelReplacements maps the known Z.ai/GLM provider-side tool-call card
// labels to their English equivalents. It is a deliberate, explicit table —
// not a general translation layer — for text the provider generates before it
// reaches the ACP stream. The full-width colon keeps ordinary Chinese prose
// that happens to contain the label word untouched.
var cardLabelReplacements = []struct{ from, to string }{
	{"内置工具：", "built-in tool: "},
	{"输入：", "input: "},
	{"输出：", "output: "},
}

// normalizeCardLabels rewrites the known Chinese card labels in provider text.
// Replacements are idempotent, so normalizing per-delta and again over the
// accumulated message text is safe.
func normalizeCardLabels(text string) string {
	for _, mapping := range cardLabelReplacements {
		text = strings.ReplaceAll(text, mapping.from, mapping.to)
	}
	return text
}

// New launches `hermes acp` from the exact binary resolved by the existing
// Hermes agent plugin. Hermes keeps filesystem and terminal execution in its
// own process; AO advertises neither ACP client capability and receives typed
// session updates and permission requests.
func New(plugin nativeacp.Plugin, log *slog.Logger) ports.ChatDriver {
	return newDriver(plugin, versionProbe, log)
}

func newDriver(plugin nativeacp.Plugin, probe nativeacp.VersionProbe, log *slog.Logger) ports.ChatDriver {
	return nativeacp.New(plugin, nativeacp.Config{
		Harness: domain.HarnessHermes,
		Capabilities: ports.ChatCapabilities{
			ports.ChatCapabilityDiffs: true,
		},
		NormalizeText: normalizeCardLabels,
		Configure:     configure,
		// Hermes advertises exactly its owner's current edit-approval policy as
		// the session's only mode and refuses session/set_mode (the gateway has
		// no verb that changes it). The mode is the session's fixed policy, not
		// a choice: advertising it gave every picker a one-item "permissions"
		// menu that suppressed AO's own approval modes. AO's approval choice is
		// honored per turn instead — default/accept-edits/auto through the turn
		// settings path, bypass through PermissionPolicy.
		SessionModesAdvisory: true,
		PermissionPolicy:     permissionPolicy,
		VersionProbe:         probe,
	}, log)
}

// configure adds provider-native arguments and environment overlays. Hermes'
// ACP server has no launch-time model flag — models are session-owned and
// switched through the ACP config options the session response advertises — and
// no system-prompt flag, so standing instructions ride in
// HERMES_EPHEMERAL_SYSTEM_PROMPT, which Hermes injects at API-call time without
// persisting it into the transcript. The output-language pin is appended
// unconditionally: hermes sessions on Z.ai/GLM models otherwise narrate tool
// calls and approval requests in Chinese, which AO users cannot act on.
func configure(_ context.Context, cfg acpdriver.LaunchConfig) ([]string, map[string]string, error) {
	args := []string{"acp"}
	prompt := strings.TrimSpace(cfg.SystemPrompt)
	if prompt != "" {
		prompt += "\n\n"
	}
	return args, map[string]string{"HERMES_EPHEMERAL_SYSTEM_PROMPT": prompt + outputLanguageInstruction}, nil
}

// permissionPolicy resolves permission requests that have an exact AO policy
// mapping before the generic client parks them for a human. Only bypass
// qualifies: in bypass mode AO answers every Hermes dangerous-command approval
// with allow, because Hermes' ACP server has no server-side bypass switch.
func permissionPolicy(
	mode ports.PermissionMode,
	params acpsdk.RequestPermissionRequest,
) (acpsdk.PermissionOptionId, bool) {
	if ports.NormalizePermissionMode(mode) != ports.PermissionModeBypassPermissions {
		return "", false
	}
	if id, ok := permissionOption(params.Options, acpsdk.PermissionOptionKindAllowAlways); ok {
		return id, true
	}
	return permissionOption(params.Options, acpsdk.PermissionOptionKindAllowOnce)
}

func permissionOption(
	options []acpsdk.PermissionOption,
	kind acpsdk.PermissionOptionKind,
) (acpsdk.PermissionOptionId, bool) {
	for _, option := range options {
		if option.Kind == kind {
			return option.OptionId, true
		}
	}
	return "", false
}

// versionProbe gates admission on `hermes acp --version` succeeding, which
// proves the resolved executable's ACP entry point imports and answers before
// a session launch pays for the full startup.
func versionProbe(ctx context.Context, bin string) error {
	probeCtx, cancel := context.WithTimeout(ctx, hermesACPVersionTimeout)
	defer cancel()
	cmd := aoprocess.CommandContext(probeCtx, bin, "acp", "--version")
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("hermes acp --version: %w", err)
	}
	if strings.TrimSpace(string(out)) == "" {
		return errors.New("hermes acp --version printed no version")
	}
	return nil
}
