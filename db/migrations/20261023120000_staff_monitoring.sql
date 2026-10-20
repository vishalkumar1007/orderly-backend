-- +goose Up
-- +goose StatementBegin

-- Staff monitoring: when someone last signed in, from where each live session
-- came, and who moved an order (for per-person stats).
ALTER TABLE users ADD COLUMN last_login_at TIMESTAMPTZ;
ALTER TABLE refresh_tokens ADD COLUMN ip TEXT NOT NULL DEFAULT '';
ALTER TABLE refresh_tokens ADD COLUMN user_agent TEXT NOT NULL DEFAULT '';
CREATE INDEX audit_logs_tenant_user_idx ON audit_logs (tenant_id, user_id, created_at DESC);

ALTER TABLE order_status_history ADD COLUMN actor_user_id UUID REFERENCES users (id) ON DELETE SET NULL;
CREATE INDEX order_status_history_actor_idx
    ON order_status_history (tenant_id, actor_user_id, created_at)
    WHERE actor_user_id IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS order_status_history_actor_idx;
ALTER TABLE order_status_history DROP COLUMN IF EXISTS actor_user_id;
DROP INDEX IF EXISTS audit_logs_tenant_user_idx;
ALTER TABLE refresh_tokens DROP COLUMN IF EXISTS user_agent;
ALTER TABLE refresh_tokens DROP COLUMN IF EXISTS ip;
ALTER TABLE users DROP COLUMN IF EXISTS last_login_at;
-- +goose StatementEnd
