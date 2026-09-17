-- 001_release.sql
CREATE TABLE billing_accounts (
    account_id text PRIMARY KEY,
    subject text NOT NULL,
    next_journal_sequence bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT billing_accounts_account_id_valid CHECK (account_id <> ''),
    CONSTRAINT billing_accounts_subject_valid CHECK (subject <> ''),
    CONSTRAINT billing_accounts_subject_unique UNIQUE (subject),
    CONSTRAINT billing_accounts_sequence_valid CHECK (next_journal_sequence >= 0)
);

CREATE TABLE billing_credit_units (
    unit_code text PRIMARY KEY,
    unit_scale bigint NOT NULL,
    CONSTRAINT billing_credit_units_code_valid CHECK (unit_code <> ''),
    CONSTRAINT billing_credit_units_scale_valid CHECK (unit_scale > 0),
    CONSTRAINT billing_credit_units_code_scale_unique UNIQUE (unit_code, unit_scale)
);

CREATE TABLE billing_lots (
    account_id text NOT NULL,
    lot_id text NOT NULL,
    unit_code text NOT NULL,
    unit_scale bigint NOT NULL,
    scope text NOT NULL,
    source text NOT NULL,
    source_ref text NOT NULL,
    valid_from timestamptz NOT NULL,
    expires_at timestamptz,
    granted_at timestamptz NOT NULL,
    revoked_at timestamptz,
    initial bigint NOT NULL,
    available bigint NOT NULL,
    held bigint NOT NULL,
    consumed bigint NOT NULL,
    expired bigint NOT NULL,
    revoked bigint NOT NULL,
    pending_revocation bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (account_id, lot_id),
    CONSTRAINT billing_lots_source_unique UNIQUE (account_id, unit_code, source, source_ref),
    CONSTRAINT billing_lots_account_fk FOREIGN KEY (account_id)
        REFERENCES billing_accounts (account_id),
    CONSTRAINT billing_lots_unit_fk FOREIGN KEY (unit_code, unit_scale)
        REFERENCES billing_credit_units (unit_code, unit_scale),
    CONSTRAINT billing_lots_unit_valid CHECK (unit_code <> '' AND unit_scale > 0),
    CONSTRAINT billing_lots_source_valid CHECK (source <> '' AND source_ref <> ''),
    CONSTRAINT billing_lots_id_valid CHECK (lot_id <> ''),
    CONSTRAINT billing_lots_interval_valid CHECK (expires_at IS NULL OR expires_at > valid_from),
    CONSTRAINT billing_lots_initial_valid CHECK (initial >= 0),
    CONSTRAINT billing_lots_available_valid CHECK (available >= 0),
    CONSTRAINT billing_lots_held_valid CHECK (held >= 0),
    CONSTRAINT billing_lots_consumed_valid CHECK (consumed >= 0),
    CONSTRAINT billing_lots_expired_valid CHECK (expired >= 0),
    CONSTRAINT billing_lots_revoked_valid CHECK (revoked >= 0),
    CONSTRAINT billing_lots_pending_revocation_valid CHECK (pending_revocation >= 0 AND pending_revocation <= held),
    CONSTRAINT billing_lots_conservation CHECK (
        initial::numeric = available::numeric + held::numeric + consumed::numeric
            + expired::numeric + revoked::numeric
    )
);

CREATE TABLE billing_reservations (
    account_id text NOT NULL,
    reservation_id text NOT NULL,
    actor text NOT NULL,
    unit text NOT NULL,
    scope text NOT NULL,
    created_at timestamptz NOT NULL,
    deadline timestamptz NOT NULL,
    state text NOT NULL,
    authorized bigint NOT NULL,
    consumed bigint NOT NULL,
    limit_period_start timestamptz,
    limit_period_end timestamptz,
    evidence jsonb NOT NULL,
    PRIMARY KEY (account_id, reservation_id),
    CONSTRAINT billing_reservations_account_fk FOREIGN KEY (account_id)
        REFERENCES billing_accounts (account_id),
    CONSTRAINT billing_reservations_id_valid CHECK (reservation_id <> ''),
    CONSTRAINT billing_reservations_state_valid CHECK (
        state IN ('held', 'settled', 'released', 'timed_out', 'cancelled')
    ),
    CONSTRAINT billing_reservations_amount_valid CHECK (
        authorized >= 0 AND consumed >= 0 AND consumed <= authorized
    ),
    CONSTRAINT billing_reservations_limit_period_valid CHECK (
        (limit_period_start IS NULL AND limit_period_end IS NULL)
        OR (limit_period_start IS NOT NULL AND limit_period_end IS NOT NULL
            AND limit_period_end > limit_period_start)
    )
);

CREATE TABLE billing_reservation_allocations (
    account_id text NOT NULL,
    reservation_id text NOT NULL,
    position bigint NOT NULL,
    lot_id text NOT NULL,
    amount bigint NOT NULL,
    PRIMARY KEY (account_id, reservation_id, position),
    CONSTRAINT billing_reservation_allocations_reservation_fk
        FOREIGN KEY (account_id, reservation_id)
        REFERENCES billing_reservations (account_id, reservation_id)
        ON DELETE CASCADE,
    CONSTRAINT billing_reservation_allocations_position_valid CHECK (position >= 0),
    CONSTRAINT billing_reservation_allocations_lot_fk
        FOREIGN KEY (account_id, lot_id)
        REFERENCES billing_lots (account_id, lot_id),
    CONSTRAINT billing_reservation_allocations_amount_valid CHECK (amount > 0)
);

CREATE TABLE billing_limits (
    account_id text NOT NULL,
    actor text NOT NULL,
    unit text NOT NULL,
    period_start timestamptz NOT NULL,
    period_end timestamptz NOT NULL,
    amount bigint NOT NULL,
    PRIMARY KEY (account_id, actor, unit, period_start),
    CONSTRAINT billing_limits_account_fk FOREIGN KEY (account_id)
        REFERENCES billing_accounts (account_id),
    CONSTRAINT billing_limits_period_valid CHECK (period_end > period_start),
    CONSTRAINT billing_limits_amount_valid CHECK (amount >= 0)
);

CREATE TABLE billing_operations (
    account_id text NOT NULL,
    operation_id text NOT NULL,
    fingerprint text NOT NULL,
    error text NOT NULL,
    result jsonb NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (account_id, operation_id),
    CONSTRAINT billing_operations_account_fk FOREIGN KEY (account_id)
        REFERENCES billing_accounts (account_id),
    CONSTRAINT billing_operations_id_valid CHECK (operation_id <> ''),
    CONSTRAINT billing_operations_fingerprint_valid CHECK (fingerprint <> '')
);

CREATE TABLE billing_journal (
    account_id text NOT NULL,
    sequence bigint NOT NULL,
    operation_id text NOT NULL,
    lot_id text,
    reservation_id text,
    kind text NOT NULL,
    reason text NOT NULL,
    recorded_at timestamptz NOT NULL,
    effective_at timestamptz NOT NULL,
    available_delta bigint NOT NULL,
    held_delta bigint NOT NULL,
    consumed_delta bigint NOT NULL,
    expired_delta bigint NOT NULL,
    revoked_delta bigint NOT NULL,
    pending_revocation_delta bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (account_id, sequence),
    CONSTRAINT billing_journal_account_fk FOREIGN KEY (account_id)
        REFERENCES billing_accounts (account_id),
    CONSTRAINT billing_journal_sequence_valid CHECK (sequence > 0),
    CONSTRAINT billing_journal_operation_valid CHECK (operation_id <> ''),
    CONSTRAINT billing_journal_kind_valid CHECK (kind <> ''),
    CONSTRAINT billing_journal_lot_fk FOREIGN KEY (account_id, lot_id)
        REFERENCES billing_lots (account_id, lot_id),
    CONSTRAINT billing_journal_reservation_fk FOREIGN KEY (account_id, reservation_id)
        REFERENCES billing_reservations (account_id, reservation_id)
);

CREATE INDEX billing_journal_account_sequence_idx
    ON billing_journal (account_id, sequence);


CREATE TABLE billing_provider_refs (
 account_id text NOT NULL REFERENCES billing_accounts(account_id),
 provider text NOT NULL, merchant text NOT NULL, environment text NOT NULL,
 kind text NOT NULL, external_id text NOT NULL,
 PRIMARY KEY(provider,merchant,environment,kind,external_id)
);
CREATE TABLE billing_plans (
 id text PRIMARY KEY, plan_id text NOT NULL, version bigint NOT NULL CHECK(version>0),
 fingerprint text NOT NULL, definition jsonb NOT NULL,
 UNIQUE(plan_id,version)
);
CREATE TABLE billing_price_mappings (
 account_id text NOT NULL REFERENCES billing_accounts(account_id),
 provider text NOT NULL, merchant text NOT NULL, environment text NOT NULL,
 price_id text NOT NULL, revision bigint NOT NULL CHECK(revision>0),
 target_id text NOT NULL REFERENCES billing_plans(id), definition jsonb NOT NULL,
 PRIMARY KEY(account_id,provider,merchant,environment,price_id,revision)
);
CREATE TABLE billing_subscriptions (
 account_id text NOT NULL REFERENCES billing_accounts(account_id),
 provider text NOT NULL, merchant text NOT NULL, environment text NOT NULL,
 external_id text NOT NULL, revision bigint NOT NULL CHECK(revision>0),
 snapshot jsonb NOT NULL,
 PRIMARY KEY(account_id,provider,merchant,environment,external_id),
 UNIQUE(provider,merchant,environment,external_id)
);
CREATE TABLE billing_subscription_history (
 account_id text NOT NULL, provider text NOT NULL, merchant text NOT NULL,
 environment text NOT NULL, external_id text NOT NULL, revision bigint NOT NULL,
 snapshot jsonb NOT NULL,
 PRIMARY KEY(account_id,provider,merchant,environment,external_id,revision),
 FOREIGN KEY(account_id,provider,merchant,environment,external_id)
 REFERENCES billing_subscriptions(account_id,provider,merchant,environment,external_id)
);
CREATE TABLE billing_rating_rules (
 version text PRIMARY KEY, fingerprint text NOT NULL, definition jsonb NOT NULL
);
CREATE TABLE billing_usage (
 account_id text NOT NULL REFERENCES billing_accounts(account_id),
 usage_id text NOT NULL, source text NOT NULL, occurred_at timestamptz NOT NULL,
 interval_start timestamptz, interval_end timestamptz,
 funding text NOT NULL CHECK(funding IN ('prepaid','postpaid','waived')),
 fingerprint text NOT NULL, record jsonb NOT NULL,
 scope_provider text NOT NULL DEFAULT '', scope_merchant text NOT NULL DEFAULT '',
 scope_environment text NOT NULL DEFAULT '', scope_subscription_id text NOT NULL DEFAULT '',
 scope_item_id text NOT NULL DEFAULT '',
 PRIMARY KEY(account_id,usage_id),
 CHECK((interval_start IS NULL AND interval_end IS NULL) OR
  (interval_start IS NOT NULL AND interval_end>interval_start)),
 CHECK((scope_provider = '' AND scope_merchant = '' AND scope_environment = '' AND scope_subscription_id = '' AND scope_item_id = '') OR
  (scope_provider <> '' AND scope_merchant <> '' AND scope_environment <> '' AND scope_subscription_id <> '' AND scope_item_id <> ''))
);
CREATE INDEX billing_usage_period_idx ON billing_usage(account_id,occurred_at,usage_id);
CREATE INDEX billing_usage_aggregate_idx ON billing_usage(account_id,source,interval_start,interval_end);
-- Candidate scans use a stable bytewise cursor inside an account/source/scope.
-- Temporal columns can reject unrelated candidates from the index; total work
-- still depends on the number of matching candidates, not only page size.
CREATE INDEX billing_usage_candidates_idx ON billing_usage(
 account_id,source,scope_provider,scope_merchant,scope_environment,
 scope_subscription_id,scope_item_id,usage_id COLLATE "C")
 INCLUDE(interval_start,interval_end,occurred_at);
CREATE INDEX billing_usage_aggregate_candidates_idx ON billing_usage(
 account_id,source,scope_provider,scope_merchant,scope_environment,
 scope_subscription_id,scope_item_id,usage_id COLLATE "C")
 INCLUDE(interval_start,interval_end)
 WHERE interval_start IS NOT NULL;


CREATE TABLE billing_inbox (
    account_id text NOT NULL,
    message_id text NOT NULL,
    provider text NOT NULL,
    merchant text NOT NULL,
    environment text NOT NULL,
    kind text NOT NULL,
    direction text NOT NULL DEFAULT 'inbound',
    occurred_at timestamptz NOT NULL,
    payload bytea NOT NULL,
    fingerprint text NOT NULL,
    state text NOT NULL DEFAULT 'pending',
    attempts integer NOT NULL DEFAULT 0,
    available_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    lease_deadline timestamptz,
    worker text,
    fence bigint NOT NULL DEFAULT 0,
    last_error text NOT NULL DEFAULT '',
    received_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    processed_at timestamptz,
    payload_pruned_at timestamptz,
    PRIMARY KEY (account_id, message_id),
    CONSTRAINT billing_inbox_account_fk FOREIGN KEY (account_id)
        REFERENCES billing_accounts (account_id),
    CONSTRAINT billing_inbox_direction_valid CHECK (direction = 'inbound'),
    CONSTRAINT billing_inbox_state_valid CHECK (state IN ('pending', 'processing', 'processed', 'dead')),
    CONSTRAINT billing_inbox_attempts_valid CHECK (attempts >= 0),
    CONSTRAINT billing_inbox_fence_valid CHECK (fence >= 0),
    CONSTRAINT billing_inbox_claim_valid CHECK (
        (state = 'processing' AND worker IS NOT NULL AND lease_deadline IS NOT NULL)
        OR (state <> 'processing' AND worker IS NULL AND lease_deadline IS NULL)
    ),
    CONSTRAINT billing_inbox_processed_at_valid CHECK (
        (state = 'processed' AND processed_at IS NOT NULL)
        OR (state <> 'processed' AND processed_at IS NULL)
    )
);

CREATE INDEX billing_inbox_claim_idx
    ON billing_inbox (available_at, received_at, message_id)
    WHERE state = 'pending';
CREATE INDEX billing_inbox_expired_idx
    ON billing_inbox (lease_deadline, message_id)
    WHERE state = 'processing';

CREATE TABLE billing_outbox (
    account_id text NOT NULL,
    message_id text NOT NULL,
    provider text NOT NULL,
    merchant text NOT NULL,
    environment text NOT NULL,
    kind text NOT NULL,
    direction text NOT NULL DEFAULT 'outbound',
    occurred_at timestamptz NOT NULL,
    payload bytea NOT NULL,
    fingerprint text NOT NULL,
    state text NOT NULL DEFAULT 'pending',
    attempts integer NOT NULL DEFAULT 0,
    available_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    lease_deadline timestamptz,
    worker text,
    fence bigint NOT NULL DEFAULT 0,
    last_error text NOT NULL DEFAULT '',
    provider_reference text,
    received_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at timestamptz,
    payload_pruned_at timestamptz,
    PRIMARY KEY (account_id, message_id),
    CONSTRAINT billing_outbox_account_fk FOREIGN KEY (account_id)
        REFERENCES billing_accounts (account_id),
    CONSTRAINT billing_outbox_direction_valid CHECK (direction = 'outbound'),
    CONSTRAINT billing_outbox_state_valid CHECK (state IN ('pending', 'processing', 'unknown', 'completed', 'rejected')),
    CONSTRAINT billing_outbox_attempts_valid CHECK (attempts >= 0),
    CONSTRAINT billing_outbox_fence_valid CHECK (fence >= 0),
    CONSTRAINT billing_outbox_claim_valid CHECK (
        (state = 'processing' AND worker IS NOT NULL AND lease_deadline IS NOT NULL)
        OR (state <> 'processing' AND worker IS NULL AND lease_deadline IS NULL)
    ),
    CONSTRAINT billing_outbox_completed_at_valid CHECK (
        (state IN ('completed', 'rejected') AND completed_at IS NOT NULL)
        OR (state NOT IN ('completed', 'rejected') AND completed_at IS NULL)
    ),
    CONSTRAINT billing_outbox_completed_reference_valid CHECK (
        state <> 'completed' OR provider_reference IS NOT NULL
    )
);

