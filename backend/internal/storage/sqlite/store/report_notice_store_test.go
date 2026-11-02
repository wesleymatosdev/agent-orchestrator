package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func TestAppendConversationNoticeWritesTurnlessIdempotentMessage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedProject(t, s, "ao")
	sess, err := s.CreateSession(ctx, sampleRecord("ao"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	conversation, err := s.CreateConversation(ctx, "conv-notice", domain.ConversationScopeSession, "ao", sess.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	msg := domain.ConversationMessage{
		ID:              "msg-notice-1",
		Text:            "Reports since your previous turn:\n\n[done] [worker](ao://sessions/ao/worker)",
		Origin:          domain.MessageOriginAutomation,
		ClientMessageID: "report-card:report-batch:rpt_1",
	}
	created, err := s.AppendConversationNotice(ctx, conversation.ID, sess.ID, msg, now)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	// The notice has no turn: it must not read as a provider dispatch.
	messages, err := s.ConversationMessages(ctx, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(messages))
	}
	got := messages[0]
	if got.Role != domain.MessageRoleUser || got.Origin != domain.MessageOriginAutomation || got.TurnID != "" {
		t.Fatalf("notice message = %+v", got)
	}
	// A repeated notice for the same batch deduplicates.
	created, err = s.AppendConversationNotice(ctx, conversation.ID, sess.ID, msg, now.Add(time.Second))
	if err != nil || created {
		t.Fatalf("duplicate created=%v err=%v", created, err)
	}
	messages, err = s.ConversationMessages(ctx, conversation.ID)
	if err != nil || len(messages) != 1 {
		t.Fatalf("messages after duplicate = %d err=%v", len(messages), err)
	}
}

func TestReportStoreScheduleExcludesUINotifiedRows(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedProject(t, s, "ao")
	sess, err := s.CreateSession(ctx, sampleRecord("ao"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	rec := domain.ReportRecord{
		ID: "rpt_notice", SessionID: sess.ID, ProjectID: sess.ProjectID,
		State: domain.ReportDone, Note: "finished",
		CreatedAt: now, DeliveryState: domain.ReportPending,
		AvailableAt: now, SettlementDeadline: now.Add(domain.ReportSettlementWindow),
		RepeatCount: 1,
	}
	if _, err = s.CreateReport(ctx, rec); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimPendingReportBatch(ctx, sess.ProjectID, "token", now)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %d reports, err = %v", len(claimed), err)
	}
	if _, err = s.MarkReportBatchUINotified(ctx, sess.ProjectID, "token", now); err != nil {
		t.Fatal(err)
	}
	schedule, err := s.ListPendingReportSchedule(ctx, 10)
	if err != nil || len(schedule) != 0 {
		t.Fatalf("schedule after ui notice = %+v err = %v", schedule, err)
	}
	// Released back to pending, the batch still reaches piggyback.
	released, err := s.ReleaseReportBatch(ctx, sess.ProjectID, "token", now, "")
	if err != nil || released != 1 {
		t.Fatalf("release = %d err = %v", released, err)
	}
	batch, err := s.ClaimPendingReportBatch(ctx, sess.ProjectID, "piggyback", now)
	if err != nil || len(batch) != 1 || batch[0].UINotifiedAt.IsZero() {
		t.Fatalf("piggyback claim = %+v err = %v", batch, err)
	}
}
