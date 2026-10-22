package chat

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestRollbackRoutesToTheHermesGlueWhenHandled(t *testing.T) {
	svc := New(Options{})
	var gotID domain.SessionID
	var gotTurn string
	svc.SetRollbackHermes(func(_ context.Context, id domain.SessionID, turnID string) (int, bool, error) {
		gotID, gotTurn = id, turnID
		return 5, true, nil
	})
	discarded, err := svc.Rollback(context.Background(), "s-1", "t-9")
	if err != nil || discarded != 5 {
		t.Fatalf("Rollback = %d, %v; want 5, nil", discarded, err)
	}
	if gotID != "s-1" || gotTurn != "t-9" {
		t.Fatalf("glue got %s/%s", gotID, gotTurn)
	}
}

func TestRollbackGlueErrorIsSurfacedUntouched(t *testing.T) {
	svc := New(Options{})
	want := errors.New("cut unresolved on the native transcript")
	svc.SetRollbackHermes(func(context.Context, domain.SessionID, string) (int, bool, error) {
		return 0, true, want
	})
	if _, err := svc.Rollback(context.Background(), "s-1", "t-9"); !errors.Is(err, want) {
		t.Fatalf("err = %v, want the glue error", err)
	}
}
