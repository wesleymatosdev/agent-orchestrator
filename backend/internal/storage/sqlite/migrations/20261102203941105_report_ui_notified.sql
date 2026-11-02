-- +goose Up
-- +goose StatementBegin
-- When the delivery coordinator emitted the batch as a UI-visible conversation
-- notice. Chat orchestrators keep their reports pending after this so the next
-- owner turn still piggybacks the agent-context envelope; the marker only retires
-- the row from the coordinator's time-driven schedule.
ALTER TABLE reports ADD COLUMN ui_notified_at TIMESTAMP;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE reports DROP COLUMN ui_notified_at;
-- +goose StatementEnd

