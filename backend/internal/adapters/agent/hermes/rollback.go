package hermes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrRollbackCutUnresolvable reports that the requested rollback point cannot
// be mapped onto hermes' own transcript: the session is missing, the cut lands
// before the transcript's oldest retained (active) user turn — which is what a
// pre-compaction target looks like — or the transcript's user turn at the
// resolved depth does not carry the prompt AO named. Refusing beats guessing:
// any invented cut would discard turns the user never named.
var ErrRollbackCutUnresolvable = errors.New("hermes: rollback cut cannot be resolved on the native transcript")

// RollbackFork is one completed fork-at-point.
type RollbackFork struct {
	// ChildID is the new hermes session holding the transcript up to the cut.
	ChildID string
	// HeadID is the live leaf the fork branched from (the recorded id after
	// any compaction-chain adoption).
	HeadID string
	// KeptMessages is how many transcript rows the child carries.
	KeptMessages int
	// DroppedTurns is how many user turns the child does NOT carry (the cut
	// turn and everything after it).
	DroppedTurns int
}

// ForkAtCut maps AO's rollback-at-message onto hermes' own rewind semantics:
// never mutating existing history rows in place, it creates a new hermes
// session (a new leaf on the compaction fork chain) whose transcript is the
// live leaf's active history up to — excluding — the cut user turn.
//
// The cut is addressed the only way that survives compaction: by depth from
// the transcript's newest user turn. turnsAfterInclusive is how many of AO's
// rollbackable turns sit at or after the cut (the cut turn included); cutPrompt
// is the cut turn's prompt text. The transcript's user turn at the resolved
// depth must carry that prompt, so a drifted AO projection fails closed
// instead of discarding someone else's turns.
//
// Tool-call pairing stays intact by construction — the cut sits on a user-turn
// boundary, so every tool result in the kept prefix belongs to a call that is
// also in the prefix — and is verified anyway before anything is written.
//
// The caller owns making the provider actually continue from the new leaf
// (point AO's recorded provider conversation at ChildID and resume the
// session); this function only creates and persists the fork.
func (p *Plugin) ForkAtCut(
	ctx context.Context,
	dataDir, aoSessionID, recordedAgentSessionID, cutPrompt string,
	turnsAfterInclusive int,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if turnsAfterInclusive < 1 {
		return "", fmt.Errorf("%w: rollback must discard at least the named turn", ErrRollbackCutUnresolvable)
	}
	id := strings.TrimSpace(recordedAgentSessionID)
	if id == "" || id == "latest" || strings.ContainsAny(id, " /\\\r\n\t") {
		return "", fmt.Errorf("%w: exact native session ID required", ErrRollbackCutUnresolvable)
	}
	home, err := nativeHome(dataDir, aoSessionID)
	if err != nil {
		return "", err
	}
	// Read-write on purpose: the fork writes new rows only. The busy timeout
	// keeps a concurrently flushing hermes writer from failing the fork.
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(home, "state.db"))+"?_pragma=busy_timeout%3d5000")
	if err != nil {
		return "", err
	}
	defer func() { _ = db.Close() }()

	headID, err := rollbackLiveHead(ctx, db, id)
	if err != nil {
		return "", err
	}

	msgColumns, err := tableColumns(ctx, db, "messages")
	if err != nil {
		return "", err
	}
	transcript, err := readTranscript(ctx, db, headID, msgColumns)
	if err != nil {
		return "", err
	}

	userTurns := userTurnIndices(transcript)
	cutPos := len(userTurns) - turnsAfterInclusive
	if cutPos < 0 {
		return "", fmt.Errorf("%w: target predates the native transcript's retained history (compacted past the cut)", ErrRollbackCutUnresolvable)
	}
	cutRow := transcript[userTurns[cutPos]]
	if !promptMatches(flattenRowContent(cutRow), cutPrompt) {
		return "", fmt.Errorf("%w: native transcript turn %d does not carry the named prompt", ErrRollbackCutUnresolvable, cutPos)
	}

	kept := transcript[:userTurns[cutPos]]
	if err := verifyToolPairing(kept); err != nil {
		return "", err
	}

	childID := uuid.NewString()
	if err := writeFork(ctx, db, headID, childID, kept); err != nil {
		return "", err
	}
	return childID, nil
}

