package project

import (
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// installCloneGitShim puts a fake `git` first on PATH for the duration of the
// test, so clone behaviour (stderr output, hanging, failing) can be scripted
// without any network access. The real git stays reachable for helpers that
// build source repositories.
func installCloneGitShim(t *testing.T, cloneScript string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\n"+cloneScript), 0o755); err != nil {
		t.Fatalf("write git shim: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// shallowSourceRepo creates a real local git repository with one commit so a
// file:// clone exercises the real binary end to end.
func shallowSourceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init", "-b", "main", ".")
	run("config", "user.email", "ao@example.com")
	run("config", "user.name", "AO Test")
	run("commit", "--allow-empty", "-m", "seed")
	return dir
}

func cloneInputFor(t *testing.T, remoteURL string) CloneInput {
	t.Helper()
	return CloneInput{RemoteURL: remoteURL, DestinationParent: t.TempDir()}
}

func apiErr(t *testing.T, err error) *apierr.Error {
	t.Helper()
	var e *apierr.Error
	if !errors.As(err, &e) {
		t.Fatalf("error %v (%T) is not an *apierr.Error", err, err)
	}
	return e
}

// A request budget that fires mid-transfer cancels the clone; the error
// envelope must carry what git managed to print, not a bare message.
func TestPrepareCloneTimeoutCarriesGitStderrInEnvelope(t *testing.T) {
	installCloneGitShim(t, `if [ "$1" = "clone" ]; then
	echo "Receiving objects:  12% (3/25), 41.2 MiB | 5.5 MiB/s" >&2
	sleep 30
fi
exit 0
`)
	m := New(nil)
	// A generous budget keeps the test deterministic under load: the shim must
	// have printed its progress line before the deadline fires.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := m.PrepareClone(ctx, cloneInputFor(t, "https://example.com/owner/large-repo.git"))
	e := apiErr(t, err)
	if e.Code != "GIT_CLONE_TIMED_OUT" {
		t.Fatalf("code = %q, want GIT_CLONE_TIMED_OUT", e.Code)
	}
	stderr, _ := e.Details["gitStderr"].(string)
	if !strings.Contains(stderr, "Receiving objects:  12%") {
		t.Fatalf("gitStderr details = %q, want the git progress output", stderr)
	}
}

// A plain git failure surfaces the same way: the envelope carries git's own
// diagnosis instead of only the generic guidance sentence.
func TestPrepareCloneFailureCarriesGitStderrInEnvelope(t *testing.T) {
	installCloneGitShim(t, `if [ "$1" = "clone" ]; then
	echo "Cloning into 'snack'" >&2
	echo "fatal: repository 'https://example.com/owner/snack.git' not found" >&2
	exit 128
fi
exit 0
`)
	m := New(nil)

	_, err := m.PrepareClone(context.Background(), cloneInputFor(t, "https://example.com/owner/snack.git"))
	e := apiErr(t, err)
	if e.Code != "GIT_CLONE_FAILED" {
		t.Fatalf("code = %q, want GIT_CLONE_FAILED", e.Code)
	}
	stderr, _ := e.Details["gitStderr"].(string)
	if !strings.Contains(stderr, "fatal: repository") {
		t.Fatalf("gitStderr details = %q, want git's fatal output", stderr)
	}
}

// Git prints the remote URL verbatim in auth failures, which can carry
// embedded credentials; the envelope tail must redact them.
func TestPrepareCloneFailureRedactsURLCredentialsInGitStderr(t *testing.T) {
	installCloneGitShim(t, `if [ "$1" = "clone" ]; then
	echo "fatal: Authentication failed for https://user:secret@github.com/acme/private.git" >&2
	exit 128
fi
exit 0
`)
	m := New(nil)

	_, err := m.PrepareClone(context.Background(), cloneInputFor(t, "https://github.com/acme/private.git"))
	e := apiErr(t, err)
	stderr, _ := e.Details["gitStderr"].(string)
	if strings.Contains(stderr, "secret") {
		t.Fatalf("gitStderr details = %q, want the credential redacted", stderr)
	}
	if !strings.Contains(stderr, "https://user:***@github.com") {
		t.Fatalf("gitStderr details = %q, want the redacted URL form", stderr)
	}
}

// Cloning publishes progress frames to subscribers: parsed stage/percent from
// git's meter, then a terminal done frame when the checkout succeeds.
func TestPrepareClonePublishesProgressEvents(t *testing.T) {
	m := New(nil)
	source := shallowSourceRepo(t)
	remoteURL := (&url.URL{Scheme: "file", Path: source}).String()

	events := m.SubscribeCloneProgress(context.Background())
	in := cloneInputFor(t, remoteURL)
	if _, err := m.PrepareClone(context.Background(), in); err != nil {
		t.Fatalf("PrepareClone: %v", err)
	}

	var received []CloneProgressEvent
	timeout := time.After(2 * time.Second)
	for {
		select {
		case event := <-events:
			received = append(received, event)
			if event.Stage == CloneStageDone {
				if event.Percent == nil || *event.Percent != 100 {
					t.Fatalf("done event percent = %v, want 100", event.Percent)
				}
				if event.RemoteURL != remoteURL {
					t.Fatalf("done event remoteUrl = %q, want %q", event.RemoteURL, remoteURL)
				}
				if len(received) < 2 {
					t.Fatalf("received = %#v, want at least one progress frame before done", received)
				}
				if received[0].Stage != CloneStageStarting {
					t.Fatalf("first event stage = %q, want %q", received[0].Stage, CloneStageStarting)
				}
				return
			}
		case <-timeout:
			t.Fatalf("no clone_progress events within 2s; received = %#v", received)
		}
	}
}

// A failed clone also reaches subscribers with a terminal failed frame, so
// the UI can clear in-flight progress state.
func TestPrepareClonePublishesFailedProgressEvent(t *testing.T) {
	installCloneGitShim(t, `if [ "$1" = "clone" ]; then
	echo "fatal: repository not found" >&2
	exit 128
fi
exit 0
`)
	m := New(nil)
	remoteURL := "https://example.com/owner/snack.git"
	events := m.SubscribeCloneProgress(context.Background())

	if _, err := m.PrepareClone(context.Background(), cloneInputFor(t, remoteURL)); err == nil {
		t.Fatal("PrepareClone: want error")
	}

	select {
	case event := <-events:
		if event.Stage != CloneStageFailed {
			t.Fatalf("stage = %q, want %q", event.Stage, CloneStageFailed)
		}
		if event.RemoteURL != remoteURL {
			t.Fatalf("remoteUrl = %q, want %q", event.RemoteURL, remoteURL)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no failed progress event")
	}
}

// cloneProgressFromGitLine maps git's meter labels to stages and pulls the
// percentage out of the line.
func TestCloneProgressFromGitLine(t *testing.T) {
	cases := []struct {
		line    string
		stage   string
		percent float64
		hasPct  bool
	}{
		{line: "Cloning into '/tmp/x'...", stage: CloneStageStarting},
		{line: "Receiving objects:  43% (12345/28700), 100.5 MiB | 5.5 MiB/s", stage: CloneStageReceiving, percent: 43, hasPct: true},
		{line: "Resolving deltas: 100% (18000/18000), done.", stage: CloneStageResolving, percent: 100, hasPct: true},
		{line: "Updating files:  55% (11000/20000), done.", stage: CloneStageCheckingOut, percent: 55, hasPct: true},
		{line: "remote: Enumerating objects: 55, done.", stage: ""},
	}
	for _, tc := range cases {
		event, ok := cloneProgressFromGitLine("https://example.com/x.git", tc.line)
		if tc.stage == "" {
			if ok {
				t.Fatalf("line %q: want no event", tc.line)
			}
			continue
		}
		if !ok || event.Stage != tc.stage {
			t.Fatalf("line %q: stage = %q ok=%v, want %q", tc.line, event.Stage, ok, tc.stage)
		}
		if tc.hasPct && (event.Percent == nil || *event.Percent != tc.percent) {
			t.Fatalf("line %q: percent = %v, want %v", tc.line, event.Percent, tc.percent)
		}
	}
}

// The stderr tail is bounded: a multi-megabyte progress meter cannot balloon
// the error envelope, and the tail keeps the last lines.
func TestCloneStderrTailBounded(t *testing.T) {
	var tail cloneStderrTail
	for i := 0; i < 5000; i++ {
		tail.add(strings.Repeat("x", 120))
	}
	if got := tail.value(); len(got) > maxCloneStderrTail {
		t.Fatalf("tail length = %d, want at most %d", len(got), maxCloneStderrTail)
	}
	if got := tail.value(); !strings.Contains(got, strings.Repeat("x", 120)) {
		t.Fatal("tail lost its last line")
	}
}

// The git shim must not leak into helpers that need the real binary.
func TestInstallCloneGitShimScopesToTest(t *testing.T) {
	installCloneGitShim(t, "exit 1")
	if err := exec.Command("git", "--version").Run(); err == nil {
		t.Fatal("git resolved past the shim")
	}
}
