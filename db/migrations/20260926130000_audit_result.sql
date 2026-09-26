-- +goose Up
-- +goose StatementBegin
-- Audit events previously hard-coded result = 'SUCCESS' in the handler, so the
-- Failure/Denied filters could never match. Record it properly from now on.
ALTER TABLE audit_logs
    ADD COLUMN result TEXT NOT NULL DEFAULT 'SUCCESS';

ALTER TABLE audit_logs
    ADD CONSTRAINT audit_logs_result_check CHECK (result IN ('SUCCESS', 'FAILURE', 'DENIED'));

CREATE INDEX audit_logs_result_idx ON audit_logs (result, created_at DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS audit_logs_result_idx;
ALTER TABLE audit_logs DROP CONSTRAINT IF EXISTS audit_logs_result_check;
ALTER TABLE audit_logs DROP COLUMN IF EXISTS result;
-- +goose StatementEnd
