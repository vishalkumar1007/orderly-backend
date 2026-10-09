-- +goose Up
-- +goose StatementBegin

-- Four new MFA-related security events, following the exact pattern the
-- existing SECURITY_MFA_ENABLED/DISABLED rows already use: IN_APP, ROLE:iam,
-- locked (a tenant cannot silently turn these off). notify.Dispatch silently
-- no-ops for any event code without a catalog row, so this is required, not
-- cosmetic.
INSERT INTO notification_events (code, category, label, description, default_channels, required_capability, variables) VALUES
    ('SECURITY_MFA_METHOD_ADDED', 'SECURITY', 'Two-factor method added', 'A second MFA method is added to an account that already has one.',
        ARRAY['IN_APP'], NULL, '["user_id","user_name","method"]'),
    ('SECURITY_MFA_RECOVERY_USED', 'SECURITY', 'Recovery code used', 'A recovery code is used to complete a login.',
        ARRAY['IN_APP'], NULL, '["user_id","user_name"]'),
    ('SECURITY_MFA_ADMIN_RESET', 'SECURITY', 'Two-factor reset by admin', 'An admin or support agent resets a locked-out user''s MFA.',
        ARRAY['IN_APP'], NULL, '["user_id","user_name"]'),
    ('SECURITY_MFA_POLICY_CHANGED', 'SECURITY', 'MFA policy changed', 'A business''s MFA policy (mode, methods, or enforcement) changes.',
        ARRAY['IN_APP'], NULL, '["tenant_id","tenant_name"]');

INSERT INTO notification_rules (event_code, channel, enabled, recipient_policy, priority, locked) VALUES
    ('SECURITY_MFA_METHOD_ADDED', 'IN_APP', TRUE, 'ROLE:iam', 'NORMAL', TRUE),
    ('SECURITY_MFA_RECOVERY_USED', 'IN_APP', TRUE, 'ROLE:iam', 'HIGH', TRUE),
    ('SECURITY_MFA_ADMIN_RESET', 'IN_APP', TRUE, 'ROLE:iam', 'HIGH', TRUE),
    ('SECURITY_MFA_POLICY_CHANGED', 'IN_APP', TRUE, 'ROLE:iam', 'NORMAL', TRUE);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM notification_rules WHERE event_code IN
    ('SECURITY_MFA_METHOD_ADDED', 'SECURITY_MFA_RECOVERY_USED', 'SECURITY_MFA_ADMIN_RESET', 'SECURITY_MFA_POLICY_CHANGED');
DELETE FROM notification_events WHERE code IN
    ('SECURITY_MFA_METHOD_ADDED', 'SECURITY_MFA_RECOVERY_USED', 'SECURITY_MFA_ADMIN_RESET', 'SECURITY_MFA_POLICY_CHANGED');
-- +goose StatementEnd
