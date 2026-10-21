// Agents a Chat session can switch to: the ones with a structured Chat driver.
// hermes is a fork addition: it has a chat driver (hermesacp) but its native
// terminal cannot stage a fresh native id, so it stays out of the terminal list.
export const CHAT_SWITCH_TARGETS = [
  "claude-code",
  "codex",
  "cursor",
  "opencode",
  "opencode-v2",
  "droid",
  "kimi",
  "kimchi",
  "pi",
  "omp",
  "qwen",
  "deepseek-harness",
  "unreal-agent",
  "hermes",
] as const;
// Agents a Terminal session can switch to: the Chat agents that also run in a terminal (Unreal
// Agent is Chat-only), plus terminal agents that take the handoff note on their command line.
// Agents that have it typed in after start stay out until each passes a real switch.
// Gemini CLI is no longer supported.
export const TERMINAL_SWITCH_TARGETS = [
  ...CHAT_SWITCH_TARGETS.filter((agent) => agent !== "unreal-agent"),
  "continue",
  "devin",
  "muse",
  "kilocode",
  "mimo-code",
  "prime-agent",
  "auggie",
  "copilot",
  "agy",
  "autohand",
  "vibe",
] as const;
// Terminal targets that answer one task and exit: greyed out with the reason there, still switchable in Chat.
export const TERMINAL_ONE_SHOT_TARGETS: ReadonlySet<string> = new Set([
  "deepseek-harness",
]);