// RecordRollbackNote persists the rollback adoption under the AO session dir —
// the same inspectability convention as the live-head note a drifted restore
// writes — so the next native restore's provenance is readable.
func (p *Plugin) RecordRollbackNote(dataDir, aoSessionID, childID string, droppedTurns int) error {
	dir := sessionDir(dataDir, aoSessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	note := fmt.Sprintf("rolled back at fork %s (%d user turns discarded)\n", childID, droppedTurns)
	return os.WriteFile(filepath.Join(dir, "live-head"), []byte(note), 0o600)
}

// rollbackLiveHead resolves the compaction fork chain's live leaf for the
// recorded session id — the same adopt-live-head walk GetRestoreCommand uses —
// so a rollback lands on the conversation hermes is actually continuing.
func rollbackLiveHead(ctx context.Context, db *sql.DB, recorded string) (string, error) {
	var cwd string
	var messages int
	var children int
	rowErr := db.QueryRowContext(ctx,
		"SELECT COALESCE(cwd,''), (SELECT count(*) FROM messages WHERE session_id=sessions.id), (SELECT count(*) FROM sessions child WHERE child.parent_session_id=sessions.id AND COALESCE(child.cwd,'')=COALESCE(sessions.cwd,'')) FROM sessions WHERE id=?",
		recorded).Scan(&cwd, &messages, &children)
	if rowErr != nil {
		return "", fmt.Errorf("%w: native session %s is missing", ErrRollbackCutUnresolvable, recorded)
	}
	if children == 0 && messages > 0 {
		return recorded, nil
	}
	leafID, _, _, _, ok := liveForkLeaf(ctx, db, recorded)
	if !ok {
		return "", fmt.Errorf("%w: native history missing or fork chain ended on an empty leaf", ErrRollbackCutUnresolvable)
	}
	return leafID, nil
}

// tableColumns returns the table's column names in declared order. The fork
// copies every column it finds (minus the ones it owns), so schema drift on
// the hermes side cannot produce a silently lossy copy.
func tableColumns(ctx context.Context, querier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, table string) ([]string, error) {
	rows, err := querier.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var columns []string
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("hermes: native table %s has no columns", table)
	}
	return columns, nil
}

// transcriptRow is one read transcript row: named columns with their raw
// values, ready to be copied into the fork child verbatim.
type transcriptRow struct {
	columns []string
	values  []any
}

func (r transcriptRow) str(column string) string {
	for i, name := range r.columns {
		if name == column {
			if r.values[i] == nil {
				return ""
			}
			if s, ok := r.values[i].(sql.NullString); ok {
				return s.String
			}
			return strings.TrimSpace(fmt.Sprint(r.values[i]))
		}
	}
	return ""
}

