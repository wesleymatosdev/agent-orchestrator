-- +goose Up
-- +goose StatementBegin
-- The upstream hermes-agent-harness branch ships
-- session_interface_transitions.degraded_kind as migration 20261031021251693,
-- but the fork line already shipped the identical column as 20261026182402270.
-- Mark the upstream version applied whenever the column is physically present,
-- so a daemon built from the upstream branch opens this database instead of
-- failing on a duplicate column name. The column guard leaves databases
-- without the column untouched (they still get it from the real migration,
-- including fresh installs of the upstream branch, which never see this
-- fork-only file), and NOT EXISTS keeps the ledger row single.
INSERT INTO goose_db_version (version_id, is_applied)
SELECT 20261031021251693, 1
WHERE EXISTS (
    SELECT 1 FROM pragma_table_info('session_interface_transitions')
    WHERE name = 'degraded_kind'
)
AND NOT EXISTS (
    SELECT 1 FROM goose_db_version WHERE version_id = 20261031021251693
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM goose_db_version WHERE version_id = 20261031021251693;
-- +goose StatementEnd