CREATE INDEX billing_outbox_claim_idx
    ON billing_outbox (available_at, received_at, message_id)
    WHERE state = 'pending';



CREATE TABLE billing_settlement_batches (
    account_id text NOT NULL REFERENCES billing_accounts(account_id),
    batch_id text NOT NULL,
    original_batch_id text,
    period_start timestamptz NOT NULL,
    period_end timestamptz NOT NULL,
    cutoff timestamptz NOT NULL,
    currency text NOT NULL,
    total bigint NOT NULL,
    state text NOT NULL,
    revision bigint NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    fingerprint text NOT NULL,
    scope_provider text NOT NULL DEFAULT '',
    scope_merchant text NOT NULL DEFAULT '',
    scope_environment text NOT NULL DEFAULT '',
    scope_subscription_id text NOT NULL DEFAULT '',
    scope_item_id text NOT NULL DEFAULT '',
    PRIMARY KEY (account_id, batch_id),
    CONSTRAINT billing_settlement_original_fk FOREIGN KEY (account_id, original_batch_id)
        REFERENCES billing_settlement_batches(account_id, batch_id)
        DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT billing_settlement_batch_id_valid CHECK (batch_id <> ''),
    CONSTRAINT billing_settlement_original_id_valid CHECK (original_batch_id IS NULL OR original_batch_id <> ''),
    CONSTRAINT billing_settlement_period_valid CHECK (period_end > period_start AND cutoff >= period_start),
    CONSTRAINT billing_settlement_currency_valid CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT billing_settlement_total_valid CHECK (total >= 0 OR original_batch_id IS NOT NULL),
    CONSTRAINT billing_settlement_state_valid CHECK (state IN ('ready', 'submitting', 'confirmed', 'rejected', 'unknown')),
    CONSTRAINT billing_settlement_revision_valid CHECK (revision >= 0),
    CONSTRAINT billing_settlement_fingerprint_valid CHECK (fingerprint <> '')
    ,CONSTRAINT billing_settlement_batch_scope_shape CHECK ((scope_provider = '' AND scope_merchant = '' AND scope_environment = '' AND scope_subscription_id = '' AND scope_item_id = '') OR (scope_provider <> '' AND scope_merchant <> '' AND scope_environment <> '' AND scope_subscription_id <> '' AND scope_item_id <> ''))
);

-- A financial period can have one original finalization. Corrections are
-- explicitly linked to that original batch and may use the same interval.
CREATE UNIQUE INDEX billing_settlement_original_period_uq
    ON billing_settlement_batches (account_id, period_start, period_end, scope_provider, scope_merchant, scope_environment, scope_subscription_id, scope_item_id)
    WHERE original_batch_id IS NULL;

CREATE INDEX billing_settlement_batches_original_idx
    ON billing_settlement_batches (account_id, original_batch_id)
    WHERE original_batch_id IS NOT NULL;

CREATE TABLE billing_settlement_lines (
    account_id text NOT NULL,
    batch_id text NOT NULL,
    line_key text NOT NULL,
    kind text NOT NULL,
    usage_id text NOT NULL DEFAULT '',
    adjustment_id text NOT NULL DEFAULT '',
    original_usage_id text NOT NULL DEFAULT '',
    source text NOT NULL DEFAULT '',
    actor text NOT NULL DEFAULT '',
    project text NOT NULL DEFAULT '',
    occurred_at timestamptz,
    interval_start timestamptz,
    interval_end timestamptz,
    amount bigint NOT NULL,
    exact_amount text NOT NULL DEFAULT '',
    rule_version text NOT NULL DEFAULT '',
    currency text NOT NULL,
    reason text NOT NULL DEFAULT '',
    scope_provider text NOT NULL DEFAULT '',
    scope_merchant text NOT NULL DEFAULT '',
    scope_environment text NOT NULL DEFAULT '',
    scope_subscription_id text NOT NULL DEFAULT '',
    scope_item_id text NOT NULL DEFAULT '',
    PRIMARY KEY (account_id, batch_id, line_key),
    CONSTRAINT billing_settlement_line_batch_fk FOREIGN KEY (account_id, batch_id)
        REFERENCES billing_settlement_batches(account_id, batch_id) ON DELETE CASCADE,
    CONSTRAINT billing_settlement_line_kind_valid CHECK (kind IN ('usage', 'adjustment')),
    CONSTRAINT billing_settlement_line_key_valid CHECK (line_key <> ''),
    CONSTRAINT billing_settlement_line_currency_valid CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT billing_settlement_line_shape_valid CHECK (
        (kind = 'usage' AND usage_id <> '' AND adjustment_id = '' AND amount >= 0 AND source <> '' AND occurred_at IS NOT NULL AND exact_amount <> '' AND rule_version <> '')
        OR
        (kind = 'adjustment' AND usage_id = '' AND adjustment_id <> '' AND original_usage_id <> '' AND amount <> 0 AND reason <> '')
    ),
    CONSTRAINT billing_settlement_line_interval_valid CHECK (
        (interval_start IS NULL AND interval_end IS NULL) OR
        (interval_start IS NOT NULL AND interval_end > interval_start)
    ),
    CONSTRAINT billing_settlement_line_scope_shape CHECK ((scope_provider = '' AND scope_merchant = '' AND scope_environment = '' AND scope_subscription_id = '' AND scope_item_id = '') OR (scope_provider <> '' AND scope_merchant <> '' AND scope_environment <> '' AND scope_subscription_id <> '' AND scope_item_id <> ''))
);

CREATE TABLE billing_settlement_usage_claims (
    account_id text NOT NULL REFERENCES billing_accounts(account_id),
    usage_id text NOT NULL,
    batch_id text NOT NULL,
    source_fingerprint text NOT NULL,
    claimed_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (account_id, usage_id),
    CONSTRAINT billing_settlement_claim_usage_fk FOREIGN KEY (account_id, usage_id)
        REFERENCES billing_usage(account_id, usage_id),
    CONSTRAINT billing_settlement_claim_batch_fk FOREIGN KEY (account_id, batch_id)
        REFERENCES billing_settlement_batches(account_id, batch_id)
        DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT billing_settlement_claim_usage_valid CHECK (usage_id <> ''),
    CONSTRAINT billing_settlement_claim_batch_valid CHECK (batch_id <> ''),
    CONSTRAINT billing_settlement_claim_fingerprint_valid CHECK (source_fingerprint <> '')
);

CREATE INDEX billing_settlement_usage_claim_batch_idx
    ON billing_settlement_usage_claims (account_id, batch_id);

CREATE TABLE billing_settlement_attempts (
    account_id text NOT NULL REFERENCES billing_accounts(account_id),
    attempt_id text NOT NULL,
    batch_id text NOT NULL,
    provider text NOT NULL,
    idempotency_key text NOT NULL,
    provider_reference text NOT NULL DEFAULT '',
    state text NOT NULL,
    revision bigint NOT NULL,
    supports_idempotency boolean NOT NULL,
    supports_lookup boolean NOT NULL,
    reason text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (account_id, attempt_id),
    CONSTRAINT billing_settlement_attempt_batch_fk FOREIGN KEY (account_id, batch_id)
        REFERENCES billing_settlement_batches(account_id, batch_id),
    CONSTRAINT billing_settlement_attempt_id_valid CHECK (attempt_id <> ''),
    CONSTRAINT billing_settlement_attempt_provider_valid CHECK (provider <> ''),
    CONSTRAINT billing_settlement_attempt_key_valid CHECK (idempotency_key <> ''),
    CONSTRAINT billing_settlement_attempt_state_valid CHECK (state IN ('submitting', 'confirmed', 'rejected', 'unknown')),
    CONSTRAINT billing_settlement_attempt_revision_valid CHECK (revision >= 1)
);

CREATE UNIQUE INDEX billing_settlement_attempt_provider_key_uq
    ON billing_settlement_attempts (account_id, provider, idempotency_key);

CREATE TABLE billing_settlement_operations (
    account_id text NOT NULL REFERENCES billing_accounts(account_id),
    operation_id text NOT NULL,
    fingerprint text NOT NULL,
    error text NOT NULL,
    result jsonb NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (account_id, operation_id),
    CONSTRAINT billing_settlement_operation_id_valid CHECK (operation_id <> ''),
    CONSTRAINT billing_settlement_operation_fingerprint_valid CHECK (fingerprint <> '')
);



CREATE TABLE billing_allowance_schedules (
    account_id text NOT NULL,
    schedule_id text NOT NULL,
    subscription_id text NOT NULL,
    subscription_provider text NOT NULL,
    subscription_merchant text NOT NULL,
    subscription_environment text NOT NULL,
    assignment_id text NOT NULL,
    plan_version_id text NOT NULL,
    assignment jsonb NOT NULL,
    anchor timestamptz,
    state text NOT NULL,
    state_effective_at timestamptz NOT NULL,
    revision bigint NOT NULL,
    previous_schedule_id text,
    adjustment_mode text NOT NULL DEFAULT 'initial',
    source_id text NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    assignment_start timestamptz NOT NULL,
    PRIMARY KEY (account_id, schedule_id),
    UNIQUE (account_id, subscription_provider, subscription_merchant, subscription_environment, subscription_id, assignment_id),
    CONSTRAINT billing_allowance_schedules_account_fk FOREIGN KEY (account_id)
        REFERENCES billing_accounts (account_id),
    CONSTRAINT billing_allowance_schedules_plan_fk FOREIGN KEY (plan_version_id)
        REFERENCES billing_plans (id),
    CONSTRAINT billing_allowance_schedules_id_valid CHECK (schedule_id <> ''),
    CONSTRAINT billing_allowance_schedules_subscription_valid CHECK (subscription_id <> ''),
    CONSTRAINT billing_allowance_schedules_subscription_scope_valid CHECK (
        subscription_provider <> '' AND subscription_merchant <> '' AND subscription_environment <> ''
    ),
    CONSTRAINT billing_allowance_schedules_assignment_valid CHECK (assignment_id <> ''),
    CONSTRAINT billing_allowance_schedules_state_valid CHECK (
        state IN ('active', 'paused', 'canceled', 'delinquent')
    ),
    CONSTRAINT billing_allowance_schedules_revision_valid CHECK (revision > 0),
    CONSTRAINT billing_allowance_schedules_adjustment_valid CHECK (
        adjustment_mode IN ('initial', 'reject', 'delta', 'prorated')
    ),
    CONSTRAINT billing_allowance_schedules_source_valid CHECK (source_id <> '')
);

CREATE INDEX billing_allowance_schedules_due_idx
    ON billing_allowance_schedules (account_id, schedule_id);

CREATE TABLE billing_allowance_schedule_history (
    account_id text NOT NULL,
    schedule_id text NOT NULL,
    revision bigint NOT NULL,
    snapshot jsonb NOT NULL,
    source_id text NOT NULL,
    recorded_at timestamptz NOT NULL,
    state_effective_at timestamptz NOT NULL,
    PRIMARY KEY (account_id, schedule_id, revision),
    CONSTRAINT billing_allowance_schedule_history_schedule_fk
        FOREIGN KEY (account_id, schedule_id)
        REFERENCES billing_allowance_schedules (account_id, schedule_id),
    CONSTRAINT billing_allowance_schedule_history_revision_valid CHECK (revision > 0),
    CONSTRAINT billing_allowance_schedule_history_source_valid CHECK (source_id <> '')
);

CREATE TABLE billing_allowance_eligibility_events (
    account_id text NOT NULL,
    schedule_id text NOT NULL,
    source_id text NOT NULL,
    effective_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    status text NOT NULL,
    eligibility jsonb NOT NULL,
    fingerprint text NOT NULL,
    PRIMARY KEY (account_id, schedule_id, source_id),
    CONSTRAINT billing_allowance_eligibility_events_schedule_fk
        FOREIGN KEY (account_id, schedule_id)
        REFERENCES billing_allowance_schedules (account_id, schedule_id),
    CONSTRAINT billing_allowance_eligibility_events_status_valid CHECK (
        status IN ('paid', 'trial', 'grace', 'canceled', 'paused', 'delinquent')
    ),
    CONSTRAINT billing_allowance_eligibility_events_source_valid CHECK (source_id <> '')
);

CREATE INDEX billing_allowance_eligibility_events_period_idx
    ON billing_allowance_eligibility_events
       (account_id, schedule_id, effective_at DESC, observed_at DESC);

CREATE TABLE billing_allowance_eligibility_evidence (
    account_id text NOT NULL,
    schedule_id text NOT NULL,
    source_id text NOT NULL,
    kind text NOT NULL,
    reference text NOT NULL,
    policy text NOT NULL,
    covered_start timestamptz NOT NULL,
    covered_end timestamptz NOT NULL,
    PRIMARY KEY (account_id, schedule_id, source_id, kind, reference, covered_start, covered_end),
    CONSTRAINT billing_allowance_eligibility_evidence_event_fk
        FOREIGN KEY (account_id, schedule_id, source_id)
        REFERENCES billing_allowance_eligibility_events (account_id, schedule_id, source_id),
    CONSTRAINT billing_allowance_eligibility_evidence_period_valid CHECK (covered_end > covered_start),
    CONSTRAINT billing_allowance_eligibility_evidence_reference_valid CHECK (reference <> '')
);

CREATE TABLE billing_allowance_issuances (
    account_id text NOT NULL,
    schedule_id text NOT NULL,
    subscription_id text NOT NULL,
    assignment_id text NOT NULL,
    plan_version_id text NOT NULL,
    definition_id text NOT NULL,
    period_start timestamptz NOT NULL,
    period_end timestamptz NOT NULL,
    grant_key text NOT NULL,
    operation_id text NOT NULL,
    unit_code text NOT NULL,
    unit_scale bigint NOT NULL,
    entitled_amount bigint NOT NULL,
    amount bigint NOT NULL,
    status text NOT NULL,
    eligibility_source_id text NOT NULL,
    issued_at timestamptz NOT NULL,
    PRIMARY KEY (account_id, schedule_id, definition_id, period_start),
    UNIQUE (account_id, grant_key),
    CONSTRAINT billing_allowance_issuances_schedule_fk
        FOREIGN KEY (account_id, schedule_id)
        REFERENCES billing_allowance_schedules (account_id, schedule_id),
    CONSTRAINT billing_allowance_issuances_plan_fk FOREIGN KEY (plan_version_id)
        REFERENCES billing_plans (id),
    CONSTRAINT billing_allowance_issuances_period_valid CHECK (period_end > period_start),
    CONSTRAINT billing_allowance_issuances_amount_valid CHECK (entitled_amount > 0 AND amount >= 0 AND amount <= entitled_amount),
    CONSTRAINT billing_allowance_issuances_status_valid CHECK (status IN ('granted', 'capped')),
    CONSTRAINT billing_allowance_issuances_ids_valid CHECK (
        grant_key <> '' AND operation_id <> '' AND definition_id <> '' AND eligibility_source_id <> ''
    )
);

CREATE INDEX billing_allowance_issuances_account_period_idx
    ON billing_allowance_issuances (account_id, period_start, schedule_id);

CREATE TABLE billing_allowance_high_water (
    account_id text NOT NULL,
    subscription_id text NOT NULL,
    lineage_key text NOT NULL,
    definition_id text NOT NULL,
    period_start timestamptz NOT NULL,
    entitled_amount bigint NOT NULL,
    assignment_id text NOT NULL,
    plan_version_id text NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (account_id, lineage_key, definition_id, period_start),
    CONSTRAINT billing_allowance_high_water_plan_fk FOREIGN KEY (plan_version_id)
        REFERENCES billing_plans (id),
    CONSTRAINT billing_allowance_high_water_amount_valid CHECK (entitled_amount > 0)
);


