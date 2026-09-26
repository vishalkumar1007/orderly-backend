-- +goose Up
-- +goose StatementBegin
CREATE UNIQUE INDEX users_one_super_admin_idx ON users (role)
WHERE role = 'SUPER_ADMIN' AND tenant_id IS NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS users_one_super_admin_idx;
-- +goose StatementEnd
