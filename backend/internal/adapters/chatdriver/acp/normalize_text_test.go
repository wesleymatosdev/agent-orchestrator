package acp

import (
	"context"
	"net"
	"strings"
	"testing"

	"log/slog"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// newNormalizationConversation builds a conversation with a Z.ai-style label
// mapping installed, mirroring what the hermesacp binding wires in.
func newNormalizationConversation(t *testing.T) *conversation {
	t.Helper()
	daemon, host := net.Pipe()
	t.Cleanup(func() { _ = daemon.Close(); _ = host.Close() })
	conv := newConversation(&process{
		stdin: daemon, stdout: host, stop: daemon.Close,
	}, slog.New(slog.DiscardHandler), "", nil, nil)
	conv.normalizeText = strings.NewReplacer("内置工具：", "built-in tool: ", "输入：", "input: ").Replace
	return conv
}

func messageChunk(text string) acpsdk.SessionNotification {
	return acpsdk.SessionNotification{
		SessionId: "provider-session",
		Update: acpsdk.SessionUpdate{AgentMessageChunk: &acpsdk.SessionUpdateAgentMessageChunk{
			SessionUpdate: "agent_message_chunk",
			Content:       acpsdk.ContentBlock{Text: &acpsdk.ContentBlockText{Text: text}},
		}},
	}
}

func TestNormalizeTextRewritesMessageDeltas(t *testing.T) {
	conv := newNormalizationConversation(t)
	if err := conv.processUpdate(messageChunk("🌐 Z.ai 内置工具：analyze_image / 输入：{}")); err != nil {
		t.Fatalf("processUpdate: %v", err)
	}
	event := nextEvent(t, conv.Events())
	if event.Kind != ports.ChatEventMessageDelta || event.Delta != "🌐 Z.ai built-in tool: analyze_image / input: {}" {
		t.Fatalf("message delta = %#v", event)
	}
}

func TestNormalizeTextRewritesThoughtDeltas(t *testing.T) {
	conv := newNormalizationConversation(t)
	if err := conv.processUpdate(acpsdk.SessionNotification{
		SessionId: "provider-session",
		Update: acpsdk.SessionUpdate{AgentThoughtChunk: &acpsdk.SessionUpdateAgentThoughtChunk{
			SessionUpdate: "agent_thought_chunk",
			Content:       acpsdk.ContentBlock{Text: &acpsdk.ContentBlockText{Text: "输入：x"}},
		}},
	}); err != nil {
		t.Fatalf("processUpdate: %v", err)
	}
	event := nextEvent(t, conv.Events())
	if event.Kind != ports.ChatEventActivityStarted {
		t.Fatalf("thought start = %#v", event)
	}
	delta := nextEvent(t, conv.Events())
	if delta.Kind != ports.ChatEventReasoningDelta || delta.Delta != "input: x" {
		t.Fatalf("thought delta = %#v", delta)
	}
}

func TestNormalizeTextRewritesToolCallTitles(t *testing.T) {
	conv := newNormalizationConversation(t)
	title := "🌐 Z.ai 内置工具：analyze_image"
	if err := conv.processUpdate(acpsdk.SessionNotification{
		SessionId: "provider-session",
		Update: acpsdk.SessionUpdate{ToolCall: &acpsdk.SessionUpdateToolCall{
			SessionUpdate: "tool_call", ToolCallId: "tool-1", Title: title,
			Kind: acpsdk.ToolKindRead, Status: acpsdk.ToolCallStatusInProgress,
		}},
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}
	started := nextEvent(t, conv.Events())
	if started.Kind != ports.ChatEventActivityStarted || started.Summary != "🌐 Z.ai built-in tool: analyze_image" {
		t.Fatalf("tool start = %#v", started)
	}

	updated := "输入：replaced"
	if err := conv.processUpdate(acpsdk.SessionNotification{
		SessionId: "provider-session",
		Update: acpsdk.SessionUpdate{ToolCallUpdate: &acpsdk.SessionToolCallUpdate{
			SessionUpdate: "tool_call_update", ToolCallId: "tool-1", Title: &updated,
		}},
	}); err != nil {
		t.Fatalf("tool update: %v", err)
	}
	replacement := nextEvent(t, conv.Events())
	if replacement.Kind != ports.ChatEventActivityStarted || replacement.Summary != "input: replaced" {
		t.Fatalf("tool update = %#v", replacement)
	}
}

func TestNormalizeTextRewritesApprovalSummary(t *testing.T) {
	conv := newNormalizationConversation(t)
	allow := acpsdk.PermissionOption{OptionId: "allow", Name: "Allow", Kind: acpsdk.PermissionOptionKindAllowOnce}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// The request parks and emits its card before the cancelled context ends it.
	if _, err := conv.RequestPermission(ctx, acpsdk.RequestPermissionRequest{
		Options: []acpsdk.PermissionOption{allow},
		ToolCall: acpsdk.ToolCallUpdate{
			Title: &[]string{"🌐 Z.ai 内置工具：rm"}[0],
			Kind:  func() *acpsdk.ToolKind { k := acpsdk.ToolKindExecute; return &k }(),
		},
	}); err != nil {
		t.Fatalf("RequestPermission: %v", err)
	}
	request := nextEvent(t, conv.Events())
	if request.Kind != ports.ChatEventApprovalRequested {
		t.Fatalf("approval event = %#v", request)
	}
	if request.Summary != "🌐 Z.ai built-in tool: rm" {
		t.Fatalf("approval summary = %q, want the English-mapped title", request.Summary)
	}
}

func TestNormalizeTextNilMappingLeavesTextUntouched(t *testing.T) {
	daemon, host := net.Pipe()
	defer func() { _ = daemon.Close(); _ = host.Close() }()
	conv := newConversation(&process{
		stdin: daemon, stdout: host, stop: daemon.Close,
	}, slog.New(slog.DiscardHandler), "", nil, nil)
	if err := conv.processUpdate(messageChunk("内置工具：raw")); err != nil {
		t.Fatalf("processUpdate: %v", err)
	}
	event := nextEvent(t, conv.Events())
	if event.Kind != ports.ChatEventMessageDelta || event.Delta != "内置工具：raw" {
		t.Fatalf("message delta = %#v", event)
	}
}

func TestNormalizeTextAppliesToCompletedAccumulatedText(t *testing.T) {
	conv := newNormalizationConversation(t)
	// A label split across two deltas escapes per-delta normalization; the
	// completed-message pass must still translate it.
	if err := conv.processUpdate(messageChunk("内置")); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	nextEvent(t, conv.Events())
	if err := conv.processUpdate(messageChunk("工具：x")); err != nil {
		t.Fatalf("second chunk: %v", err)
	}
	nextEvent(t, conv.Events())
	conv.settleOpenItems("turn-1")
	var completed bool
	for event := range conv.Events() {
		if event.Kind != ports.ChatEventMessageCompleted {
			continue
		}
		completed = true
		if event.Text != "built-in tool: x" {
			t.Fatalf("completed text = %q, want %q", event.Text, "built-in tool: x")
		}
		break
	}
	if !completed {
		t.Fatal("no completed message event emitted")
	}
}