CREATE TABLE billing_payment_intents (
    account_id text NOT NULL,
    intent_id text NOT NULL,
    provider text NOT NULL,
    provider_account text NOT NULL,
    environment text NOT NULL,
    checkout_id text NOT NULL DEFAULT '',
    amount bigint NOT NULL,
    currency text NOT NULL,
    unit_code text NOT NULL,
    unit_scale bigint NOT NULL,
    credits bigint NOT NULL,
    lot_id text NOT NULL,
    valid_from timestamptz NOT NULL,
    expires_at timestamptz,
    expiry_policy text NOT NULL,
    status text NOT NULL DEFAULT 'pending',
    created_at timestamptz NOT NULL,
    confirmed_at timestamptz,
    transaction_id text,
    PRIMARY KEY (account_id, intent_id),
    CONSTRAINT billing_payment_intents_account_fk FOREIGN KEY (account_id)
        REFERENCES billing_accounts(account_id),
    CONSTRAINT billing_payment_intents_unit_fk FOREIGN KEY (unit_code, unit_scale)
        REFERENCES billing_credit_units(unit_code, unit_scale),
    CONSTRAINT billing_payment_intents_id_valid CHECK (intent_id <> ''),
    CONSTRAINT billing_payment_intents_scope_valid CHECK (
        provider <> '' AND provider_account <> '' AND environment <> ''
    ),
    CONSTRAINT billing_payment_intents_amount_valid CHECK (amount > 0 AND credits > 0),
    CONSTRAINT billing_payment_intents_currency_valid CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT billing_payment_intents_lot_valid CHECK (lot_id <> ''),
    CONSTRAINT billing_payment_intents_expiry_valid CHECK (
        (expiry_policy = 'fixed' AND expires_at IS NOT NULL AND expires_at > valid_from)
        OR (expiry_policy = 'none' AND expires_at IS NULL)
    ),
    CONSTRAINT billing_payment_intents_status_valid CHECK (status IN ('pending', 'confirmed')),
    CONSTRAINT billing_payment_intents_confirmation_valid CHECK (
        (status = 'pending' AND confirmed_at IS NULL AND transaction_id IS NULL)
        OR (status = 'confirmed' AND confirmed_at IS NOT NULL AND transaction_id IS NOT NULL)
    )
);

CREATE UNIQUE INDEX billing_payment_intents_checkout_idx
    ON billing_payment_intents(account_id, provider, provider_account, environment, checkout_id)
    WHERE checkout_id <> '';

CREATE TABLE billing_payment_transactions (
    account_id text NOT NULL,
    provider text NOT NULL,
    provider_account text NOT NULL,
    environment text NOT NULL,
    transaction_id text NOT NULL,
    intent_id text NOT NULL,
    amount bigint NOT NULL,
    currency text NOT NULL,
    status text NOT NULL,
    first_event_id text NOT NULL,
    outcome jsonb NOT NULL DEFAULT '{}'::jsonb,
    occurred_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (account_id, provider, provider_account, environment, transaction_id),
    CONSTRAINT billing_payment_transactions_intent_fk FOREIGN KEY (account_id, intent_id)
        REFERENCES billing_payment_intents(account_id, intent_id),
    CONSTRAINT billing_payment_transactions_identity_valid CHECK (
        provider <> '' AND provider_account <> '' AND environment <> '' AND transaction_id <> '' AND intent_id <> ''
    ),
    CONSTRAINT billing_payment_transactions_amount_valid CHECK (amount > 0),
    CONSTRAINT billing_payment_transactions_currency_valid CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT billing_payment_transactions_status_valid CHECK (status IN ('paid', 'completed', 'unpaid', 'failed', 'checkout_completed', 'checkout_aborted')),
    CONSTRAINT billing_payment_transactions_event_valid CHECK (first_event_id <> '')
);

-- A verified paid transaction cannot fund two internal billing accounts.
CREATE UNIQUE INDEX billing_payment_transactions_owner_idx
 ON billing_payment_transactions(provider,provider_account,environment,transaction_id)
 WHERE status IN ('paid','completed');

CREATE TABLE billing_payment_events (
    account_id text NOT NULL,
    event_id text NOT NULL,
    provider text NOT NULL,
    provider_account text NOT NULL,
    environment text NOT NULL,
    transaction_id text NOT NULL,
    intent_id text NOT NULL,
    status text NOT NULL,
    amount bigint NOT NULL,
    currency text NOT NULL,
    occurred_at timestamptz NOT NULL,
    payload bytea NOT NULL,
    fingerprint text NOT NULL,
    outcome jsonb NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (account_id, provider, provider_account, environment, event_id),
    CONSTRAINT billing_payment_events_intent_fk FOREIGN KEY (account_id, intent_id)
        REFERENCES billing_payment_intents(account_id, intent_id),
    CONSTRAINT billing_payment_events_transaction_fk FOREIGN KEY (account_id, provider, provider_account, environment, transaction_id)
        REFERENCES billing_payment_transactions(account_id, provider, provider_account, environment, transaction_id),
    CONSTRAINT billing_payment_events_id_valid CHECK (event_id <> '' AND transaction_id <> '' AND intent_id <> ''),
    CONSTRAINT billing_payment_events_scope_valid CHECK (provider <> '' AND provider_account <> '' AND environment <> ''),
    CONSTRAINT billing_payment_events_amount_valid CHECK (amount > 0),
    CONSTRAINT billing_payment_events_currency_valid CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT billing_payment_events_status_valid CHECK (status IN ('paid', 'completed', 'unpaid', 'failed', 'checkout_completed', 'checkout_aborted')),
    CONSTRAINT billing_payment_events_fingerprint_valid CHECK (fingerprint <> '')
);

CREATE INDEX billing_payment_events_transaction_idx
    ON billing_payment_events(account_id, provider, provider_account, environment, transaction_id);

CREATE TABLE billing_payment_adjustments (
    account_id text NOT NULL,
    adjustment_id text NOT NULL,
    event_id text NOT NULL,
    provider text NOT NULL,
    provider_account text NOT NULL,
    environment text NOT NULL,
    transaction_id text NOT NULL,
    intent_id text NOT NULL,
    kind text NOT NULL,
    amount bigint NOT NULL,
    currency text NOT NULL,
    occurred_at timestamptz NOT NULL,
    reason text NOT NULL,
    policy text NOT NULL DEFAULT 'reject',
    payload bytea NOT NULL,
    fingerprint text NOT NULL,
    revoked_credits bigint NOT NULL DEFAULT 0,
    consumed_exposure bigint NOT NULL DEFAULT 0,
    applied boolean NOT NULL DEFAULT false,
    error text NOT NULL DEFAULT '',
    result jsonb NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (account_id, adjustment_id),
    CONSTRAINT billing_payment_adjustments_intent_fk FOREIGN KEY (account_id, intent_id)
        REFERENCES billing_payment_intents(account_id, intent_id),
    CONSTRAINT billing_payment_adjustments_id_valid CHECK (adjustment_id <> '' AND event_id <> '' AND transaction_id <> '' AND intent_id <> ''),
    CONSTRAINT billing_payment_adjustments_scope_valid CHECK (provider <> '' AND provider_account <> '' AND environment <> ''),
    CONSTRAINT billing_payment_adjustments_kind_valid CHECK (kind IN ('refund', 'chargeback')),
    CONSTRAINT billing_payment_adjustments_amount_valid CHECK (amount > 0),
    CONSTRAINT billing_payment_adjustments_currency_valid CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT billing_payment_adjustments_policy_valid CHECK (policy IN ('reject', 'proportional')),
    CONSTRAINT billing_payment_adjustments_credit_valid CHECK (revoked_credits >= 0 AND consumed_exposure >= 0),
    CONSTRAINT billing_payment_adjustments_fingerprint_valid CHECK (fingerprint <> '')
);

CREATE UNIQUE INDEX billing_payment_adjustments_transaction_idx
    ON billing_payment_adjustments(account_id, provider, provider_account, environment, transaction_id);
CREATE UNIQUE INDEX billing_payment_adjustments_event_idx
    ON billing_payment_adjustments(account_id, provider, provider_account, environment, event_id);


CREATE INDEX billing_usage_scope_period_idx
    ON billing_usage(account_id, scope_provider, scope_merchant, scope_environment, scope_subscription_id, scope_item_id, occurred_at, usage_id);

CREATE INDEX billing_settlement_batches_scope_idx
    ON billing_settlement_batches (account_id, scope_provider, scope_merchant, scope_environment, scope_subscription_id, scope_item_id, period_start, period_end);

CREATE TABLE billing_subscription_events (
    account_id text NOT NULL,
    provider text NOT NULL,
    merchant text NOT NULL,
    environment text NOT NULL,
    external_id text NOT NULL,
    event_id text NOT NULL,
    occurred_at timestamptz NOT NULL,
    fingerprint text NOT NULL,
    result jsonb NOT NULL,
    PRIMARY KEY (account_id, provider, merchant, environment, external_id, event_id),
    CONSTRAINT billing_subscription_events_subscription_fk
        FOREIGN KEY (account_id, provider, merchant, environment, external_id)
        REFERENCES billing_subscriptions(account_id, provider, merchant, environment, external_id),
    CONSTRAINT billing_subscription_events_identity_valid CHECK (
        provider <> '' AND merchant <> '' AND environment <> '' AND external_id <> '' AND event_id <> ''
    ),
    CONSTRAINT billing_subscription_events_fingerprint_valid CHECK (fingerprint <> '')
);

CREATE INDEX billing_subscription_events_event_idx
    ON billing_subscription_events(account_id, event_id);


CREATE INDEX billing_inbox_retention_idx ON billing_inbox(account_id,processed_at,message_id)
 WHERE state='processed' AND payload_pruned_at IS NULL;
CREATE INDEX billing_outbox_retention_idx ON billing_outbox(account_id,completed_at,message_id)
 WHERE state IN ('completed','rejected') AND payload_pruned_at IS NULL;


CREATE INDEX billing_reservations_active_idx
    ON billing_reservations(account_id, reservation_id)
    WHERE state = 'held';

CREATE INDEX billing_reservations_usage_idx
    ON billing_reservations(account_id, ((evidence->>'UsageID')))
    WHERE evidence->>'UsageID' <> '';

CREATE INDEX billing_reservations_limit_idx
    ON billing_reservations(account_id, actor, unit, state, created_at)
    INCLUDE (consumed)
    WHERE state = 'settled';


CREATE INDEX billing_inbox_due_idx
    ON billing_inbox (available_at, received_at, account_id, message_id)
    WHERE state = 'pending';

CREATE INDEX billing_outbox_due_idx
    ON billing_outbox (available_at, received_at, account_id, message_id)
    WHERE state = 'pending';

CREATE INDEX billing_outbox_expired_idx
    ON billing_outbox (lease_deadline, account_id, message_id)
    WHERE state = 'processing';


-- Credit balance projections are maintained only for the portion of the lot
-- history that an account's readiness row says has been covered.  The lot
-- table remains the per-lot projection of the authoritative journal; these
-- aggregate rows can be backfilled from it without creating financial effects.

CREATE TABLE billing_credit_balance_projection_state (
    account_id text PRIMARY KEY,
    last_lot_id text,
    ready boolean NOT NULL,
    CONSTRAINT billing_credit_balance_projection_state_account_fk
        FOREIGN KEY (account_id) REFERENCES billing_accounts (account_id),
    CONSTRAINT billing_credit_balance_projection_state_cursor_valid
        CHECK (ready OR last_lot_id IS NULL OR last_lot_id <> '')
);

CREATE TABLE billing_credit_account_balances (
    account_id text NOT NULL,
    unit_code text NOT NULL,
    available numeric NOT NULL DEFAULT 0,
    held numeric NOT NULL DEFAULT 0,
    consumed numeric NOT NULL DEFAULT 0,
    expired numeric NOT NULL DEFAULT 0,
    revoked numeric NOT NULL DEFAULT 0,
    PRIMARY KEY (account_id, unit_code),
    CONSTRAINT billing_credit_account_balances_account_fk
        FOREIGN KEY (account_id) REFERENCES billing_accounts (account_id),
    CONSTRAINT billing_credit_account_balances_unit_fk
        FOREIGN KEY (unit_code) REFERENCES billing_credit_units (unit_code)
);

CREATE TABLE billing_credit_scope_balances (
    account_id text NOT NULL,
    unit_code text NOT NULL,
    scope text NOT NULL,
    available numeric NOT NULL DEFAULT 0,
    held numeric NOT NULL DEFAULT 0,
    consumed numeric NOT NULL DEFAULT 0,
    expired numeric NOT NULL DEFAULT 0,
    revoked numeric NOT NULL DEFAULT 0,
    PRIMARY KEY (account_id, unit_code, scope),
    CONSTRAINT billing_credit_scope_balances_account_fk
        FOREIGN KEY (account_id) REFERENCES billing_accounts (account_id),
    CONSTRAINT billing_credit_scope_balances_unit_fk
        FOREIGN KEY (unit_code) REFERENCES billing_credit_units (unit_code)
);

CREATE INDEX billing_lots_credit_balance_live_idx
    ON billing_lots (account_id, lot_id)
    INCLUDE (unit_code, scope, available, held, consumed, expired, revoked, pending_revocation)
    WHERE available > 0 OR held > 0 OR pending_revocation > 0;

CREATE OR REPLACE FUNCTION billing_credit_balance_apply_delta(
    p_account_id text,
    p_unit_code text,
    p_scope text,
    p_available numeric,
    p_held numeric,
    p_consumed numeric,
    p_expired numeric,
    p_revoked numeric
) RETURNS void
LANGUAGE plpgsql
AS $function$
BEGIN
    INSERT INTO billing_credit_account_balances (
        account_id, unit_code, available, held, consumed, expired, revoked
    ) VALUES (
        p_account_id, p_unit_code, p_available, p_held, p_consumed, p_expired, p_revoked
    )
    ON CONFLICT (account_id, unit_code) DO UPDATE SET
        available = billing_credit_account_balances.available + EXCLUDED.available,
        held = billing_credit_account_balances.held + EXCLUDED.held,
        consumed = billing_credit_account_balances.consumed + EXCLUDED.consumed,
        expired = billing_credit_account_balances.expired + EXCLUDED.expired,
        revoked = billing_credit_account_balances.revoked + EXCLUDED.revoked;

    INSERT INTO billing_credit_scope_balances (
        account_id, unit_code, scope, available, held, consumed, expired, revoked
    ) VALUES (
        p_account_id, p_unit_code, p_scope, p_available, p_held, p_consumed, p_expired, p_revoked
    )
    ON CONFLICT (account_id, unit_code, scope) DO UPDATE SET
        available = billing_credit_scope_balances.available + EXCLUDED.available,
        held = billing_credit_scope_balances.held + EXCLUDED.held,
        consumed = billing_credit_scope_balances.consumed + EXCLUDED.consumed,
        expired = billing_credit_scope_balances.expired + EXCLUDED.expired,
        revoked = billing_credit_scope_balances.revoked + EXCLUDED.revoked;
END;
$function$;

CREATE OR REPLACE FUNCTION billing_credit_lot_balance_projection_trigger()
RETURNS trigger
LANGUAGE plpgsql
AS $function$
DECLARE
    old_covered boolean := false;
    new_covered boolean := false;
BEGIN
    IF TG_OP = 'UPDATE' AND (OLD.account_id <> NEW.account_id OR OLD.lot_id <> NEW.lot_id) THEN
        RAISE EXCEPTION 'billing lot identity is immutable';
    END IF;

    -- Every lot mutation shares the account lock used by credit operations and
    -- by the page builder.
    IF TG_OP IN ('UPDATE', 'DELETE') THEN
        PERFORM 1 FROM billing_accounts WHERE account_id = OLD.account_id FOR UPDATE;
    ELSE
        PERFORM 1 FROM billing_accounts WHERE account_id = NEW.account_id FOR UPDATE;
    END IF;

    IF TG_OP IN ('UPDATE', 'DELETE') THEN
        SELECT ready OR (last_lot_id IS NOT NULL AND OLD.lot_id <= last_lot_id)
          INTO old_covered
          FROM billing_credit_balance_projection_state
         WHERE account_id = OLD.account_id;
        old_covered := COALESCE(old_covered, false);
    END IF;

    IF TG_OP IN ('INSERT', 'UPDATE') THEN
        SELECT ready OR (last_lot_id IS NOT NULL AND NEW.lot_id <= last_lot_id)
          INTO new_covered
          FROM billing_credit_balance_projection_state
         WHERE account_id = NEW.account_id;
        new_covered := COALESCE(new_covered, false);
    END IF;

    IF old_covered THEN
        PERFORM billing_credit_balance_apply_delta(
            OLD.account_id, OLD.unit_code, OLD.scope,
            -OLD.available::numeric, -OLD.held::numeric, -OLD.consumed::numeric,
            -OLD.expired::numeric, -OLD.revoked::numeric
        );
    END IF;
    IF new_covered THEN
        PERFORM billing_credit_balance_apply_delta(
            NEW.account_id, NEW.unit_code, NEW.scope,
            NEW.available::numeric, NEW.held::numeric, NEW.consumed::numeric,
            NEW.expired::numeric, NEW.revoked::numeric
        );
    END IF;

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$function$;

