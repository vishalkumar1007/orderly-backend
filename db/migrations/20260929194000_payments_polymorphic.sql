-- +goose Up
-- +goose StatementBegin

-- Payment is a common platform capability but must not be hard-coupled to
-- "an order" (HLD §15) — Barber pays against a service/appointment, Hotel
-- pays a deposit and later a balance against a reservation/folio. Make the
-- payable side polymorphic instead of a fixed FK.
ALTER TABLE payments
    ADD COLUMN payable_type TEXT,
    ADD COLUMN payable_id   UUID,
    ADD COLUMN purpose      TEXT NOT NULL DEFAULT 'FULL';

-- A BEFORE INSERT trigger, not an app-code change: every existing insert
-- path (CreatePayment) only ever sets order_id, and must keep working
-- unmodified. New modules (appointments, reservations/folios) insert
-- payable_type/payable_id directly and never touch order_id.
CREATE OR REPLACE FUNCTION payments_default_payable() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.payable_id IS NULL AND NEW.order_id IS NOT NULL THEN
        NEW.payable_type := 'ORDER';
        NEW.payable_id := NEW.order_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER payments_default_payable_trigger
    BEFORE INSERT ON payments
    FOR EACH ROW EXECUTE FUNCTION payments_default_payable();

UPDATE payments SET payable_type = 'ORDER', payable_id = order_id WHERE payable_id IS NULL;

ALTER TABLE payments
    ALTER COLUMN payable_type SET NOT NULL,
    ALTER COLUMN payable_id SET NOT NULL;

ALTER TABLE payments
    ADD CONSTRAINT payments_payable_type_check
        CHECK (payable_type IN ('ORDER', 'APPOINTMENT', 'RESERVATION', 'FOLIO')),
    ADD CONSTRAINT payments_purpose_check
        CHECK (purpose IN ('FULL', 'DEPOSIT', 'BALANCE', 'ADDITIONAL'));

-- Old rule was "one payment per order" (payments_order_id_unique). The new
-- rule is "one payment per (payable, purpose)" — Food/Barber/Cafe/Restaurant
-- keep exactly one FULL row per payable, same as today; Hotel gets to record
-- a DEPOSIT and a BALANCE as two distinct, auditable rows against the same
-- reservation/folio instead of overwriting one row and losing the deposit.
ALTER TABLE payments DROP CONSTRAINT payments_order_id_unique;
CREATE UNIQUE INDEX payments_payable_purpose_unique
    ON payments (payable_type, payable_id, purpose);

-- order_id stays as a convenience column for existing Food code paths (it is
-- still populated by the trigger for every ORDER payment); it just stops
-- being mandatory so a non-order payable can omit it.
ALTER TABLE payments DROP CONSTRAINT payments_order_id_fkey;
ALTER TABLE payments ALTER COLUMN order_id DROP NOT NULL;
ALTER TABLE payments
    ADD CONSTRAINT payments_order_id_fkey FOREIGN KEY (order_id) REFERENCES orders (id) ON DELETE RESTRICT;

CREATE INDEX payments_payable_idx ON payments (payable_type, payable_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS payments_payable_idx;

ALTER TABLE payments DROP CONSTRAINT IF EXISTS payments_order_id_fkey;
ALTER TABLE payments ALTER COLUMN order_id SET NOT NULL;
ALTER TABLE payments
    ADD CONSTRAINT payments_order_id_fkey FOREIGN KEY (order_id) REFERENCES orders (id) ON DELETE RESTRICT;

DROP INDEX IF EXISTS payments_payable_purpose_unique;
ALTER TABLE payments ADD CONSTRAINT payments_order_id_unique UNIQUE (order_id);

ALTER TABLE payments
    DROP CONSTRAINT IF EXISTS payments_purpose_check,
    DROP CONSTRAINT IF EXISTS payments_payable_type_check;

DROP TRIGGER IF EXISTS payments_default_payable_trigger ON payments;
DROP FUNCTION IF EXISTS payments_default_payable();

ALTER TABLE payments
    DROP COLUMN IF EXISTS purpose,
    DROP COLUMN IF EXISTS payable_id,
    DROP COLUMN IF EXISTS payable_type;

-- +goose StatementEnd
