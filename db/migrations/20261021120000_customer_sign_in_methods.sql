-- +goose Up
-- +goose StatementBegin

-- Customers can now sign in with an email code as well as a phone code. An
-- email-only customer has no phone, which the phone index already allows.
ALTER TABLE customers ADD COLUMN email TEXT;
CREATE UNIQUE INDEX customers_tenant_email_unique
    ON customers (tenant_id, lower(email))
    WHERE email IS NOT NULL;

-- Which sign-in methods the owner offers. A method is only shown to customers
-- when its channel (SMS for PHONE, email for EMAIL) is also set up.
-- customer_otp_codes.phone keeps the identifier the code was sent to: a phone
-- number or a lower-cased email address.
ALTER TABLE tenant_storefront_settings
    ADD COLUMN customer_login_methods TEXT[] NOT NULL DEFAULT '{PHONE,EMAIL}';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE tenant_storefront_settings DROP COLUMN IF EXISTS customer_login_methods;
DROP INDEX IF EXISTS customers_tenant_email_unique;
ALTER TABLE customers DROP COLUMN IF EXISTS email;
-- +goose StatementEnd