CREATE TRIGGER billing_credit_lot_balance_projection_trg
AFTER INSERT OR UPDATE OR DELETE ON billing_lots
FOR EACH ROW EXECUTE FUNCTION billing_credit_lot_balance_projection_trigger();

CREATE OR REPLACE FUNCTION billing_credit_account_projection_ready_trigger()
RETURNS trigger
LANGUAGE plpgsql
AS $function$
BEGIN
    INSERT INTO billing_credit_balance_projection_state (account_id, last_lot_id, ready)
    VALUES (NEW.account_id, NULL, true)
    ON CONFLICT (account_id) DO NOTHING;
    RETURN NEW;
END;
$function$;

CREATE TRIGGER billing_credit_account_projection_ready_trg
AFTER INSERT ON billing_accounts
FOR EACH ROW EXECUTE FUNCTION billing_credit_account_projection_ready_trigger();


CREATE INDEX billing_reservations_due_idx
    ON billing_reservations(account_id,deadline,reservation_id)
    WHERE state='held';

CREATE INDEX billing_reservations_held_cap_idx
    ON billing_reservations(account_id,actor,unit,created_at)
    INCLUDE (authorized,deadline)
    WHERE state='held';


CREATE FUNCTION billing_allowance_history_instant(snapshot jsonb) RETURNS timestamptz
LANGUAGE plpgsql AS $function$
DECLARE instant timestamptz;
BEGIN
    instant := (snapshot->>'StateEffectiveAt')::timestamptz;
    IF instant IS NULL OR NOT isfinite(instant) THEN
        RAISE EXCEPTION 'allowance history requires a finite effective instant';
    END IF;
    RETURN instant;
END;
$function$;

CREATE FUNCTION billing_allowance_history_project_instant() RETURNS trigger
LANGUAGE plpgsql AS $function$
BEGIN
    NEW.state_effective_at := billing_allowance_history_instant(NEW.snapshot);
    RETURN NEW;
END;
$function$;

CREATE TRIGGER billing_allowance_history_instant_trg
BEFORE INSERT OR UPDATE OF snapshot ON billing_allowance_schedule_history
FOR EACH ROW EXECUTE FUNCTION billing_allowance_history_project_instant();

CREATE INDEX billing_allowance_history_asof_idx
    ON billing_allowance_schedule_history(account_id,schedule_id,state_effective_at,revision);

CREATE INDEX billing_allowance_eligibility_asof_idx
    ON billing_allowance_eligibility_events(account_id,schedule_id,effective_at DESC,observed_at DESC,source_id DESC);


CREATE TABLE billing_allowance_lineage (
 account_id text NOT NULL,
 schedule_id text NOT NULL,
 root_assignment_id text NOT NULL CHECK(root_assignment_id <> ''),
 root_anchor timestamptz NOT NULL,
 transition_anchor timestamptz NOT NULL,
 PRIMARY KEY(account_id,schedule_id),
 FOREIGN KEY(account_id,schedule_id) REFERENCES billing_allowance_schedules(account_id,schedule_id)
);

CREATE FUNCTION billing_allowance_lineage_identity_guard() RETURNS trigger
LANGUAGE plpgsql AS $function$
BEGIN
 IF OLD.account_id IS DISTINCT FROM NEW.account_id
 OR OLD.schedule_id IS DISTINCT FROM NEW.schedule_id
 OR OLD.assignment_id IS DISTINCT FROM NEW.assignment_id
 OR OLD.plan_version_id IS DISTINCT FROM NEW.plan_version_id
 OR OLD.subscription_id IS DISTINCT FROM NEW.subscription_id
 OR OLD.subscription_provider IS DISTINCT FROM NEW.subscription_provider
 OR OLD.subscription_merchant IS DISTINCT FROM NEW.subscription_merchant
 OR OLD.subscription_environment IS DISTINCT FROM NEW.subscription_environment
 OR OLD.previous_schedule_id IS DISTINCT FROM NEW.previous_schedule_id
 OR OLD.anchor IS DISTINCT FROM NEW.anchor
 OR (OLD.assignment->'Effective'->>'Start') IS DISTINCT FROM (NEW.assignment->'Effective'->>'Start')
 THEN RAISE EXCEPTION 'allowance lineage identity is immutable'; END IF;
 RETURN NEW;
END;
$function$;

CREATE TRIGGER billing_allowance_lineage_identity_trg
BEFORE UPDATE ON billing_allowance_schedules
FOR EACH ROW EXECUTE FUNCTION billing_allowance_lineage_identity_guard();


CREATE TABLE billing_allowance_successors (
 account_id text NOT NULL,
 schedule_id text NOT NULL,
 effective_at timestamptz NOT NULL,
 PRIMARY KEY(account_id,schedule_id),
 FOREIGN KEY(account_id,schedule_id) REFERENCES billing_allowance_schedules(account_id,schedule_id)
);

CREATE FUNCTION billing_allowance_assignment_start() RETURNS trigger
LANGUAGE plpgsql AS $function$
BEGIN
 NEW.assignment_start := (NEW.assignment->'Effective'->>'Start')::timestamptz;
 IF NEW.assignment_start IS NULL OR NOT isfinite(NEW.assignment_start)
 THEN RAISE EXCEPTION 'allowance assignment start is required'; END IF;
 RETURN NEW;
END;
$function$;

CREATE TRIGGER billing_allowance_assignment_start_trg
BEFORE INSERT OR UPDATE ON billing_allowance_schedules
FOR EACH ROW EXECUTE FUNCTION billing_allowance_assignment_start();

CREATE FUNCTION billing_allowance_successor_minimum() RETURNS trigger
LANGUAGE plpgsql AS $function$
BEGIN
 IF NEW.previous_schedule_id IS NOT NULL THEN
  INSERT INTO billing_allowance_successors(account_id,schedule_id,effective_at)
  VALUES(NEW.account_id,NEW.previous_schedule_id,NEW.assignment_start)
  ON CONFLICT(account_id,schedule_id) DO UPDATE
   SET effective_at=EXCLUDED.effective_at
   WHERE EXCLUDED.effective_at < billing_allowance_successors.effective_at;
 END IF;
 RETURN NEW;
END;
$function$;

-- Every update can prepare a legacy row, including lineage preparation.
CREATE TRIGGER billing_allowance_successor_minimum_trg
AFTER INSERT OR UPDATE ON billing_allowance_schedules
FOR EACH ROW EXECUTE FUNCTION billing_allowance_successor_minimum();

-- 002_allowance_checkpoints.sql
CREATE TABLE billing_allowance_change_journal (
    change_id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    account_id text NOT NULL,
    schedule_id text NOT NULL,
    changed_at timestamptz NOT NULL,
    effective_at timestamptz NOT NULL,
    kind text NOT NULL,
    FOREIGN KEY (account_id) REFERENCES billing_accounts(account_id) ON DELETE CASCADE
);
CREATE INDEX billing_allowance_change_journal_account_idx
    ON billing_allowance_change_journal(account_id, change_id);

CREATE TABLE billing_allowance_checkpoints (
    account_id text NOT NULL,
    checkpoint_id text NOT NULL,
    revision bigint NOT NULL DEFAULT 0 CHECK (revision >= 0),
    schedule_id text NOT NULL DEFAULT '',
    definition_id text NOT NULL DEFAULT '',
    period_start timestamptz,
    through timestamptz,
    dirty_from timestamptz,
    completed_through timestamptz,
    processed_change_id bigint NOT NULL DEFAULT 0 CHECK (processed_change_id >= 0),
    change_observed_id bigint NOT NULL DEFAULT 0 CHECK (change_observed_id >= 0),
    has_more boolean NOT NULL DEFAULT false,
    pass_active boolean NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (account_id, checkpoint_id),
    FOREIGN KEY (account_id) REFERENCES billing_accounts(account_id) ON DELETE CASCADE
);

-- 003_usage_ingestion.sql
-- Ingestion order is storage-owned, independent of caller-provided event and
-- receipt timestamps. Account transactions serialize accepted usage with close
-- watermark capture. Identity gaps after rollback are expected and harmless.
ALTER TABLE billing_usage
 ADD COLUMN ingestion_sequence bigint GENERATED ALWAYS AS IDENTITY;

CREATE UNIQUE INDEX billing_usage_ingestion_idx
 ON billing_usage(account_id,ingestion_sequence);

CREATE INDEX billing_usage_close_ingestion_idx
 ON billing_usage(account_id,scope_provider,scope_merchant,scope_environment,
                 scope_subscription_id,scope_item_id,ingestion_sequence)
 INCLUDE(usage_id,occurred_at,interval_start,interval_end)
 WHERE funding='postpaid';

-- 004_settlement_close.sql
-- Bounded, resumable settlement close state. A close batch is staged in the
-- existing batch/line tables while it is preparing; publication is a single
-- visibility CAS. Pending reservations stay separate from published claims.
ALTER TABLE billing_settlement_batches
    DROP CONSTRAINT billing_settlement_state_valid;

ALTER TABLE billing_settlement_batches
    ADD CONSTRAINT billing_settlement_state_valid CHECK
        (state IN ('preparing', 'canceling', 'ready', 'submitting', 'confirmed', 'rejected', 'unknown'));

CREATE TABLE billing_settlement_close_jobs (
    account_id text NOT NULL REFERENCES billing_accounts(account_id),
    job_id text NOT NULL,
    operation_id text NOT NULL,
    batch_id text NOT NULL,
    period_start timestamptz NOT NULL,
    period_end timestamptz NOT NULL,
    cutoff timestamptz NOT NULL,
    scope_provider text NOT NULL DEFAULT '',
    scope_merchant text NOT NULL DEFAULT '',
    scope_environment text NOT NULL DEFAULT '',
    scope_subscription_id text NOT NULL DEFAULT '',
    scope_item_id text NOT NULL DEFAULT '',
    currency text NOT NULL,
    watermark bigint NOT NULL,
    cursor bigint NOT NULL DEFAULT 0 CHECK (cursor >= 0),
    processed bigint NOT NULL DEFAULT 0,
    staged_total bigint NOT NULL DEFAULT 0,
    staged_checksum text NOT NULL,
    state text NOT NULL,
    revision bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (account_id, job_id),
    UNIQUE (account_id, operation_id),
    UNIQUE (account_id, batch_id),
    CONSTRAINT billing_settlement_close_job_state_valid CHECK (state IN ('preparing', 'ready', 'published', 'canceling', 'canceled')),
    CONSTRAINT billing_settlement_close_job_watermark_valid CHECK (watermark >= 0),
    CONSTRAINT billing_settlement_close_job_counts_valid CHECK (processed >= 0 AND staged_total >= 0),
    CONSTRAINT billing_settlement_close_job_revision_valid CHECK (revision >= 0),
    CONSTRAINT billing_settlement_close_job_period_valid CHECK (period_end > period_start AND cutoff >= period_start),
    CONSTRAINT billing_settlement_close_job_scope_shape CHECK ((scope_provider = '' AND scope_merchant = '' AND scope_environment = '' AND scope_subscription_id = '' AND scope_item_id = '') OR (scope_provider <> '' AND scope_merchant <> '' AND scope_environment <> '' AND scope_subscription_id <> '' AND scope_item_id <> ''))
);

CREATE UNIQUE INDEX billing_settlement_close_original_key_uq
    ON billing_settlement_close_jobs (account_id, period_start, period_end, scope_provider, scope_merchant, scope_environment, scope_subscription_id, scope_item_id)
    WHERE state <> 'canceled';

CREATE INDEX billing_settlement_close_page_idx
    ON billing_settlement_close_jobs (account_id, job_id, cursor);

CREATE TABLE billing_settlement_pending_claims (
    account_id text NOT NULL,
    usage_id text NOT NULL,
    job_id text NOT NULL,
    source_fingerprint text NOT NULL,
    reserved_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (account_id, usage_id),
    CONSTRAINT billing_settlement_pending_claim_usage_fk
        FOREIGN KEY (account_id, usage_id) REFERENCES billing_usage(account_id, usage_id),
    CONSTRAINT billing_settlement_pending_claim_job_fk
        FOREIGN KEY (account_id, job_id) REFERENCES billing_settlement_close_jobs(account_id, job_id) ON DELETE CASCADE,
    CONSTRAINT billing_settlement_pending_claim_usage_valid CHECK (usage_id <> ''),
    CONSTRAINT billing_settlement_pending_claim_job_valid CHECK (job_id <> ''),
    CONSTRAINT billing_settlement_pending_claim_fingerprint_valid CHECK (source_fingerprint <> '')
);

CREATE INDEX billing_settlement_pending_claim_job_idx
    ON billing_settlement_pending_claims (account_id, job_id, usage_id);

-- Summary reads must not count every line on each request.
ALTER TABLE billing_settlement_batches ADD COLUMN line_count bigint NOT NULL DEFAULT 0 CHECK(line_count>=0);
UPDATE billing_settlement_batches b SET line_count=(SELECT count(*) FROM billing_settlement_lines l WHERE l.account_id=b.account_id AND l.batch_id=b.batch_id);
CREATE INDEX billing_settlement_line_page_idx ON billing_settlement_lines(account_id,batch_id,line_key COLLATE "C");
CREATE INDEX billing_settlement_pending_cleanup_idx ON billing_settlement_pending_claims(account_id,job_id,usage_id COLLATE "C");
CREATE TABLE billing_settlement_close_nonlinear (
 account_id text NOT NULL,
 job_id text NOT NULL,
 aggregate_key text NOT NULL,
 PRIMARY KEY(account_id,job_id,aggregate_key),
 FOREIGN KEY(account_id,job_id) REFERENCES billing_settlement_close_jobs(account_id,job_id)
);

-- 005_refund_projection.sql
-- Immutable successful adjustments remain authoritative. This derived counter
-- makes the next refund's cumulative calculation independent of history size.
ALTER TABLE billing_payment_intents
 ADD COLUMN refunded_amount bigint NOT NULL DEFAULT 0,
 ADD CONSTRAINT billing_payment_refunded_amount_valid
 CHECK(refunded_amount >= 0 AND refunded_amount <= amount);

UPDATE billing_payment_intents i
 SET refunded_amount = a.amount
 FROM (
  SELECT account_id,intent_id,SUM(amount)::bigint AS amount
  FROM billing_payment_adjustments WHERE error=''
  GROUP BY account_id,intent_id
 ) a
 WHERE i.account_id=a.account_id AND i.intent_id=a.intent_id;

CREATE FUNCTION billing_payment_refund_projection_insert()
 RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.error='' THEN
  UPDATE billing_payment_intents
   SET refunded_amount=refunded_amount+NEW.amount
   WHERE account_id=NEW.account_id AND intent_id=NEW.intent_id;
  IF NOT FOUND THEN
   RAISE EXCEPTION 'refund projection has no owning intent';
  END IF;
 END IF;
 RETURN NEW;
END $$;

CREATE TRIGGER billing_payment_refund_projection_insert_trg
 AFTER INSERT ON billing_payment_adjustments
 FOR EACH ROW EXECUTE FUNCTION billing_payment_refund_projection_insert();

