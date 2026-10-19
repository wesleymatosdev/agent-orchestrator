package acp

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"
)

// A mid-update gateway refusal is transient, so the normalized error must read
// as "wait and retry" while still carrying the raw RPC payload for diagnostics.
func TestNormalizeACPErrorExplainsGatewayDraining(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			"reason in message",
			&acpsdk.RequestError{Code: -32603, Message: "Gateway draining: update_paused"},
		},
		{
			// Exact shape Hermes answers with while an update owns the
			// install: reason buried in the data payload.
			"reason in data details",
			&acpsdk.RequestError{
				Code:    -32603,
				Message: "Internal error",
				Data:    map[string]any{"details": "Gateway draining: update_paused"},
			},
		},
		{
			"wrapped rpc failure",
			fmt.Errorf("dial: %w", &acpsdk.RequestError{
				Code:    -32603,
				Message: "Internal error",
				Data:    map[string]any{"details": "Gateway draining: update_paused"},
			}),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeACPError("ACP session/new", tc.err)
			if !strings.Contains(got.Error(), "gateway is updating itself") {
				t.Fatalf("normalizeACPError = %q, want retry hint", got)
			}
			if !strings.Contains(got.Error(), "try again") {
				t.Fatalf("normalizeACPError = %q, want try-again guidance", got)
			}
			if !strings.Contains(got.Error(), "update_paused") {
				t.Fatalf("normalizeACPError = %q, want original provider detail preserved", got)
			}
		})
	}
}

// Unrelated failures keep their plain shape: no retry hint, no swallowing.
func TestNormalizeACPErrorLeavesOtherFailuresAlone(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"plain error", errors.New("connection refused")},
		{"internal error without drain reason", &acpsdk.RequestError{Code: -32603, Message: "Internal error"}},
		{"drain reason on a non-internal code", &acpsdk.RequestError{Code: -32601, Message: "Gateway draining: update_paused"}},
		{"nil data map", &acpsdk.RequestError{Code: -32603, Message: "prompt failed", Data: map[string]any{"n": 1}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeACPError("ACP session/new", tc.err)
			if strings.Contains(got.Error(), "try again") {
				t.Fatalf("normalizeACPError = %q, want no retry hint", got)
			}
			want := "ACP session/new: " + tc.err.Error()
			if got.Error() != want {
				t.Fatalf("normalizeACPError = %q, want %q", got, want)
			}
		})
	}
}

// An auth refusal must still win over the drain mapping so the reauth prompt
// is not replaced by a retry hint.
func TestNormalizeACPErrorAuthRequiredStillWins(t *testing.T) {
	err := &acpsdk.RequestError{Code: -32000, Message: "authentication required"}
	got := normalizeACPError("ACP session/new", err)
	if !strings.Contains(got.Error(), "authentication required") || strings.Contains(got.Error(), "try again") {
		t.Fatalf("normalizeACPError = %q, want auth-required mapping without retry hint", got)
	}
}
