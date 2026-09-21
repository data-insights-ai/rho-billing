-- A provider-side discount is a legitimate reason for what was collected to
-- differ from what was quoted, and the only one. Recording it keeps the
-- rule that every gap must be explained.
--
-- The amount check has to be relaxed with it: a fully discounted purchase
-- collects nothing, and gross 0 is the honest record of that. What must
-- never be zero is gross and discount together, because a purchase that
-- collected nothing and was not discounted is not a purchase.
ALTER TABLE billing_purchase_funding
    ADD COLUMN IF NOT EXISTS discount bigint NOT NULL DEFAULT 0;

ALTER TABLE billing_purchase_funding
    DROP CONSTRAINT IF EXISTS billing_purchase_funding_amount_valid;

ALTER TABLE billing_purchase_funding
    ADD CONSTRAINT billing_purchase_funding_amount_valid
    CHECK (gross >= 0 AND tax >= 0 AND tax <= gross
           AND discount >= 0 AND gross + discount > 0);