// readTranscript reads the head session's active rows in transcript order.
// Sessions whose schema predates the active column read every row: nothing to
// filter means nothing archived.
func readTranscript(ctx context.Context, db *sql.DB, headID string, columns []string) ([]transcriptRow, error) {
	var selected, names []string
	for _, column := range columns {
		if column == "id" {
			continue
		}
		names = append(names, column)
		selected = append(selected, quoteIdent(column))
	}
	//nolint:gosec // column identifiers are code-owned and quoted via quoteIdent; values are parameterized
	query := "SELECT rowid, " + strings.Join(selected, ", ") + " FROM messages WHERE session_id=?"
	if hasColumn(columns, "active") {
		query += " AND active=1"
	}
	query += " ORDER BY rowid"
	rows, err := db.QueryContext(ctx, query, headID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var transcript []transcriptRow
	for rows.Next() {
		values := make([]any, len(selected))
		ptrs := make([]any, len(selected))
		for i := range values {
			ptrs[i] = &values[i]
		}
		var rowid int64
		ptrs = append([]any{&rowid}, ptrs...)
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		transcript = append(transcript, transcriptRow{columns: names, values: values})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return transcript, nil
}

func userTurnIndices(transcript []transcriptRow) []int {
	var indices []int
	for i, row := range transcript {
		if strings.EqualFold(row.str("role"), "user") {
			indices = append(indices, i)
		}
	}
	return indices
}

// flattenRowContent flattens a stored content cell to plain text. Hermes
// persists either a plain string or a JSON array of typed parts; both project
// to the same text its own durable views compare with.
func flattenRowContent(row transcriptRow) string {
	raw := row.str("content")
	if raw == "" {
		return ""
	}
	var parts []any
	if err := json.Unmarshal([]byte(raw), &parts); err != nil {
		return raw
	}
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		switch typed := part.(type) {
		case string:
			texts = append(texts, typed)
		case map[string]any:
			if text, ok := typed["text"].(string); ok {
				texts = append(texts, text)
			}
		}
	}
	return strings.Join(texts, "\n")
}

// promptMatches compares AO's cut prompt with the native row's flattened
// content. Equality is the ordinary case; a suffix match accepts compaction
// carriers, whose row keeps the retained summary ahead of the live ask it
// represents. Whitespace-insensitive on both sides.
func promptMatches(native, prompt string) bool {
	norm := func(s string) string {
		return strings.Join(strings.Fields(s), " ")
	}
	nativeNorm, promptNorm := norm(native), norm(prompt)
	if nativeNorm == "" || promptNorm == "" {
		return false
	}
	return nativeNorm == promptNorm || strings.HasSuffix(nativeNorm, promptNorm)
}

// verifyToolPairing refuses a kept prefix whose assistant tool calls are not
// all answered inside the prefix. A rollback that dangles a tool result (or a
// call) would hand hermes a transcript the provider API rejects on the next
// turn. At a user-turn boundary this cannot trigger; the check keeps an
// unexpected transcript shape from silently shipping one.
func verifyToolPairing(kept []transcriptRow) error {
	calls := map[string]bool{}
	answered := map[string]bool{}
	for _, row := range kept {
		switch strings.ToLower(row.str("role")) {
		case "assistant":
			for _, id := range toolCallIDs(row.str("tool_calls")) {
				calls[id] = true
			}
		case "tool":
			if id := row.str("tool_call_id"); id != "" {
				answered[id] = true
			}
		}
	}
	for id := range calls {
		if !answered[id] {
			return fmt.Errorf("%w: kept transcript dangles tool call %s", ErrRollbackCutUnresolvable, id)
		}
	}
	return nil
}

// toolCallIDs extracts the call ids from a stored tool_calls cell (a JSON
// array of objects carrying an "id"). Unparsable cells yield nothing: the
// pairing check then simply has nothing to demand for them.
func toolCallIDs(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var calls []map[string]any
	if err := json.Unmarshal([]byte(raw), &calls); err != nil {
		return nil
	}
	var ids []string
	for _, call := range calls {
		if id, ok := call["id"].(string); ok && id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// writeFork creates the child session and copies the kept transcript into it,
// atomically. Existing rows are only ever read here — the parent chain stays
// exactly as hermes wrote it, which is the whole point of mapping rollback to
// a fork instead of an in-place rewind.
func writeFork(ctx context.Context, db *sql.DB, headID, childID string, kept []transcriptRow) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	sessionColumns, err := tableColumns(ctx, tx, "sessions")
	if err != nil {
		return err
	}
	headValues, err := readSessionRow(ctx, tx, headID, sessionColumns)
	if err != nil {
		return err
	}
	owned := map[string]any{
		"id":                childID,
		"parent_session_id": headID,
		"started_at":        float64(time.Now().UnixNano()) / 1e9,
		"ended_at":          nil,
		"end_reason":        nil,
		"message_count":     len(kept),
		"tool_call_count":   countToolCalls(kept),
	}
	var (
		names  []string
		values []any
	)
	for i, column := range sessionColumns {
		if value, ok := owned[column]; ok {
			names = append(names, column)
			values = append(values, value)
			continue
		}
		names = append(names, column)
		values = append(values, headValues[i])
	}
	//nolint:gosec // column identifiers are code-owned and quoted via quoteIdents; values are parameterized
	insert := "INSERT INTO sessions (" + strings.Join(quoteIdents(names), ", ") + ") VALUES (" + strings.TrimSuffix(strings.Repeat("?, ", len(names)), ", ") + ")"
	if _, err := tx.ExecContext(ctx, insert, values...); err != nil {
		return fmt.Errorf("insert hermes fork session: %w", err)
	}

	for _, row := range kept {
		columns := append([]string{"session_id"}, row.columns...)
		vals := append([]any{childID}, row.values...)
		//nolint:gosec // column identifiers are code-owned and quoted via quoteIdents; values are parameterized
		insert := "INSERT INTO messages (" + strings.Join(quoteIdents(columns), ", ") + ") VALUES (" + strings.TrimSuffix(strings.Repeat("?, ", len(columns)), ", ") + ")"
		if _, err := tx.ExecContext(ctx, insert, vals...); err != nil {
			return fmt.Errorf("copy hermes transcript into fork: %w", err)
		}
	}
	return tx.Commit()
}

// readSessionRow reads the head session's full row in declared column order.
func readSessionRow(ctx context.Context, querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, headID string, columns []string) ([]any, error) {
	selected := make([]string, 0, len(columns))
	for _, column := range columns {
		selected = append(selected, quoteIdent(column))
	}
	values := make([]any, len(columns))
	ptrs := make([]any, len(columns))
	for i := range values {
		ptrs[i] = &values[i]
	}
	if err := querier.QueryRowContext(ctx, "SELECT "+strings.Join(selected, ", ")+" FROM sessions WHERE id=?", headID).Scan(ptrs...); err != nil {
		return nil, fmt.Errorf("%w: native session row vanished mid-fork", ErrRollbackCutUnresolvable)
	}
	return values, nil
}

func countToolCalls(kept []transcriptRow) int {
	count := 0
	for _, row := range kept {
		count += len(toolCallIDs(row.str("tool_calls")))
	}
	return count
}

func hasColumn(columns []string, name string) bool {
	for _, column := range columns {
		if column == name {
			return true
		}
	}
	return false
}

// quoteIdent guards the few identifiers this file interpolates into SQL — all
// of them come from PRAGMA table_info, never from user input.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func quoteIdents(names []string) []string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = quoteIdent(name)
	}
	return quoted
}
