-- A purchase that cost the customer nothing is still a purchase. A full
-- discount, or a credit that covers the whole price, both settle at zero,
-- and the provider decides that, not this platform. The constraint that
-- demanded a positive amount refused those rows and with them the
-- customer's subscription.
--
-- What remains is arithmetic that must hold for the row to mean anything:
-- nothing negative, and tax no larger than the amount it was charged on.
ALTER TABLE billing_purchase_funding
    DROP CONSTRAINT IF EXISTS billing_purchase_funding_amount_valid;

ALTER TABLE billing_purchase_funding
    ADD CONSTRAINT billing_purchase_funding_amount_valid
    CHECK (gross >= 0 AND tax >= 0 AND tax <= gross AND discount >= 0);
