-- +goose Up
-- +goose StatementBegin

CREATE TABLE terms_documents (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    version      TEXT NOT NULL,
    content_url  TEXT NOT NULL DEFAULT '',
    content_hash TEXT NOT NULL DEFAULT '',
    published_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT terms_documents_version_unique UNIQUE (version)
);

INSERT INTO terms_documents (version, content_url, content_hash)
VALUES ('1.0', '', '');

-- Immutable once written: application code only ever INSERTs here, never
-- UPDATEs or DELETEs a row, so "who accepted what, when" can never be
-- retroactively rewritten.
CREATE TABLE terms_acceptances (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    user_id     UUID NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    document_id UUID NOT NULL REFERENCES terms_documents (id) ON DELETE RESTRICT,
    accepted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    metadata    JSONB NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX terms_acceptances_tenant_idx ON terms_acceptances (tenant_id);
CREATE INDEX terms_acceptances_user_idx ON terms_acceptances (user_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS terms_acceptances;
DROP TABLE IF EXISTS terms_documents;
-- +goose StatementEnd
