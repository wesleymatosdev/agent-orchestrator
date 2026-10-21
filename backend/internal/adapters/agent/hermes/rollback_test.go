package hermes

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// rollbackFixture builds a throwaway HERMES_HOME with a minimal state.db and
// points the AO session dir's native-home marker at it, the way the daemon's
// launch path does.
type rollbackFixture struct {
	t       *testing.T
	home    string
	dataDir string
	db      *sql.DB
	jobID   string
}

func newRollbackFixture(t *testing.T) *rollbackFixture {
	t.Helper()
	home := t.TempDir()
	dataDir := t.TempDir()
	jobID := "ao-session"
	f := &rollbackFixture{t: t, home: home, dataDir: dataDir, jobID: jobID}
	dir := sessionDir(dataDir, jobID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "native-home"), []byte(home), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	schema := `CREATE TABLE sessions(
		id TEXT PRIMARY KEY, parent_session_id TEXT, cwd TEXT, model TEXT,
		started_at REAL, ended_at REAL, message_count INTEGER
	);
	CREATE TABLE messages(
		session_id TEXT, role TEXT, content TEXT,
		tool_call_id TEXT, tool_calls TEXT, active INTEGER DEFAULT 1
	);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	f.db = db
	return f
}

func (f *rollbackFixture) addSession(id, parent, cwd string) {
	f.t.Helper()
	if _, err := f.db.Exec(
		"INSERT INTO sessions(id, parent_session_id, cwd, started_at) VALUES(?,?,?,1.0)", id, parent, cwd); err != nil {
		f.t.Fatal(err)
	}
}

func (f *rollbackFixture) addMessage(session, role, content string) {
	f.t.Helper()
	if _, err := f.db.Exec(
		"INSERT INTO messages(session_id, role, content) VALUES(?,?,?)", session, role, content); err != nil {
		f.t.Fatal(err)
	}
}

func (f *rollbackFixture) messages(session string) []string {
	f.t.Helper()
	rows, err := f.db.Query(
		"SELECT role || ':' || COALESCE(content,'') FROM messages WHERE session_id=? ORDER BY rowid", session)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, line)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

func TestForkAtCutKeepsPrefixDropsTail(t *testing.T) {
	f := newRollbackFixture(t)
	f.addSession("head", "", "/ws")
	f.addMessage("head", "user", "first ask")
	f.addMessage("head", "assistant", "first answer")
	f.addMessage("head", "user", "second ask")
	f.addMessage("head", "assistant", "second answer")
	f.addMessage("head", "user", "third ask")
	f.addMessage("head", "assistant", "third answer")

	// Cut at the second user turn: that turn and the third go away.
	fork, err := New().ForkAtCut(context.Background(), f.dataDir, f.jobID, "head", "second ask", 2)
	if err != nil {
		t.Fatalf("ForkAtCut: %v", err)
	}
	if fork.HeadID != "head" || fork.DroppedTurns != 2 {
		t.Fatalf("unexpected fork %+v", fork)
	}
	kept := f.messages(fork.ChildID)
	want := []string{"user:first ask", "assistant:first answer"}
	if len(kept) != len(want) {
		t.Fatalf("child transcript = %v, want %v", kept, want)
	}
	for i := range want {
		if kept[i] != want[i] {
			t.Fatalf("child transcript = %v, want %v", kept, want)
		}
	}
	var parent string
	var count int
	if err := f.db.QueryRow(
		"SELECT COALESCE(parent_session_id,''), message_count FROM sessions WHERE id=?", fork.ChildID,
	).Scan(&parent, &count); err != nil {
		t.Fatal(err)
	}
	if parent != "head" || count != len(want) {
		t.Fatalf("child session row: parent=%q count=%d", parent, count)
	}
	// The parent chain is never mutated in place.
	if got := f.messages("head"); len(got) != 6 {
		t.Fatalf("head transcript changed: %v", got)
	}
}

func TestForkAtCutAfterCompactionLandsOnLiveLeaf(t *testing.T) {
	f := newRollbackFixture(t)
	// Compaction chain: recorded -> drained parent -> live leaf. The parent's
	// rows are archived (active=0) and the leaf carries a compaction carrier
	// plus the post-compaction turns.
	f.addSession("recorded", "", "/ws")
	f.addSession("leaf", "recorded", "/ws")
	f.addMessage("recorded", "user", "pre-compaction ask")
	f.addMessage("recorded", "assistant", "pre-compaction answer")
	f.addMessage("leaf", "user", "summary of earlier turns\n\npost-compaction first ask")
	f.addMessage("leaf", "assistant", "post-compaction answer")
	f.addMessage("leaf", "user", "post-compaction second ask")

	fork, err := New().ForkAtCut(context.Background(), f.dataDir, f.jobID, "recorded",
		"post-compaction second ask", 1)
	if err != nil {
		t.Fatalf("ForkAtCut: %v", err)
	}
	if fork.HeadID != "leaf" {
		t.Fatalf("fork head = %q, want the live leaf", fork.HeadID)
	}
	kept := f.messages(fork.ChildID)
	want := []string{
		"user:summary of earlier turns\n\npost-compaction first ask",
		"assistant:post-compaction answer",
	}
	if len(kept) != len(want) {
		t.Fatalf("child transcript = %v, want %v", kept, want)
	}
	for i := range want {
		if kept[i] != want[i] {
			t.Fatalf("child transcript = %v, want %v", kept, want)
		}
	}
}

func TestForkAtCutRefusesCutBeforeRetainedHistory(t *testing.T) {
	f := newRollbackFixture(t)
	f.addSession("recorded", "", "/ws")
	f.addSession("leaf", "recorded", "/ws")
	f.addMessage("leaf", "user", "only retained ask")
	f.addMessage("leaf", "assistant", "answer")

	// Two turns to drop, but the live leaf holds one user turn: the target is
	// on the compacted-away side of the chain.
	if _, err := New().ForkAtCut(context.Background(), f.dataDir, f.jobID, "recorded",
		"pre-compaction ask", 2); !errors.Is(err, ErrRollbackCutUnresolvable) {
		t.Fatalf("err = %v, want ErrRollbackCutUnresolvable", err)
	}
}

func TestForkAtCutVerifiesTheNamedPrompt(t *testing.T) {
	f := newRollbackFixture(t)
	f.addSession("head", "", "/ws")
	f.addMessage("head", "user", "real ask")
	f.addMessage("head", "assistant", "answer")
	f.addMessage("head", "user", "later ask")

	if _, err := New().ForkAtCut(context.Background(), f.dataDir, f.jobID, "head",
		"a prompt that never happened", 2); !errors.Is(err, ErrRollbackCutUnresolvable) {
		t.Fatalf("err = %v, want ErrRollbackCutUnresolvable", err)
	}
}

func TestForkAtCutRefusesDanglingToolPair(t *testing.T) {
	f := newRollbackFixture(t)
	f.addSession("head", "", "/ws")
	f.addMessage("head", "user", "run it")
	f.addMessage("head", "assistant", "")
	// The result row lives AFTER the cut boundary, so the kept prefix would
	// carry an unanswered call.
	if _, err := f.db.Exec(
		`UPDATE messages SET tool_calls='[{"id":"call-1"}]' WHERE role='assistant'`); err != nil {
		f.t.Fatal(err)
	}
	f.addMessage("head", "user", "next ask")
	if _, err := f.db.Exec(
		`UPDATE messages SET role='tool', tool_call_id='call-1', content='{}'
		 WHERE rowid = (SELECT max(rowid) FROM messages)`); err != nil {
		f.t.Fatal(err)
	}

	if _, err := New().ForkAtCut(context.Background(), f.dataDir, f.jobID, "head",
		"next ask", 1); !errors.Is(err, ErrRollbackCutUnresolvable) {
		t.Fatalf("err = %v, want ErrRollbackCutUnresolvable", err)
	}
}

func TestForkAtCutRefusesUnknownSessionAndBadIDs(t *testing.T) {
	f := newRollbackFixture(t)
	if _, err := New().ForkAtCut(context.Background(), f.dataDir, f.jobID, "nope", "x", 1); !errors.Is(err, ErrRollbackCutUnresolvable) {
		t.Fatalf("err = %v, want ErrRollbackCutUnresolvable", err)
	}
	if _, err := New().ForkAtCut(context.Background(), f.dataDir, f.jobID, "", "x", 1); !errors.Is(err, ErrRollbackCutUnresolvable) {
		t.Fatalf("err = %v, want ErrRollbackCutUnresolvable", err)
	}
	f.addSession("head", "", "/ws")
	f.addMessage("head", "user", "ask")
	if _, err := New().ForkAtCut(context.Background(), f.dataDir, f.jobID, "head", "ask", 0); !errors.Is(err, ErrRollbackCutUnresolvable) {
		t.Fatalf("err = %v, want ErrRollbackCutUnresolvable", err)
	}
}

func TestPromptMatchesAcceptsCompactionCarrierSuffix(t *testing.T) {
	if !promptMatches("earlier summary\n\nthe live ask", "the live ask") {
		t.Fatal("carrier suffix should match")
	}
	if !promptMatches("the live ask", "the  live\nask") {
		t.Fatal("whitespace differences should not matter")
	}
	if promptMatches("unrelated", "the live ask") {
		t.Fatal("unrelated text must not match")
	}
	if promptMatches("", "ask") {
		t.Fatal("empty native content must not match")
	}
}
