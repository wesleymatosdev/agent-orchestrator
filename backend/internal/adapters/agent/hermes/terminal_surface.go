package hermes

import (
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/terminalui"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// hermesComposerChromeLabels names provider-owned text Hermes paints inside or
// under its composer. Only styled-dim occurrences are forgiven; ordinary
// visible runes still read as a human draft.
var hermesComposerChromeLabels = []string{
	"msg=interrupt",
	"/queue",
	"Ctrl+C cancel",
	"? for help",
}

// hermesBlockedFrameMarkers mirror the approval/secret frames
// DetectTerminalActivity classifies as blocked.
var hermesBlockedFrameMarkers = []string{
	"Dangerous Command",
	"Allow once",
	"Allow this command?",
	"Help improve Hermes?",
	"Collect locally only",
	"type secret (hidden)",
	"type password (hidden)",
	"type your answer here",
}

// InspectTerminalSurface implements ports.TerminalSurfaceInspector for the
// Hermes classic TUI. The composer prompt is `☤ ❯` (blocked/secret frames use
// `⚠ ❯` / `? ❯`); an active turn repaints it with an `msg=interrupt` footer.
// Facts are deliberately conservative: unrecognized frames stay unknown and
// can never prove a conversation unstarted.
func (p *Plugin) InspectTerminalSurface(output string) ports.TerminalSurfaceObservation {
	// The v0.22 classic TUI frame, top to bottom: a bordered welcome dashboard
	// (│ panel rows, ╭╰ rounded borders), provider status notices (`◆ 3 project
	// skill(s) found …`), the welcome/tip lines ("Welcome to Hermes Agent…",
	// "✦ Tip: …"), a `☤` status header (model/context/timer/YOLO), full-width
	// ─ rules, and the composer `❯ <dim placeholder>`. All of those are
	// provider-owned by construction — only a composer line carrying ordinary
	// visible text beyond the known placeholder is a human draft.
	var kept []string
	for _, line := range terminalui.PlainTerminalLines(output) {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "│") || strings.HasPrefix(t, "╭") || strings.HasPrefix(t, "╰") ||
			hermesStatusNoticeLine(t) || strings.HasPrefix(t, "Welcome to Hermes Agent") ||
			strings.HasPrefix(t, "✦ Tip:") || hermesRuleLine(t) || strings.HasPrefix(t, "☤") {
			continue
		}
		kept = append(kept, line)
	}

	observation := ports.TerminalSurfaceObservation{}
	blocked, active, composerSeen, draft := false, false, false, false
	for _, line := range kept {
		trimmed := strings.TrimSpace(line)
		if hermesBlockedFrameLine(trimmed) {
			blocked = true
		}
		if strings.Contains(trimmed, "msg=interrupt") {
			active = true
		}
		if strings.HasPrefix(trimmed, "❯") {
			composerSeen = true
			if !hermesPlaceholderComposer(strings.TrimPrefix(trimmed, "❯")) {
				draft = true
				observation.Composer = ports.TerminalComposerDraft
			} else if observation.Composer == ports.TerminalComposerUnknown {
				observation.Composer = ports.TerminalComposerEmpty
			}
		}
	}
	switch {
	case blocked:
		observation.Work = ports.TerminalSurfaceWorkBlocked
		observation.Composer = ports.TerminalComposerUnknown
	case active:
		observation.Work = ports.TerminalSurfaceWorkActive
	case composerSeen:
		observation.Work = ports.TerminalSurfaceWorkIdle
		if observation.Composer == ports.TerminalComposerUnknown {
			observation.Composer = ports.TerminalComposerEmpty
		}
	}
	if observation.Work == ports.TerminalSurfaceWorkIdle && !draft && composerSeen {
		observation.NativeConversationNotStarted = true
	}
	return observation
}

// hermesRuleLine reports whether a trimmed line is a full-width horizontal
// rule (the composer box's top/bottom borders).
func hermesRuleLine(line string) bool {
	if line == "" {
		return false
	}
	for _, r := range line {
		if r != '─' && r != '━' && r != ' ' {
			return false
		}
	}
	return true
}

// hermesPlaceholderComposer reports whether the text after the composer
// marker is provider placeholder chrome rather than a human draft.
func hermesPlaceholderComposer(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return true
	}
	for _, prefix := range []string{"Ask anything"} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

func hermesComposerState(state terminalui.ComposerState) ports.TerminalComposerState {
	switch state {
	case terminalui.ComposerEmpty:
		return ports.TerminalComposerEmpty
	case terminalui.ComposerDraft:
		return ports.TerminalComposerDraft
	default:
		return ports.TerminalComposerUnknown
	}
}

func hermesBlockedFrameLine(line string) bool {
	for _, marker := range hermesBlockedFrameMarkers {
		if strings.Contains(line, marker) {
			return true
		}
	}
	return false
}

// hermesStatusNoticeLine recognizes hermes' startup status notices, e.g.
// "◆ 3 project skill(s) found in /path". The ◆ marker followed by a leading
// count is provider styling; human drafts do not open with a numbered notice.
func hermesStatusNoticeLine(line string) bool {
	if !strings.HasPrefix(line, "◆ ") {
		return false
	}
	rest := strings.TrimPrefix(line, "◆ ")
	i := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		i++
	}
	return i > 0
}
