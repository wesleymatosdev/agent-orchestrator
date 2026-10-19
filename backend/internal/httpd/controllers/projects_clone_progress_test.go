package controllers_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
)

// stubCloneProgressManager serves only the clone progress stream; the
// embedded nil Manager keeps every other project route out of scope.
type stubCloneProgressManager struct {
	projectsvc.Manager
	events chan projectsvc.CloneProgressEvent
}

func (s stubCloneProgressManager) SubscribeCloneProgress(context.Context) <-chan projectsvc.CloneProgressEvent {
	return s.events
}

func newCloneProgressServer(t *testing.T, events chan projectsvc.CloneProgressEvent) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{
		Projects: stubCloneProgressManager{events: events},
	}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)
	return srv
}

// TestProjectsAPI_CloneProgressStreamRelaysEvents locks the SSE contract the
// clone dialog depends on: frames named clone_progress whose data is the
// CloneProgressEvent JSON, plus heartbeats to keep proxies from buffering.
func TestProjectsAPI_CloneProgressStreamRelaysEvents(t *testing.T) {
	events := make(chan projectsvc.CloneProgressEvent, 4)
	srv := newCloneProgressServer(t, events)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/projects/clone/progress", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET clone/progress: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	percent := 43.0
	events <- projectsvc.CloneProgressEvent{
		RemoteURL: "https://example.com/owner/repo.git",
		Stage:     projectsvc.CloneStageReceiving,
		Percent:   &percent,
		Message:   "Receiving objects:  43% (12345/28700)",
	}

	scanner := bufio.NewScanner(resp.Body)
	var payload projectsvc.CloneProgressEvent
	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for a clone_progress frame")
		}
		if !scanner.Scan() {
			t.Fatalf("stream closed before a clone_progress frame: %v", scanner.Err())
		}
		line := scanner.Text()
		switch line {
		case "event: clone_progress":
			if !scanner.Scan() {
				t.Fatal("stream closed before frame data")
			}
			data, ok := strings.CutPrefix(scanner.Text(), "data: ")
			if !ok {
				t.Fatalf("frame data = %q, want a data: line", scanner.Text())
			}
			if err := json.Unmarshal([]byte(data), &payload); err != nil {
				t.Fatalf("decode frame %q: %v", data, err)
			}
			if payload.RemoteURL != "https://example.com/owner/repo.git" || payload.Stage != projectsvc.CloneStageReceiving {
				t.Fatalf("frame = %+v, want the receiving event", payload)
			}
			if payload.Percent == nil || *payload.Percent != 43 {
				t.Fatalf("frame percent = %v, want 43", payload.Percent)
			}
			return
		default:
			// heartbeat comments (": heartbeat") and blank lines: keep scanning
		}
	}
}

// Without a project manager the stream answers the standard 501 envelope, not
// a hung connection.
func TestProjectsAPI_CloneProgressStreamWithoutManagerIsNotImplemented(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/projects/clone/progress")
	if err != nil {
		t.Fatalf("GET clone/progress: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 501; body=%s ct=%s", resp.StatusCode, string(body), resp.Header.Get("Content-Type"))
	}
	body := make([]byte, 512)
	n, _ := resp.Body.Read(body)
	if !strings.Contains(string(body[:n]), "NOT_IMPLEMENTED") {
		t.Fatalf("body = %q, want the NOT_IMPLEMENTED envelope", string(body[:n]))
	}
}