-- 006_settlement_summaries.sql
-- Reject malformed historical payloads before transforming any outcome. Null or
-- array Lines are the only accepted legacy forms; explicit counts are integral.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM billing_settlement_operations
        WHERE result ? 'Batch'
          AND (
              (result->'Batch' ? 'Lines' AND jsonb_typeof(result->'Batch'->'Lines') NOT IN ('array','null'))
              OR (result->'Batch' ? 'LineCount' AND (
                    jsonb_typeof(result->'Batch'->'LineCount') <> 'number'
                    OR (result->'Batch'->>'LineCount') !~ '^[0-9]+$'
              ))
          )
    ) THEN
        RAISE EXCEPTION 'invalid historical settlement outcome line metadata';
    END IF;
END $$;

-- Operation outcomes retain the bounded batch summary and provider attempt.
-- Historical settlement effects stay in batches/lines; this only removes the
-- duplicated line payload from replay records and never changes financial rows.
UPDATE billing_settlement_operations
SET result =
    (result #- '{Batch,Lines}')
    || jsonb_build_object(
        'Batch',
        ((result->'Batch') #- '{Lines}') || jsonb_build_object(
            'LineCount',
            COALESCE(
                result->'Batch'->'LineCount',
                CASE
                    WHEN jsonb_typeof(result->'Batch'->'Lines') = 'array'
                        THEN to_jsonb(jsonb_array_length(result->'Batch'->'Lines'))
                    WHEN result->'Batch'->'Lines' IS NULL OR result->'Batch'->'Lines' = 'null'::jsonb
                        THEN '0'::jsonb
                    ELSE NULL
                END
            )
        )
    )
WHERE result ? 'Batch'
  AND result->'Batch' ? 'Lines';

-- Correction membership and source checks are bounded by requested usage IDs.
CREATE INDEX IF NOT EXISTS billing_settlement_usage_line_lookup_idx
    ON billing_settlement_lines(account_id, batch_id, usage_id)
    WHERE kind = 'usage' AND usage_id <> '';

-- 007_credit_work_bounds.sql
-- Bounded credit maintenance and FEFO allocation queries.
CREATE INDEX billing_lots_due_work_idx
    ON billing_lots (account_id, expires_at, lot_id)
    WHERE available > 0 AND expires_at IS NOT NULL;

CREATE INDEX billing_lots_fefo_work_idx
    ON billing_lots (account_id, unit_code, expires_at, granted_at, lot_id)
    WHERE available > 0 AND revoked_at IS NULL;

-- 008_credit_repair.sql
-- Controlled credit projection repair generation and staged evidence.
ALTER TABLE billing_credit_balance_projection_state
    ADD COLUMN repair_job_id text;

CREATE TABLE billing_credit_repair_jobs (
    account_id text NOT NULL REFERENCES billing_accounts(account_id),
    repair_id text NOT NULL,
    fingerprint text NOT NULL,
    actor text NOT NULL,
    reason text NOT NULL,
    phase text NOT NULL,
    revision bigint NOT NULL DEFAULT 0,
    target_sequence bigint NOT NULL,
    journal_cursor bigint NOT NULL DEFAULT 0,
    lot_cursor text NOT NULL DEFAULT '',
    reservation_cursor text NOT NULL DEFAULT '',
    allocation_position bigint NOT NULL DEFAULT -1,
    allocation_total bigint NOT NULL DEFAULT 0,
    allocation_authorized bigint NOT NULL DEFAULT 0,
    replay_lots bigint NOT NULL DEFAULT 0,
    verified_lots bigint NOT NULL DEFAULT 0,
    changed_lots bigint NOT NULL DEFAULT 0,
    applied_lots bigint NOT NULL DEFAULT 0,
    unit_cursor text NOT NULL DEFAULT '',
    scope_cursor text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (account_id, repair_id),
    CONSTRAINT billing_credit_repair_job_id_valid CHECK (repair_id <> ''),
    CONSTRAINT billing_credit_repair_job_fingerprint_valid CHECK (fingerprint <> ''),
    CONSTRAINT billing_credit_repair_job_counters_valid CHECK (revision >= 0 AND target_sequence >= 0 AND journal_cursor >= 0 AND allocation_position >= -1 AND allocation_total >= 0 AND allocation_authorized >= 0 AND replay_lots >= 0 AND verified_lots >= 0 AND changed_lots >= 0 AND applied_lots >= 0),
    CONSTRAINT billing_credit_repair_job_phase_valid CHECK (phase IN ('replay','allocations','verify','ready','apply_lots','clear_accounts','clear_scopes','write_accounts','write_scopes','completed')) ,
    CONSTRAINT billing_credit_repair_job_actor_reason_valid CHECK (actor <> '' AND reason <> '')
);

ALTER TABLE billing_credit_balance_projection_state
    ADD CONSTRAINT billing_credit_projection_repair_fk
    FOREIGN KEY (account_id, repair_job_id)
    REFERENCES billing_credit_repair_jobs(account_id, repair_id);

CREATE TABLE billing_credit_repair_lots (
    account_id text NOT NULL,
    repair_id text NOT NULL,
    lot_id text NOT NULL,
    granted bigint NOT NULL,
    available bigint NOT NULL,
    held bigint NOT NULL,
    consumed bigint NOT NULL,
    expired bigint NOT NULL,
    revoked bigint NOT NULL,
    pending_revocation bigint NOT NULL DEFAULT 0,
    allocation_held bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (account_id, repair_id, lot_id),
    FOREIGN KEY (account_id, repair_id) REFERENCES billing_credit_repair_jobs(account_id, repair_id) ON DELETE CASCADE,
    CHECK (lot_id <> '' AND granted >= 0 AND available >= 0 AND held >= 0 AND consumed >= 0 AND expired >= 0 AND revoked >= 0 AND pending_revocation >= 0 AND allocation_held >= 0)
);

CREATE TABLE billing_credit_repair_totals (
    account_id text NOT NULL,
    repair_id text NOT NULL,
    kind text NOT NULL,
    unit_code text NOT NULL,
    scope text NOT NULL,
    available numeric NOT NULL DEFAULT 0,
    held numeric NOT NULL DEFAULT 0,
    consumed numeric NOT NULL DEFAULT 0,
    expired numeric NOT NULL DEFAULT 0,
    revoked numeric NOT NULL DEFAULT 0,
    PRIMARY KEY (account_id, repair_id, kind, unit_code, scope),
    FOREIGN KEY (account_id, repair_id) REFERENCES billing_credit_repair_jobs(account_id, repair_id) ON DELETE CASCADE,
    CHECK (kind IN ('account','scope') AND unit_code <> '' AND available >= 0 AND held >= 0 AND consumed >= 0 AND expired >= 0 AND revoked >= 0)
);

CREATE TABLE billing_credit_repair_evidence (
    account_id text NOT NULL,
    repair_id text NOT NULL,
    lot_id text NOT NULL,
    before_state jsonb NOT NULL,
    after_state jsonb NOT NULL,
    PRIMARY KEY (account_id, repair_id, lot_id),
    FOREIGN KEY (account_id, repair_id) REFERENCES billing_credit_repair_jobs(account_id, repair_id) ON DELETE CASCADE
);

CREATE OR REPLACE FUNCTION billing_credit_repair_write_fence() RETURNS trigger
LANGUAGE plpgsql AS $function$
DECLARE active_repair text;
BEGIN
    IF TG_OP = 'DELETE' THEN
        PERFORM 1 FROM billing_accounts WHERE account_id = OLD.account_id FOR UPDATE;
        SELECT repair_job_id INTO active_repair FROM billing_credit_balance_projection_state WHERE account_id = OLD.account_id;
    ELSE
        PERFORM 1 FROM billing_accounts WHERE account_id = NEW.account_id FOR UPDATE;
        SELECT repair_job_id INTO active_repair FROM billing_credit_balance_projection_state WHERE account_id = NEW.account_id;
    END IF;
    IF active_repair IS NOT NULL AND active_repair IS DISTINCT FROM current_setting('rho_billing.credit_repair', true) THEN
        RAISE EXCEPTION 'credit repair % fences writes for account', active_repair USING ERRCODE = 'RB001';
    END IF;
    RETURN COALESCE(NEW, OLD);
END;
$function$;

CREATE OR REPLACE FUNCTION billing_credit_repair_projection_fence() RETURNS trigger
LANGUAGE plpgsql AS $function$
DECLARE active_repair text;
DECLARE account_key text;
BEGIN
    account_key := CASE WHEN TG_OP = 'DELETE' THEN OLD.account_id ELSE NEW.account_id END;
    PERFORM 1 FROM billing_accounts WHERE account_id = account_key FOR UPDATE;
    SELECT repair_job_id INTO active_repair FROM billing_credit_balance_projection_state WHERE account_id = account_key;
    IF active_repair IS NOT NULL AND active_repair IS DISTINCT FROM current_setting('rho_billing.credit_repair', true) THEN
        RAISE EXCEPTION 'credit repair % keeps projection unavailable', active_repair USING ERRCODE = 'RB001';
    END IF;
    RETURN COALESCE(NEW, OLD);
END;
$function$;

CREATE TRIGGER billing_credit_repair_lots_fence BEFORE INSERT OR UPDATE OR DELETE ON billing_lots FOR EACH ROW EXECUTE FUNCTION billing_credit_repair_write_fence();
CREATE TRIGGER billing_credit_repair_reservations_fence BEFORE INSERT OR UPDATE OR DELETE ON billing_reservations FOR EACH ROW EXECUTE FUNCTION billing_credit_repair_write_fence();
CREATE TRIGGER billing_credit_repair_allocations_fence BEFORE INSERT OR UPDATE OR DELETE ON billing_reservation_allocations FOR EACH ROW EXECUTE FUNCTION billing_credit_repair_write_fence();
CREATE TRIGGER billing_credit_repair_journal_fence BEFORE INSERT OR UPDATE OR DELETE ON billing_journal FOR EACH ROW EXECUTE FUNCTION billing_credit_repair_write_fence();
CREATE TRIGGER billing_credit_repair_limits_fence BEFORE INSERT OR UPDATE OR DELETE ON billing_limits FOR EACH ROW EXECUTE FUNCTION billing_credit_repair_write_fence();
CREATE TRIGGER billing_credit_repair_operations_fence BEFORE INSERT OR UPDATE OR DELETE ON billing_operations FOR EACH ROW EXECUTE FUNCTION billing_credit_repair_write_fence();
CREATE TRIGGER billing_credit_repair_account_balances_fence BEFORE INSERT OR UPDATE OR DELETE ON billing_credit_account_balances FOR EACH ROW EXECUTE FUNCTION billing_credit_repair_write_fence();
CREATE TRIGGER billing_credit_repair_scope_balances_fence BEFORE INSERT OR UPDATE OR DELETE ON billing_credit_scope_balances FOR EACH ROW EXECUTE FUNCTION billing_credit_repair_write_fence();
CREATE TRIGGER billing_credit_repair_projection_state_fence BEFORE INSERT OR UPDATE OR DELETE ON billing_credit_balance_projection_state FOR EACH ROW EXECUTE FUNCTION billing_credit_repair_projection_fence();

-- 009_purchase_catalog.sql
-- Immutable commercial catalog and quote evidence for generic purchases.
-- Purchase intent, payment-fact, and fulfillment tables are added separately.

CREATE TABLE billing_purchase_offers (
    account_id text NOT NULL REFERENCES billing_accounts(account_id),
    offer_id text NOT NULL,
    offer_version bigint NOT NULL,
    name text NOT NULL,
    effects jsonb NOT NULL,
    published_at timestamptz NOT NULL,
    fingerprint text NOT NULL,
    PRIMARY KEY (account_id, offer_id, offer_version),
    CONSTRAINT billing_purchase_offer_id_valid CHECK (offer_id <> '' AND offer_version > 0),
    CONSTRAINT billing_purchase_offer_name_valid CHECK (name <> ''),
    CONSTRAINT billing_purchase_offer_fingerprint_valid CHECK (fingerprint <> ''),
    CONSTRAINT billing_purchase_offer_effects_size CHECK (octet_length(effects::text) <= 2097152)
);

CREATE TABLE billing_purchase_prices (
    account_id text NOT NULL,
    price_id text NOT NULL,
    price_version bigint NOT NULL,
    offer_id text NOT NULL,
    offer_version bigint NOT NULL,
    currency text NOT NULL,
    unit_amount bigint NOT NULL,
    tax_treatment text NOT NULL,
    published_at timestamptz NOT NULL,
    fingerprint text NOT NULL,
    PRIMARY KEY (account_id, price_id, price_version),
    FOREIGN KEY (account_id, offer_id, offer_version)
        REFERENCES billing_purchase_offers(account_id, offer_id, offer_version),
    CONSTRAINT billing_purchase_price_id_valid CHECK (price_id <> '' AND price_version > 0),
    CONSTRAINT billing_purchase_price_currency_valid CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT billing_purchase_price_amount_valid CHECK (unit_amount >= 0),
    CONSTRAINT billing_purchase_price_tax_valid CHECK (tax_treatment IN ('inclusive', 'exclusive')),
    CONSTRAINT billing_purchase_price_fingerprint_valid CHECK (fingerprint <> '')
);

CREATE TABLE billing_purchase_quotes (
    account_id text NOT NULL REFERENCES billing_accounts(account_id),
    quote_id text NOT NULL,
    currency text NOT NULL,
    tax_treatment text NOT NULL,
    amount bigint NOT NULL,
    created_at timestamptz NOT NULL,
    valid_until timestamptz NOT NULL,
    lines jsonb NOT NULL,
    fingerprint text NOT NULL,
    PRIMARY KEY (account_id, quote_id),
    CONSTRAINT billing_purchase_quote_id_valid CHECK (quote_id <> ''),
    CONSTRAINT billing_purchase_quote_currency_valid CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT billing_purchase_quote_tax_valid CHECK (tax_treatment IN ('inclusive', 'exclusive')),
    CONSTRAINT billing_purchase_quote_amount_valid CHECK (amount >= 0),
    CONSTRAINT billing_purchase_quote_validity CHECK (valid_until > created_at),
    CONSTRAINT billing_purchase_quote_fingerprint_valid CHECK (fingerprint <> ''),
    CONSTRAINT billing_purchase_quote_lines_size CHECK (octet_length(lines::text) <= 2097152)
);

CREATE INDEX billing_purchase_prices_offer_idx
    ON billing_purchase_prices(account_id, offer_id, offer_version);

-- 010_independent_assignments.sql
-- Immutable free, manual, and purchase-derived plan assignments.

CREATE TABLE billing_entitlement_assignments (
    account_id text NOT NULL REFERENCES billing_accounts(account_id),
    assignment_id text NOT NULL,
    plan_version_id text NOT NULL REFERENCES billing_plans(id),
    plan jsonb NOT NULL,
    source_ref text NOT NULL,
    actor text NOT NULL,
    reason text NOT NULL,
    created_at timestamptz NOT NULL,
    fingerprint text NOT NULL,
    PRIMARY KEY (account_id, assignment_id),
    CONSTRAINT billing_entitlement_assignment_id_valid CHECK (assignment_id <> ''),
    CONSTRAINT billing_entitlement_assignment_source_ref_valid CHECK (source_ref <> ''),
    CONSTRAINT billing_entitlement_assignment_actor_valid CHECK (actor <> ''),
    CONSTRAINT billing_entitlement_assignment_reason_valid CHECK (reason <> ''),
    CONSTRAINT billing_entitlement_assignment_fingerprint_valid CHECK (fingerprint <> ''),
    CONSTRAINT billing_entitlement_assignment_plan_size CHECK (octet_length(plan::text) <= 1048576)
);

CREATE INDEX billing_entitlement_assignments_plan_idx
    ON billing_entitlement_assignments(account_id, plan_version_id);

-- 011_purchase_lifecycle.sql
-- Generic purchase lifecycle evidence and mutable intent projections.

CREATE TABLE billing_purchase_intents (
    account_id text NOT NULL REFERENCES billing_accounts(account_id),
    intent_id text NOT NULL,
    operation text NOT NULL,
    quote_id text NOT NULL,
    quote_fingerprint text NOT NULL,
    provider text NOT NULL,
    merchant text NOT NULL,
    environment text NOT NULL,
    actor text NOT NULL,
    reason text NOT NULL,
    expires_at timestamptz NOT NULL,
    currency text NOT NULL,
    tax_treatment text NOT NULL,
    amount bigint NOT NULL,
    command text NOT NULL,
    payment text NOT NULL,
    fulfillment text NOT NULL,
    revision bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    transaction_id text NOT NULL DEFAULT '',
    paid_at timestamptz,
    last_payment_at timestamptz,
    last_payment_event_id text NOT NULL DEFAULT '',
    projection_fingerprint text NOT NULL,
    fingerprint text NOT NULL,
    PRIMARY KEY (account_id, intent_id),
    UNIQUE (account_id, operation),
    FOREIGN KEY (account_id, quote_id) REFERENCES billing_purchase_quotes(account_id, quote_id),
    CONSTRAINT billing_purchase_intent_id_valid CHECK (intent_id <> '' AND operation <> '' AND quote_id <> '' AND quote_fingerprint <> ''),
    CONSTRAINT billing_purchase_intent_scope_valid CHECK (provider <> '' AND merchant <> '' AND environment <> ''),
    CONSTRAINT billing_purchase_intent_actor_reason_valid CHECK (actor <> '' AND reason <> ''),
    CONSTRAINT billing_purchase_intent_currency_valid CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT billing_purchase_intent_tax_valid CHECK (tax_treatment IN ('inclusive', 'exclusive')),
    CONSTRAINT billing_purchase_intent_amount_valid CHECK (amount > 0),
    CONSTRAINT billing_purchase_intent_revision_valid CHECK (revision >= 1),
    CONSTRAINT billing_purchase_intent_validity CHECK (expires_at > created_at AND updated_at >= created_at),
    CONSTRAINT billing_purchase_intent_command_valid CHECK (command IN ('planned', 'dispatched', 'accepted', 'rejected', 'unknown', 'reconciled')),
    CONSTRAINT billing_purchase_intent_payment_valid CHECK (payment IN ('pending', 'action_required', 'failed', 'paid')),
    CONSTRAINT billing_purchase_intent_fulfillment_valid CHECK (fulfillment IN ('pending', 'complete')),
    CONSTRAINT billing_purchase_intent_fingerprint_valid CHECK (fingerprint <> '' AND projection_fingerprint <> '')
);

CREATE TABLE billing_purchase_commands (
    account_id text NOT NULL REFERENCES billing_accounts(account_id),
    operation text NOT NULL,
    intent_id text NOT NULL,
    input jsonb NOT NULL,
    result jsonb NOT NULL,
    fingerprint text NOT NULL,
    PRIMARY KEY (account_id, operation),
    FOREIGN KEY (account_id, intent_id) REFERENCES billing_purchase_intents(account_id, intent_id),
    CONSTRAINT billing_purchase_command_identity_valid CHECK (operation <> '' AND intent_id <> '' AND fingerprint <> ''),
    CONSTRAINT billing_purchase_command_size CHECK (octet_length(input::text) <= 2097152 AND octet_length(result::text) <= 2097152)
);

CREATE TABLE billing_purchase_payment_events (
    account_id text NOT NULL REFERENCES billing_accounts(account_id),
    provider text NOT NULL,
    merchant text NOT NULL,
    environment text NOT NULL,
    event_id text NOT NULL,
    intent_id text NOT NULL,
    fact jsonb NOT NULL,
    result jsonb NOT NULL,
    fingerprint text NOT NULL,
    PRIMARY KEY (account_id, provider, merchant, environment, event_id),
    UNIQUE (provider, merchant, environment, event_id),
    FOREIGN KEY (account_id, intent_id) REFERENCES billing_purchase_intents(account_id, intent_id),
    CONSTRAINT billing_purchase_payment_event_identity_valid CHECK (provider <> '' AND merchant <> '' AND environment <> '' AND event_id <> '' AND intent_id <> '' AND fingerprint <> ''),
    CONSTRAINT billing_purchase_payment_event_size CHECK (octet_length(fact::text) <= 2097152 AND octet_length(result::text) <= 2097152)
);

CREATE TABLE billing_purchase_funding (
    account_id text NOT NULL REFERENCES billing_accounts(account_id),
    provider text NOT NULL,
    merchant text NOT NULL,
    environment text NOT NULL,
    transaction_id text NOT NULL,
    intent_id text NOT NULL,
    currency text NOT NULL,
    gross bigint NOT NULL,
    tax bigint NOT NULL,
    paid_at timestamptz NOT NULL,
    lines jsonb NOT NULL,
    fingerprint text NOT NULL,
    PRIMARY KEY (account_id, provider, merchant, environment, transaction_id),
    UNIQUE (provider, merchant, environment, transaction_id),
    UNIQUE (account_id, intent_id),
    FOREIGN KEY (account_id, intent_id) REFERENCES billing_purchase_intents(account_id, intent_id),
    CONSTRAINT billing_purchase_funding_identity_valid CHECK (provider <> '' AND merchant <> '' AND environment <> '' AND transaction_id <> '' AND intent_id <> '' AND fingerprint <> ''),
    CONSTRAINT billing_purchase_funding_currency_valid CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT billing_purchase_funding_amount_valid CHECK (gross > 0 AND tax >= 0 AND tax <= gross),
    CONSTRAINT billing_purchase_funding_size CHECK (octet_length(lines::text) <= 2097152)
);

CREATE INDEX billing_purchase_payment_events_intent_idx
    ON billing_purchase_payment_events(account_id, intent_id);
CREATE INDEX billing_purchase_funding_intent_idx
    ON billing_purchase_funding(account_id, intent_id);

-- 012_purchase_fulfillment.sql
-- Durable purchase effects and settlement funding links.
CREATE TABLE billing_purchase_fulfillments (
 account_id text NOT NULL REFERENCES billing_accounts(account_id),
 fulfillment_id text NOT NULL,
 intent_id text NOT NULL,
 line_id text NOT NULL,
 effect jsonb NOT NULL,
 quantity bigint NOT NULL,
 settlement_batch_id text,
 effective_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL,
 state text NOT NULL,
 host_reference text NOT NULL DEFAULT '',
 applied_at timestamptz,
 acknowledged_at timestamptz,
 immutable_fingerprint text NOT NULL,
 record_digest text NOT NULL,
 PRIMARY KEY(account_id,fulfillment_id),
 FOREIGN KEY(account_id,intent_id) REFERENCES billing_purchase_intents(account_id,intent_id),
 FOREIGN KEY(account_id,settlement_batch_id) REFERENCES billing_settlement_batches(account_id,batch_id),
 CONSTRAINT purchase_fulfillment_state CHECK(state IN ('pending','complete')),
 CONSTRAINT purchase_fulfillment_ids CHECK(fulfillment_id<>'' AND intent_id<>'' AND line_id<>'' AND quantity>0),
 CONSTRAINT purchase_fulfillment_digest CHECK(immutable_fingerprint<>'' AND record_digest<>''),
 CONSTRAINT purchase_fulfillment_size CHECK(octet_length(effect::text)<=2097152)
);
CREATE INDEX billing_purchase_fulfillments_intent_idx ON billing_purchase_fulfillments(account_id,intent_id,fulfillment_id);
CREATE TABLE billing_purchase_settlement_funding (
 account_id text NOT NULL REFERENCES billing_accounts(account_id),
 batch_id text NOT NULL,
 intent_id text NOT NULL,
 effect_id text NOT NULL,
 scope_provider text NOT NULL,
 scope_merchant text NOT NULL,
 scope_environment text NOT NULL,
 transaction_id text NOT NULL,
 currency text NOT NULL,
 amount bigint NOT NULL,
 paid_at timestamptz NOT NULL,
 fingerprint text NOT NULL,
 PRIMARY KEY(account_id,batch_id),
 UNIQUE(account_id,effect_id),
 FOREIGN KEY(account_id,batch_id) REFERENCES billing_settlement_batches(account_id,batch_id),
 FOREIGN KEY(account_id,intent_id) REFERENCES billing_purchase_intents(account_id,intent_id),
 CONSTRAINT purchase_funding_identity CHECK(batch_id<>'' AND intent_id<>'' AND effect_id<>'' AND transaction_id<>'' AND fingerprint<>''),
 CONSTRAINT purchase_funding_scope CHECK(scope_provider<>'' AND scope_merchant<>'' AND scope_environment<>'' AND currency~'^[A-Z]{3}$' AND amount>=0)
);
CREATE INDEX billing_purchase_settlement_funding_intent_idx ON billing_purchase_settlement_funding(account_id,intent_id);

-- 013_assignment_revocations.sql
-- Immutable whole-assignment revocations.
CREATE TABLE billing_entitlement_assignment_revocations (
    account_id text NOT NULL,
    assignment_id text NOT NULL,
    source_ref text NOT NULL,
    actor text NOT NULL,
    reason text NOT NULL,
    effective_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL,
    fingerprint text NOT NULL,
    PRIMARY KEY (account_id, assignment_id),
    CONSTRAINT billing_entitlement_revocation_assignment_fk
        FOREIGN KEY (account_id, assignment_id)
        REFERENCES billing_entitlement_assignments(account_id, assignment_id),
    CONSTRAINT billing_entitlement_revocation_source_ref_valid CHECK (source_ref <> ''),
    CONSTRAINT billing_entitlement_revocation_actor_valid CHECK (actor <> ''),
    CONSTRAINT billing_entitlement_revocation_reason_valid CHECK (reason <> ''),
    CONSTRAINT billing_entitlement_revocation_fingerprint_valid CHECK (fingerprint <> '')
);

-- 014_purchase_adjustments.sql
-- Immutable purchase refund/chargeback evidence and bounded effect reversals.
CREATE TABLE billing_purchase_adjustments (
 account_id text NOT NULL REFERENCES billing_accounts(account_id),
 adjustment_id text NOT NULL,
 intent_id text NOT NULL,
 provider text NOT NULL,
 merchant text NOT NULL,
 environment text NOT NULL,
 provider_adjustment_id text NOT NULL,
 transaction_id text NOT NULL,
 kind text NOT NULL,
 currency text NOT NULL,
 input jsonb NOT NULL,
 result jsonb NOT NULL,
 created_at timestamptz NOT NULL,
 fingerprint text NOT NULL,
 record_digest text NOT NULL,
 PRIMARY KEY(account_id,adjustment_id),
 UNIQUE(account_id,adjustment_id,intent_id),
 UNIQUE(provider,merchant,environment,provider_adjustment_id),
 FOREIGN KEY(account_id,intent_id) REFERENCES billing_purchase_intents(account_id,intent_id),
 CONSTRAINT purchase_adjustment_identity CHECK(adjustment_id<>'' AND intent_id<>'' AND provider<>'' AND merchant<>'' AND environment<>'' AND provider_adjustment_id<>'' AND transaction_id<>''),
 CONSTRAINT purchase_adjustment_kind CHECK(kind IN ('refund','chargeback')),
 CONSTRAINT purchase_adjustment_currency CHECK(currency~'^[A-Z]{3}$'),
 CONSTRAINT purchase_adjustment_digest CHECK(fingerprint<>'' AND record_digest<>''),
 CONSTRAINT purchase_adjustment_size CHECK(octet_length(input::text)<=2097152 AND octet_length(result::text)<=2097152)
);
CREATE TABLE billing_purchase_adjustment_states (
 account_id text NOT NULL REFERENCES billing_accounts(account_id),
 intent_id text NOT NULL,
 revision bigint NOT NULL,
 lines jsonb NOT NULL,
 effects jsonb NOT NULL,
 fingerprint text NOT NULL,
 PRIMARY KEY(account_id,intent_id),
 FOREIGN KEY(account_id,intent_id) REFERENCES billing_purchase_intents(account_id,intent_id),
 CONSTRAINT purchase_adjustment_state_revision CHECK(revision>=1),
 CONSTRAINT purchase_adjustment_state_digest CHECK(fingerprint<>''),
 CONSTRAINT purchase_adjustment_state_size CHECK(octet_length(lines::text)<=2097152 AND octet_length(effects::text)<=2097152)
);
CREATE TABLE billing_purchase_reversals (
 account_id text NOT NULL REFERENCES billing_accounts(account_id),
 reversal_id text NOT NULL,
 adjustment_id text NOT NULL,
 intent_id text NOT NULL,
 original_effect_id text NOT NULL,
 original jsonb NOT NULL,
 effective_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL,
 state text NOT NULL,
 host_reference text NOT NULL DEFAULT '',
 applied_at timestamptz,
 acknowledged_at timestamptz,
 immutable_fingerprint text NOT NULL,
 record_digest text NOT NULL,
 PRIMARY KEY(account_id,reversal_id),
 UNIQUE(account_id,original_effect_id),
 FOREIGN KEY(account_id,adjustment_id,intent_id) REFERENCES billing_purchase_adjustments(account_id,adjustment_id,intent_id) DEFERRABLE INITIALLY DEFERRED,
 FOREIGN KEY(account_id,intent_id) REFERENCES billing_purchase_intents(account_id,intent_id),
 FOREIGN KEY(account_id,original_effect_id) REFERENCES billing_purchase_fulfillments(account_id,fulfillment_id),
 CONSTRAINT purchase_reversal_state CHECK(state IN ('pending','complete')),
 CONSTRAINT purchase_reversal_identity CHECK(reversal_id<>'' AND adjustment_id<>'' AND intent_id<>'' AND original_effect_id<>''),
 CONSTRAINT purchase_reversal_digest CHECK(immutable_fingerprint<>'' AND record_digest<>''),
 CONSTRAINT purchase_reversal_size CHECK(octet_length(original::text)<=2097152)
);
CREATE INDEX billing_purchase_adjustments_intent_idx ON billing_purchase_adjustments(account_id,intent_id);
CREATE INDEX billing_purchase_reversals_intent_idx ON billing_purchase_reversals(account_id,intent_id,reversal_id);
ALTER TABLE billing_purchase_fulfillments DROP CONSTRAINT IF EXISTS purchase_fulfillment_state;
ALTER TABLE billing_purchase_fulfillments ADD CONSTRAINT purchase_fulfillment_state CHECK(state IN ('pending','complete','canceled'));

-- 015_retire_credit_payments.sql
-- Freeze the legacy credit-payment write surface while retaining its historical
-- rows for export and audit. New purchase writes are fenced against identities
-- already owned by those rows; no historical row is converted into a quote or
-- purchase effect.
-- Lock both identity domains before checking conflicts and installing triggers,
-- closing the check/install race. Migration briefly blocks application writes.
LOCK TABLE billing_payment_intents, billing_payment_transactions, billing_payment_events, billing_payment_adjustments,
           billing_purchase_intents, billing_purchase_funding, billing_purchase_payment_events, billing_purchase_adjustments
    IN SHARE ROW EXCLUSIVE MODE;
DO $$
BEGIN
 IF EXISTS (SELECT 1 FROM billing_purchase_intents n JOIN billing_payment_intents l USING (account_id,intent_id)) THEN
  RAISE EXCEPTION 'legacy payment intent identity conflicts with purchase intent' USING ERRCODE='23505';
 END IF;
 IF EXISTS (SELECT 1 FROM billing_purchase_funding n JOIN billing_payment_transactions l ON l.provider=n.provider AND l.provider_account=n.merchant AND l.environment=n.environment AND l.transaction_id=n.transaction_id WHERE l.status IN ('paid','completed')) THEN
  RAISE EXCEPTION 'legacy paid transaction identity conflicts with purchase funding' USING ERRCODE='23505';
 END IF;
 IF EXISTS (SELECT 1 FROM billing_purchase_payment_events n JOIN billing_payment_events l ON l.provider=n.provider AND l.provider_account=n.merchant AND l.environment=n.environment AND l.event_id=n.event_id) THEN
  RAISE EXCEPTION 'legacy payment event identity conflicts with purchase event' USING ERRCODE='23505';
 END IF;
 IF EXISTS (SELECT 1 FROM billing_purchase_adjustments n JOIN billing_payment_adjustments l ON l.account_id=n.account_id AND l.adjustment_id=n.adjustment_id) THEN
  RAISE EXCEPTION 'legacy adjustment identity conflicts with purchase adjustment' USING ERRCODE='23505';
 END IF;
 IF EXISTS (SELECT 1 FROM billing_purchase_adjustments n JOIN billing_payment_adjustments l ON l.provider=n.provider AND l.provider_account=n.merchant AND l.environment=n.environment AND l.transaction_id=n.provider_adjustment_id) THEN
  RAISE EXCEPTION 'legacy adjustment provider identity conflicts with purchase adjustment' USING ERRCODE='23505';
 END IF;
END $$;

CREATE INDEX billing_payment_events_global_identity_idx
 ON billing_payment_events(provider,provider_account,environment,event_id);
CREATE INDEX billing_payment_adjustments_global_identity_idx
 ON billing_payment_adjustments(provider,provider_account,environment,transaction_id);

CREATE FUNCTION billing_legacy_payment_write_frozen() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'legacy credit-payment tables are historical read-only data' USING ERRCODE='55000';
END $$;
CREATE TRIGGER billing_payment_intents_frozen_trg BEFORE INSERT OR UPDATE OR DELETE ON billing_payment_intents FOR EACH ROW EXECUTE FUNCTION billing_legacy_payment_write_frozen();
CREATE TRIGGER billing_payment_transactions_frozen_trg BEFORE INSERT OR UPDATE OR DELETE ON billing_payment_transactions FOR EACH ROW EXECUTE FUNCTION billing_legacy_payment_write_frozen();
CREATE TRIGGER billing_payment_events_frozen_trg BEFORE INSERT OR UPDATE OR DELETE ON billing_payment_events FOR EACH ROW EXECUTE FUNCTION billing_legacy_payment_write_frozen();
CREATE TRIGGER billing_payment_adjustments_frozen_trg BEFORE INSERT OR UPDATE OR DELETE ON billing_payment_adjustments FOR EACH ROW EXECUTE FUNCTION billing_legacy_payment_write_frozen();
CREATE TRIGGER billing_payment_intents_truncate_frozen_trg BEFORE TRUNCATE ON billing_payment_intents FOR EACH STATEMENT EXECUTE FUNCTION billing_legacy_payment_write_frozen();
CREATE TRIGGER billing_payment_transactions_truncate_frozen_trg BEFORE TRUNCATE ON billing_payment_transactions FOR EACH STATEMENT EXECUTE FUNCTION billing_legacy_payment_write_frozen();
CREATE TRIGGER billing_payment_events_truncate_frozen_trg BEFORE TRUNCATE ON billing_payment_events FOR EACH STATEMENT EXECUTE FUNCTION billing_legacy_payment_write_frozen();
CREATE TRIGGER billing_payment_adjustments_truncate_frozen_trg BEFORE TRUNCATE ON billing_payment_adjustments FOR EACH STATEMENT EXECUTE FUNCTION billing_legacy_payment_write_frozen();

CREATE FUNCTION billing_purchase_identity_fence() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_TABLE_NAME = 'billing_purchase_intents' THEN
  IF EXISTS (SELECT 1 FROM billing_payment_intents WHERE account_id=NEW.account_id AND intent_id=NEW.intent_id) THEN
   RAISE EXCEPTION 'purchase intent is owned by legacy payment history' USING ERRCODE='23505';
  END IF;
 ELSIF TG_TABLE_NAME = 'billing_purchase_funding' THEN
  IF EXISTS (SELECT 1 FROM billing_payment_transactions WHERE provider=NEW.provider AND provider_account=NEW.merchant AND environment=NEW.environment AND transaction_id=NEW.transaction_id AND status IN ('paid','completed')) THEN
   RAISE EXCEPTION 'purchase funding transaction is owned by legacy payment history' USING ERRCODE='23505';
  END IF;
 ELSIF TG_TABLE_NAME = 'billing_purchase_payment_events' THEN
  IF EXISTS (SELECT 1 FROM billing_payment_events WHERE provider=NEW.provider AND provider_account=NEW.merchant AND environment=NEW.environment AND event_id=NEW.event_id) THEN
   RAISE EXCEPTION 'purchase payment event is owned by legacy payment history' USING ERRCODE='23505';
  END IF;
 ELSIF TG_TABLE_NAME = 'billing_purchase_adjustments' THEN
  IF EXISTS (SELECT 1 FROM billing_payment_adjustments WHERE account_id=NEW.account_id AND adjustment_id=NEW.adjustment_id) OR EXISTS (SELECT 1 FROM billing_payment_adjustments WHERE provider=NEW.provider AND provider_account=NEW.merchant AND environment=NEW.environment AND transaction_id=NEW.provider_adjustment_id) THEN
   RAISE EXCEPTION 'purchase adjustment identity is owned by legacy payment history' USING ERRCODE='23505';
  END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER billing_purchase_intents_legacy_fence_trg BEFORE INSERT ON billing_purchase_intents FOR EACH ROW EXECUTE FUNCTION billing_purchase_identity_fence();
CREATE TRIGGER billing_purchase_funding_legacy_fence_trg BEFORE INSERT ON billing_purchase_funding FOR EACH ROW EXECUTE FUNCTION billing_purchase_identity_fence();
CREATE TRIGGER billing_purchase_events_legacy_fence_trg BEFORE INSERT ON billing_purchase_payment_events FOR EACH ROW EXECUTE FUNCTION billing_purchase_identity_fence();
CREATE TRIGGER billing_purchase_adjustments_legacy_fence_trg BEFORE INSERT ON billing_purchase_adjustments FOR EACH ROW EXECUTE FUNCTION billing_purchase_identity_fence();

-- 016_purchase_disputes.sql
-- Provider dispute observations and bounded financial recovery.
CREATE TABLE billing_purchase_disputes (
 account_id text NOT NULL,
 provider text NOT NULL,
 merchant text NOT NULL,
 environment text NOT NULL,
 dispute_id text NOT NULL,
 intent_id text NOT NULL,
 transaction_id text NOT NULL,
 currency text NOT NULL,
 amount bigint NOT NULL,
 status text NOT NULL,
 status_occurred_at timestamptz NOT NULL,
 status_event_id text NOT NULL,
 debit_adjustment_id text,
 recovered jsonb NOT NULL,
 revision bigint NOT NULL,
 created_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL,
 terms_fingerprint text NOT NULL,
 record_digest text NOT NULL,
 PRIMARY KEY(account_id,provider,merchant,environment,dispute_id),
 UNIQUE(provider,merchant,environment,dispute_id),
 UNIQUE(account_id,provider,merchant,environment,dispute_id,intent_id,transaction_id),
 UNIQUE(account_id,provider,merchant,environment,dispute_id,intent_id,transaction_id,debit_adjustment_id),
 UNIQUE(provider,merchant,environment,debit_adjustment_id),
 FOREIGN KEY(account_id,intent_id) REFERENCES billing_purchase_intents(account_id,intent_id),
 FOREIGN KEY(account_id,debit_adjustment_id,intent_id) REFERENCES billing_purchase_adjustments(account_id,adjustment_id,intent_id) DEFERRABLE INITIALLY DEFERRED,
 CONSTRAINT purchase_dispute_amount CHECK(amount>0),
 CONSTRAINT purchase_dispute_revision CHECK(revision>0),
 CONSTRAINT purchase_dispute_currency CHECK(currency~'^[A-Z]{3}$'),
 CONSTRAINT purchase_dispute_size CHECK(octet_length(recovered::text)<=1048576),
 CONSTRAINT purchase_dispute_digest CHECK(terms_fingerprint<>'' AND record_digest<>'')
);
CREATE INDEX billing_purchase_disputes_intent_idx ON billing_purchase_disputes(account_id,intent_id,provider,merchant,environment);

CREATE TABLE billing_purchase_dispute_events (
 account_id text NOT NULL,
 provider text NOT NULL,
 merchant text NOT NULL,
 environment text NOT NULL,
 event_id text NOT NULL,
 dispute_id text NOT NULL,
 intent_id text NOT NULL,
 transaction_id text NOT NULL,
 fact jsonb NOT NULL,
 result jsonb NOT NULL,
 created_at timestamptz NOT NULL,
 record_digest text NOT NULL,
 PRIMARY KEY(account_id,provider,merchant,environment,event_id),
 UNIQUE(provider,merchant,environment,event_id),
 FOREIGN KEY(account_id,provider,merchant,environment,dispute_id,intent_id,transaction_id) REFERENCES billing_purchase_disputes(account_id,provider,merchant,environment,dispute_id,intent_id,transaction_id) DEFERRABLE INITIALLY DEFERRED,
 CONSTRAINT purchase_dispute_event_size CHECK(octet_length(fact::text)<=1048576 AND octet_length(result::text)<=1048576),
 CONSTRAINT purchase_dispute_event_digest CHECK(record_digest<>'')
);

CREATE TABLE billing_purchase_dispute_recoveries (
 account_id text NOT NULL,
 provider text NOT NULL,
 merchant text NOT NULL,
 environment text NOT NULL,
 recovery_id text NOT NULL,
 adjustment_id text NOT NULL,
 dispute_id text NOT NULL,
 intent_id text NOT NULL,
 transaction_id text NOT NULL,
 recovery jsonb NOT NULL,
 created_at timestamptz NOT NULL,
 recovery_fingerprint text NOT NULL,
 record_digest text NOT NULL,
 PRIMARY KEY(account_id,provider,merchant,environment,recovery_id),
 UNIQUE(provider,merchant,environment,recovery_id),
 FOREIGN KEY(account_id,provider,merchant,environment,dispute_id,intent_id,transaction_id,adjustment_id) REFERENCES billing_purchase_disputes(account_id,provider,merchant,environment,dispute_id,intent_id,transaction_id,debit_adjustment_id) DEFERRABLE INITIALLY DEFERRED,
 FOREIGN KEY(account_id,adjustment_id,intent_id) REFERENCES billing_purchase_adjustments(account_id,adjustment_id,intent_id) DEFERRABLE INITIALLY DEFERRED,
 CONSTRAINT purchase_dispute_recovery_size CHECK(octet_length(recovery::text)<=1048576),
 CONSTRAINT purchase_dispute_recovery_digest CHECK(recovery_fingerprint<>'' AND record_digest<>'')
);

-- 017_purchase_collection_bindings.sql
-- Immutable provider transaction bindings recorded before payment evidence.
CREATE TABLE billing_purchase_collection_bindings (
    account_id text NOT NULL REFERENCES billing_accounts(account_id),
    provider text NOT NULL,
    merchant text NOT NULL,
    environment text NOT NULL,
    transaction_id text NOT NULL,
    intent_id text NOT NULL,
    quote_fingerprint text NOT NULL,
    binding jsonb NOT NULL,
    fingerprint text NOT NULL,
    PRIMARY KEY (account_id, provider, merchant, environment, transaction_id),
    UNIQUE (provider, merchant, environment, transaction_id),
    FOREIGN KEY (account_id, intent_id) REFERENCES billing_purchase_intents(account_id, intent_id),
    CONSTRAINT billing_purchase_collection_binding_identity_valid CHECK (
        provider <> '' AND merchant <> '' AND environment <> '' AND transaction_id <> '' AND intent_id <> ''
        AND quote_fingerprint <> '' AND fingerprint <> ''
    ),
    CONSTRAINT billing_purchase_collection_binding_size CHECK (octet_length(binding::text) <= 1048576)
);

CREATE INDEX billing_purchase_collection_bindings_intent_idx
    ON billing_purchase_collection_bindings(account_id, intent_id);

CREATE FUNCTION billing_purchase_collection_legacy_fence() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF EXISTS (
    SELECT 1 FROM billing_payment_transactions
    WHERE provider=NEW.provider AND provider_account=NEW.merchant AND environment=NEW.environment
      AND transaction_id=NEW.transaction_id AND status IN ('paid','completed')
 ) THEN
  RAISE EXCEPTION 'purchase collection transaction is owned by legacy payment history' USING ERRCODE='23505';
 END IF;
 RETURN NEW;
END $$;

CREATE TRIGGER billing_purchase_collection_legacy_fence_trg
    BEFORE INSERT ON billing_purchase_collection_bindings
    FOR EACH ROW EXECUTE FUNCTION billing_purchase_collection_legacy_fence();

-- 018_outbox_dispatch.sql
-- Claim-fenced outbound dispatch and immutable provider result observations.
ALTER TABLE billing_outbox
    ADD COLUMN begun_at timestamptz,
    ADD COLUMN last_observation_id text;

CREATE TABLE billing_outbox_results (
    account_id text NOT NULL,
    message_id text NOT NULL,
    observation_id text NOT NULL,
    message_fingerprint text NOT NULL,
    claim_fence bigint NOT NULL,
    mode text NOT NULL,
    state text NOT NULL,
    provider_reference text,
    evidence text NOT NULL,
    expected_previous text NOT NULL DEFAULT '',
    result_fingerprint text NOT NULL,
    observed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (account_id, message_id, observation_id),
    FOREIGN KEY (account_id, message_id) REFERENCES billing_outbox(account_id, message_id),
    CONSTRAINT billing_outbox_result_identity_valid CHECK (
        observation_id <> '' AND message_fingerprint <> '' AND claim_fence > 0 AND result_fingerprint <> ''
    ),
    CONSTRAINT billing_outbox_result_mode_valid CHECK (mode IN ('finish','resolve')),
    CONSTRAINT billing_outbox_result_state_valid CHECK (state IN ('completed','rejected','unknown')),
    CONSTRAINT billing_outbox_result_reference_valid CHECK (
        state <> 'completed' OR (provider_reference IS NOT NULL AND provider_reference <> '')
    ),
    CONSTRAINT billing_outbox_result_evidence_valid CHECK (evidence <> '' AND octet_length(evidence) <= 4096)
);

CREATE INDEX billing_outbox_results_observed_idx
    ON billing_outbox_results(account_id, message_id, observed_at, observation_id);

ALTER TABLE billing_outbox
    ADD CONSTRAINT billing_outbox_last_observation_fk
    FOREIGN KEY (account_id, message_id, last_observation_id)
    REFERENCES billing_outbox_results(account_id, message_id, observation_id)
    DEFERRABLE INITIALLY DEFERRED;

-- 019_chargeback_recovery_totals.sql
-- Bounded per-intent chargeback-recovery totals derived from immutable recovery history.
-- Retained source rows already carry application-verified fingerprints and record
-- digests. This migration leaves them untouched and validates projection fields.
CREATE TABLE billing_purchase_chargeback_recovery_totals (
 account_id text NOT NULL,
 intent_id text NOT NULL,
 line_id text NOT NULL,
 gross bigint NOT NULL,
 tax bigint NOT NULL,
 PRIMARY KEY(account_id,intent_id,line_id),
 FOREIGN KEY(account_id,intent_id) REFERENCES billing_purchase_intents(account_id,intent_id),
 CONSTRAINT purchase_chargeback_recovery_amounts CHECK(gross>0 AND tax>=0 AND tax<=gross)
);

CREATE FUNCTION billing_validate_chargeback_recovery_lines(value jsonb) RETURNS void AS $$
DECLARE line jsonb; line_id text; gross numeric; tax numeric;
BEGIN
 IF jsonb_typeof(value) IS DISTINCT FROM 'array' OR jsonb_array_length(value)=0 OR jsonb_array_length(value)>100 THEN
  RAISE EXCEPTION 'invalid chargeback recovery lines' USING ERRCODE='23514';
 END IF;
 FOR line IN SELECT item FROM jsonb_array_elements(value) AS item LOOP
  IF jsonb_typeof(line) IS DISTINCT FROM 'object'
     OR (SELECT COUNT(*) FROM jsonb_object_keys(line))<>3
     OR NOT (line ? 'LineID' AND line ? 'Gross' AND line ? 'Tax')
     OR jsonb_typeof(line->'LineID') IS DISTINCT FROM 'string'
     OR jsonb_typeof(line->'Gross') IS DISTINCT FROM 'number'
     OR jsonb_typeof(line->'Tax') IS DISTINCT FROM 'number' THEN
   RAISE EXCEPTION 'malformed chargeback recovery line' USING ERRCODE='23514';
  END IF;
  line_id := line->>'LineID'; gross := (line->>'Gross')::numeric; tax := (line->>'Tax')::numeric;
  IF length(line_id)<1 OR length(line_id)>256 OR line_id~'[[:cntrl:]]'
     OR gross<>trunc(gross) OR tax<>trunc(tax) OR gross<=0 OR gross>9223372036854775807
     OR tax<0 OR tax>gross OR tax>9223372036854775807 THEN
   RAISE EXCEPTION 'invalid chargeback recovery line value' USING ERRCODE='23514';
  END IF;
 END LOOP;
 IF (SELECT COUNT(*) FROM jsonb_array_elements(value)) <>
    (SELECT COUNT(DISTINCT item->>'LineID') FROM jsonb_array_elements(value) AS item) THEN
  RAISE EXCEPTION 'duplicate chargeback recovery line' USING ERRCODE='23514';
 END IF;
END;
$$ LANGUAGE plpgsql IMMUTABLE;

DO $$
DECLARE source record;
BEGIN
 FOR source IN SELECT recovery FROM billing_purchase_dispute_recoveries LOOP
  PERFORM billing_validate_chargeback_recovery_lines(source.recovery->'Lines');
 END LOOP;
 IF EXISTS (
  SELECT 1 FROM (
   SELECT SUM((line->>'Gross')::numeric) AS gross,SUM((line->>'Tax')::numeric) AS tax
   FROM billing_purchase_dispute_recoveries AS recovery
   CROSS JOIN LATERAL jsonb_array_elements(recovery.recovery->'Lines') AS line
   GROUP BY recovery.account_id,recovery.intent_id,line->>'LineID'
  ) AS total
  WHERE total.gross>9223372036854775807 OR total.tax>9223372036854775807
 ) THEN
  RAISE EXCEPTION 'chargeback recovery projection overflow' USING ERRCODE='22003';
 END IF;
END;
$$;

INSERT INTO billing_purchase_chargeback_recovery_totals(account_id,intent_id,line_id,gross,tax)
SELECT recovery.account_id,recovery.intent_id,line->>'LineID',
       SUM((line->>'Gross')::numeric)::bigint,SUM((line->>'Tax')::numeric)::bigint
FROM billing_purchase_dispute_recoveries AS recovery
CROSS JOIN LATERAL jsonb_array_elements(recovery.recovery->'Lines') AS line
GROUP BY recovery.account_id,recovery.intent_id,line->>'LineID';

CREATE FUNCTION billing_assert_chargeback_recovery_bounds(bound_account text,bound_intent text) RETURNS void AS $$
BEGIN
 IF (SELECT COUNT(*) FROM billing_purchase_chargeback_recovery_totals
     WHERE account_id=bound_account AND intent_id=bound_intent)>100 THEN
  RAISE EXCEPTION 'chargeback recovery projection exceeds 100 lines' USING ERRCODE='23514';
 END IF;
 IF EXISTS (
  WITH chargeback AS (
   SELECT line->>'LineID' AS line_id,(line->>'ChargebackGross')::numeric AS gross,
          (line->>'ChargebackTax')::numeric AS tax
   FROM billing_purchase_adjustment_states AS state
   CROSS JOIN LATERAL jsonb_array_elements(state.lines) AS line
   WHERE state.account_id=bound_account AND state.intent_id=bound_intent
  )
  SELECT 1 FROM billing_purchase_chargeback_recovery_totals AS recovered
  LEFT JOIN chargeback USING(line_id)
  WHERE recovered.account_id=bound_account AND recovered.intent_id=bound_intent
    AND (chargeback.line_id IS NULL OR chargeback.gross IS NULL OR chargeback.tax IS NULL
      OR chargeback.gross<0 OR chargeback.tax<0 OR chargeback.tax>chargeback.gross
      OR recovered.gross>chargeback.gross OR recovered.tax>chargeback.tax
      OR recovered.gross-recovered.tax>chargeback.gross-chargeback.tax)
 ) THEN
  RAISE EXCEPTION 'chargeback recovery exceeds applied chargeback' USING ERRCODE='23514';
 END IF;
END;
$$ LANGUAGE plpgsql;

DO $$
DECLARE projected record;
BEGIN
 FOR projected IN SELECT DISTINCT account_id,intent_id FROM billing_purchase_chargeback_recovery_totals LOOP
  PERFORM billing_assert_chargeback_recovery_bounds(projected.account_id,projected.intent_id);
 END LOOP;
END;
$$;

CREATE FUNCTION billing_project_chargeback_recovery() RETURNS trigger AS $$
BEGIN
 PERFORM billing_validate_chargeback_recovery_lines(NEW.recovery->'Lines');
 INSERT INTO billing_purchase_chargeback_recovery_totals(account_id,intent_id,line_id,gross,tax)
 SELECT NEW.account_id,NEW.intent_id,line->>'LineID',SUM((line->>'Gross')::numeric)::bigint,SUM((line->>'Tax')::numeric)::bigint
 FROM jsonb_array_elements(NEW.recovery->'Lines') AS line GROUP BY line->>'LineID'
 ON CONFLICT(account_id,intent_id,line_id) DO UPDATE SET
  gross=billing_purchase_chargeback_recovery_totals.gross+EXCLUDED.gross,
  tax=billing_purchase_chargeback_recovery_totals.tax+EXCLUDED.tax;
 PERFORM billing_assert_chargeback_recovery_bounds(NEW.account_id,NEW.intent_id);
 RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER billing_project_chargeback_recovery_after_insert
AFTER INSERT ON billing_purchase_dispute_recoveries
FOR EACH ROW EXECUTE FUNCTION billing_project_chargeback_recovery();

-- 020_subscription_lifecycle.sql
-- Host-owned subscription lifecycle: desired quantity, access, and collection
-- remain independent of provider observation snapshots.

CREATE TABLE billing_subscription_lifecycles (
    account_id text NOT NULL REFERENCES billing_accounts(account_id),
    lifecycle_id text NOT NULL,
    revision bigint NOT NULL,
    record jsonb NOT NULL,
    fingerprint text NOT NULL,
    PRIMARY KEY (account_id, lifecycle_id),
    CONSTRAINT billing_subscription_lifecycle_id_valid CHECK (lifecycle_id <> ''),
    CONSTRAINT billing_subscription_lifecycle_revision_valid CHECK (revision > 0),
    CONSTRAINT billing_subscription_lifecycle_fingerprint_valid CHECK (fingerprint <> ''),
    CONSTRAINT billing_subscription_lifecycle_record_size CHECK (octet_length(record::text) <= 1048576)
);

CREATE TABLE billing_subscription_changes (
    account_id text NOT NULL REFERENCES billing_accounts(account_id),
    operation text NOT NULL,
    lifecycle_id text NOT NULL,
    fingerprint text NOT NULL,
    state text NOT NULL,
    record jsonb NOT NULL,
    PRIMARY KEY (account_id, operation),
    CONSTRAINT billing_subscription_change_operation_valid CHECK (operation <> ''),
    CONSTRAINT billing_subscription_change_lifecycle_valid CHECK (lifecycle_id <> ''),
    CONSTRAINT billing_subscription_change_fingerprint_valid CHECK (fingerprint <> ''),
    CONSTRAINT billing_subscription_change_state_valid CHECK (state IN ('planned','accepted','confirmed','superseded','rejected')),
    CONSTRAINT billing_subscription_change_record_size CHECK (octet_length(record::text) <= 1048576)
);

CREATE INDEX billing_subscription_changes_lifecycle_idx
    ON billing_subscription_changes(account_id, lifecycle_id);

-- 021_internal_costs.sql
-- Operator internal cost evidence. Customer rating amounts are never stored
-- here; a usage_id is an optional identity link only.

CREATE TABLE billing_internal_costs (
    account_id text NOT NULL REFERENCES billing_accounts(account_id),
    cost_id text NOT NULL,
    usage_id text NOT NULL DEFAULT '',
    resource text NOT NULL,
    model text NOT NULL,
    quantity bigint NOT NULL,
    rule_version text NOT NULL,
    currency text NOT NULL,
    amount bigint NOT NULL,
    exact_amount text NOT NULL,
    state text NOT NULL,
    corrects_id text NOT NULL DEFAULT '',
    missing_reason text NOT NULL DEFAULT '',
    source_currency text NOT NULL DEFAULT '',
    occurred_at timestamptz NOT NULL,
    recorded_at timestamptz NOT NULL,
    fingerprint text NOT NULL,
    record jsonb NOT NULL,
    PRIMARY KEY (account_id, cost_id),
    CONSTRAINT billing_internal_costs_id_valid CHECK (cost_id <> ''),
    CONSTRAINT billing_internal_costs_resource_valid CHECK (resource <> ''),
    CONSTRAINT billing_internal_costs_model_valid CHECK (model <> ''),
    CONSTRAINT billing_internal_costs_rule_valid CHECK (rule_version <> ''),
    CONSTRAINT billing_internal_costs_quantity_valid CHECK (quantity >= 0),
    CONSTRAINT billing_internal_costs_amount_valid CHECK (amount >= 0),
    CONSTRAINT billing_internal_costs_currency_valid CHECK (char_length(currency) = 3 AND currency = upper(currency)),
    CONSTRAINT billing_internal_costs_state_valid CHECK (state IN ('estimated', 'actual', 'missing')),
    CONSTRAINT billing_internal_costs_missing CHECK (
        (state = 'missing' AND amount = 0 AND exact_amount = '0' AND missing_reason <> '')
        OR (state <> 'missing' AND missing_reason = '')
    ),
    CONSTRAINT billing_internal_costs_source_currency CHECK (source_currency = '' OR source_currency = currency),
    CONSTRAINT billing_internal_costs_corrects_self CHECK (corrects_id <> cost_id),
    CONSTRAINT billing_internal_costs_fingerprint_valid CHECK (fingerprint <> ''),
    CONSTRAINT billing_internal_costs_record_size CHECK (octet_length(record::text) <= 1048576)
);

CREATE UNIQUE INDEX billing_internal_costs_one_correction_idx
    ON billing_internal_costs(account_id, corrects_id)
    WHERE corrects_id <> '';

CREATE INDEX billing_internal_costs_usage_idx
    ON billing_internal_costs(account_id, usage_id)
    WHERE usage_id <> '';

-- 022_limits_and_topups.sql
-- Monetary budgets and consented automatic top-up attempts.

CREATE TABLE billing_budgets (
    account_id text NOT NULL REFERENCES billing_accounts(account_id),
    budget_id text NOT NULL,
    actor text NOT NULL DEFAULT '',
    project text NOT NULL DEFAULT '',
    basis text NOT NULL,
    currency text NOT NULL,
    period_start timestamptz NOT NULL,
    period_end timestamptz NOT NULL,
    amount bigint NOT NULL,
    record jsonb NOT NULL,
    PRIMARY KEY (account_id, budget_id),
    CONSTRAINT billing_budgets_id_valid CHECK (budget_id <> ''),
    CONSTRAINT billing_budgets_basis_valid CHECK (basis IN ('billable', 'internal', 'topup')),
    CONSTRAINT billing_budgets_currency_valid CHECK (char_length(currency) = 3 AND currency = upper(currency)),
    CONSTRAINT billing_budgets_amount_valid CHECK (amount >= 0),
    CONSTRAINT billing_budgets_period_valid CHECK (period_end > period_start),
    CONSTRAINT billing_budgets_record_size CHECK (octet_length(record::text) <= 1048576)
);

CREATE TABLE billing_budget_holds (
    account_id text NOT NULL REFERENCES billing_accounts(account_id),
    hold_id text NOT NULL,
    group_id text NOT NULL,
    budget_id text NOT NULL,
    state text NOT NULL,
    amount bigint NOT NULL,
    settled bigint NOT NULL DEFAULT 0,
    record jsonb NOT NULL,
    PRIMARY KEY (account_id, hold_id),
    CONSTRAINT billing_budget_holds_group_valid CHECK (group_id <> ''),
    CONSTRAINT billing_budget_holds_budget_fk FOREIGN KEY (account_id, budget_id) REFERENCES billing_budgets (account_id, budget_id),
    CONSTRAINT billing_budget_holds_state_valid CHECK (state IN ('held', 'settled', 'released')),
    CONSTRAINT billing_budget_holds_amount_valid CHECK (amount > 0 AND settled >= 0 AND settled <= amount),
    CONSTRAINT billing_budget_holds_record_size CHECK (octet_length(record::text) <= 1048576)
);

CREATE INDEX billing_budget_holds_group_idx ON billing_budget_holds(account_id, group_id);
CREATE INDEX billing_budget_holds_budget_idx ON billing_budget_holds(account_id, budget_id);

CREATE TABLE billing_topup_policies (
    account_id text NOT NULL REFERENCES billing_accounts(account_id),
    policy_id text NOT NULL,
    record jsonb NOT NULL,
    PRIMARY KEY (account_id, policy_id),
    CONSTRAINT billing_topup_policies_id_valid CHECK (policy_id <> ''),
    CONSTRAINT billing_topup_policies_record_size CHECK (octet_length(record::text) <= 1048576)
);

CREATE TABLE billing_topup_attempts (
    account_id text NOT NULL REFERENCES billing_accounts(account_id),
    attempt_id text NOT NULL,
    policy_id text NOT NULL,
    state text NOT NULL,
    created_at timestamptz NOT NULL,
    record jsonb NOT NULL,
    PRIMARY KEY (account_id, attempt_id),
    CONSTRAINT billing_topup_attempts_policy_valid CHECK (policy_id <> ''),
    CONSTRAINT billing_topup_attempts_state_valid CHECK (state IN ('planned', 'dispatched', 'unknown', 'action_required', 'rejected', 'granted')),
    CONSTRAINT billing_topup_attempts_record_size CHECK (octet_length(record::text) <= 1048576)
);

CREATE UNIQUE INDEX billing_topup_attempts_active_idx
    ON billing_topup_attempts(account_id, policy_id)
    WHERE state IN ('planned', 'dispatched', 'unknown', 'action_required');

-- 023_ops.sql
-- Operator repairs, restartable backfills and identity tombstones.

CREATE TABLE billing_ops_repairs (
    account_id text NOT NULL REFERENCES billing_accounts(account_id),
    repair_id text NOT NULL,
    revision bigint NOT NULL,
    actor text NOT NULL,
    reason text NOT NULL,
    evidence text NOT NULL,
    fingerprint text NOT NULL,
    record jsonb NOT NULL,
    PRIMARY KEY (account_id, repair_id),
    CONSTRAINT billing_ops_repairs_id_valid CHECK (repair_id <> ''),
    CONSTRAINT billing_ops_repairs_revision_valid CHECK (revision > 0),
    CONSTRAINT billing_ops_repairs_actor_valid CHECK (actor <> ''),
    CONSTRAINT billing_ops_repairs_record_size CHECK (octet_length(record::text) <= 1048576)
);

CREATE TABLE billing_ops_backfills (
    account_id text NOT NULL REFERENCES billing_accounts(account_id),
    job_id text NOT NULL,
    cursor bigint NOT NULL,
    processed bigint NOT NULL,
    state text NOT NULL,
    record jsonb NOT NULL,
    PRIMARY KEY (account_id, job_id),
    CONSTRAINT billing_ops_backfills_id_valid CHECK (job_id <> ''),
    CONSTRAINT billing_ops_backfills_cursor_valid CHECK (cursor >= 0 AND processed >= 0),
    CONSTRAINT billing_ops_backfills_state_valid CHECK (state IN ('running', 'done')),
    CONSTRAINT billing_ops_backfills_record_size CHECK (octet_length(record::text) <= 1048576)
);

CREATE TABLE billing_ops_tombstones (
    account_id text NOT NULL REFERENCES billing_accounts(account_id),
    kind text NOT NULL,
    identity text NOT NULL,
    fingerprint text NOT NULL,
    record jsonb NOT NULL,
    PRIMARY KEY (account_id, kind, identity),
    CONSTRAINT billing_ops_tombstones_kind_valid CHECK (kind <> ''),
    CONSTRAINT billing_ops_tombstones_id_valid CHECK (identity <> ''),
    CONSTRAINT billing_ops_tombstones_record_size CHECK (octet_length(record::text) <= 1048576)
);

