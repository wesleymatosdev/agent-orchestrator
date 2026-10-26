-- +goose Up
-- +goose StatementBegin
-- A degraded handoff completes by starting the target fresh instead of
-- transferring native history. The kind records why ("identity-reset": the
-- installed harness adapter never declared a compatible TUI+Chat identity
-- pair), so status/marker/UI render the boundary from the same durable row.
ALTER TABLE session_interface_transitions ADD COLUMN degraded_kind TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE session_interface_transitions DROP COLUMN degraded_kind;
-- +goose StatementEnd
