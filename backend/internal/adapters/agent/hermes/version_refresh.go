package hermes

// SupportsTerminalVersionRefresh declares unattended exact-conversation resume.
// AO does not manage the hermes binary lifecycle — it is a git install that
// updates itself — so AO never refreshes hermes terminals on a new release;
// the user (or hermes itself) upgrades, and AO's restore preflight then
// validates the workspace before an exact resume.
func (*Plugin) SupportsTerminalVersionRefresh() bool { return false }
