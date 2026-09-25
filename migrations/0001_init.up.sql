-- 0001_init.up.sql
-- Core schema for the distributed wager processing service.
-- All financial invariants that can be expressed in SQL are enforced here:
-- non-negative balances, append-only ledger, unique idempotency keys and
-- duplicate-reversal protection.

CREATE TABLE wallets (
    id            UUID PRIMARY KEY,
    player_id     UUID        NOT NULL,
    currency      CHAR(3)     NOT NULL,
    balance_minor BIGINT      NOT NULL,
    version       BIGINT      NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL,
    CONSTRAINT wallets_player_currency_unique UNIQUE (player_id, currency),
    CONSTRAINT wallets_currency_iso CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT wallets_balance_non_negative CHECK (balance_minor >= 0),
    CONSTRAINT wallets_version_positive CHECK (version >= 1),
    CONSTRAINT wallets_updated_after_created CHECK (updated_at >= created_at)
);

CREATE TABLE wager_transactions (
    id                              UUID PRIMARY KEY,
    origin                          TEXT        NOT NULL,
    provider_id                     TEXT,
    external_transaction_id         TEXT,
    idempotency_key                 TEXT,
    payload_hash                    TEXT,
    wallet_id                       UUID        NOT NULL REFERENCES wallets (id),
    player_id                       UUID        NOT NULL,
    round_id                        TEXT,
    game_id                         TEXT,
    kind                            TEXT        NOT NULL,
    amount_minor                    BIGINT      NOT NULL,
    currency                        CHAR(3)     NOT NULL,
    reference_external_transaction_id TEXT,
    reference_transaction_id        UUID REFERENCES wager_transactions (id),
    status                          TEXT        NOT NULL,
    failure_code                    TEXT,
    result_balance_minor            BIGINT,
    attempts                        INTEGER     NOT NULL DEFAULT 0,
    next_attempt_at                 TIMESTAMPTZ,
    created_at                      TIMESTAMPTZ NOT NULL,
    updated_at                      TIMESTAMPTZ NOT NULL,
    CONSTRAINT wager_tx_origin CHECK (origin IN ('INTERNAL', 'EXTERNAL')),
    CONSTRAINT wager_tx_kind CHECK (kind IN ('OPENING', 'BET', 'WIN', 'LOSS', 'REFUND', 'ROLLBACK')),
    CONSTRAINT wager_tx_status CHECK (status IN ('PENDING', 'PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED')),
    CONSTRAINT wager_tx_currency_iso CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT wager_tx_attempts_non_negative CHECK (attempts >= 0),
    -- LOSS never moves money; every other kind requires a positive amount.
    CONSTRAINT wager_tx_amount_by_kind CHECK ((kind = 'LOSS') = (amount_minor = 0)),
    -- Internal operations are only wallet openings, which are always processed on creation.
    CONSTRAINT wager_tx_internal_is_opening CHECK (origin <> 'INTERNAL' OR (kind = 'OPENING' AND status = 'PROCESSED')),
    -- Internal rows do not carry external metadata; external rows must carry it.
    CONSTRAINT wager_tx_origin_fields CHECK (
        (
            origin = 'INTERNAL'
            AND provider_id IS NULL
            AND external_transaction_id IS NULL
            AND idempotency_key IS NULL
            AND payload_hash IS NULL
            AND round_id IS NULL
            AND game_id IS NULL
            AND reference_external_transaction_id IS NULL
        )
        OR (
            origin = 'EXTERNAL'
            AND kind <> 'OPENING'
            AND provider_id IS NOT NULL
            AND external_transaction_id IS NOT NULL
            AND idempotency_key IS NOT NULL
            AND payload_hash IS NOT NULL
            AND round_id IS NOT NULL
            AND game_id IS NOT NULL
        )
    ),
    -- REFUND and ROLLBACK always reference an external transaction.
    CONSTRAINT wager_tx_reversal_reference CHECK (
        kind NOT IN ('REFUND', 'ROLLBACK') OR reference_external_transaction_id IS NOT NULL
    ),
    CONSTRAINT wager_tx_result_balance_non_negative CHECK (result_balance_minor IS NULL OR result_balance_minor >= 0),
    -- Terminal failures and rejections always carry a stable failure code.
    CONSTRAINT wager_tx_failure_code_required CHECK (
        (status IN ('REJECTED', 'FAILED')) = (failure_code IS NOT NULL)
    )
);

-- One external transaction per provider, independent of the idempotency key used.
CREATE UNIQUE INDEX wager_tx_provider_external_unique
    ON wager_transactions (provider_id, external_transaction_id)
    WHERE origin = 'EXTERNAL';

-- An idempotency key may be used exactly once per provider.
CREATE UNIQUE INDEX wager_tx_provider_idempotency_unique
    ON wager_transactions (provider_id, idempotency_key)
    WHERE origin = 'EXTERNAL';

-- A reference may not receive two successful reversals of the same type.
CREATE UNIQUE INDEX wager_tx_reference_reversal_unique
    ON wager_transactions (reference_transaction_id, kind)
    WHERE status = 'PROCESSED' AND kind IN ('REFUND', 'ROLLBACK');

-- A wallet can have at most one opening credit.
CREATE UNIQUE INDEX wager_tx_opening_unique
    ON wager_transactions (wallet_id)
    WHERE kind = 'OPENING';

-- Worker claim scan (PENDING and PENDING_REFERENCE rows due for retry).
CREATE INDEX wager_tx_pending_scan_idx
    ON wager_transactions (next_attempt_at)
    WHERE status IN ('PENDING', 'PENDING_REFERENCE');

-- Lookup of dependents waiting on a given external reference.
CREATE INDEX wager_tx_reference_lookup_idx
    ON wager_transactions (provider_id, reference_external_transaction_id)
    WHERE reference_external_transaction_id IS NOT NULL;

CREATE INDEX wager_tx_wallet_idx ON wager_transactions (wallet_id, created_at DESC);
CREATE INDEX wager_tx_player_idx ON wager_transactions (player_id, created_at DESC);

CREATE TABLE wallet_ledger_entries (
    id                  UUID PRIMARY KEY,
    wallet_id           UUID        NOT NULL REFERENCES wallets (id),
    transaction_id      UUID        NOT NULL REFERENCES wager_transactions (id),
    direction           TEXT        NOT NULL,
    amount_minor        BIGINT      NOT NULL,
    currency            CHAR(3)     NOT NULL,
    balance_before_minor BIGINT     NOT NULL,
    balance_after_minor  BIGINT     NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL,
    CONSTRAINT ledger_direction CHECK (direction IN ('DEBIT', 'CREDIT')),
    CONSTRAINT ledger_amount_positive CHECK (amount_minor > 0),
    CONSTRAINT ledger_balances_non_negative CHECK (balance_before_minor >= 0 AND balance_after_minor >= 0),
    CONSTRAINT ledger_math CHECK (
        (direction = 'DEBIT' AND balance_after_minor = balance_before_minor - amount_minor)
        OR (direction = 'CREDIT' AND balance_after_minor = balance_before_minor + amount_minor)
    ),
    -- A transaction moves one wallet at most once: blocks duplicate movement.
    CONSTRAINT ledger_wallet_transaction_unique UNIQUE (wallet_id, transaction_id)
);

CREATE INDEX wallet_ledger_entries_wallet_idx
    ON wallet_ledger_entries (wallet_id, created_at DESC, id DESC);

-- Append-only enforcement: any UPDATE, DELETE or TRUNCATE on the ledger aborts
-- the statement/transaction.
CREATE OR REPLACE FUNCTION wallet_ledger_entries_immutable()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'wallet_ledger_entries is append-only (attempted %)', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER wallet_ledger_entries_no_update
    BEFORE UPDATE OR DELETE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION wallet_ledger_entries_immutable();

CREATE TRIGGER wallet_ledger_entries_no_truncate
    BEFORE TRUNCATE ON wallet_ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION wallet_ledger_entries_immutable();

-- Terminal wager transactions are immutable: corrections require new
-- transactions (append-only ledger semantics).
CREATE OR REPLACE FUNCTION wager_transactions_terminal_immutable()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF OLD.status IN ('PROCESSED', 'REJECTED', 'FAILED') THEN
        RAISE EXCEPTION 'wager_transactions % is terminal (%) and cannot be modified', OLD.id, OLD.status
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER wager_transactions_no_terminal_update
    BEFORE UPDATE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION wager_transactions_terminal_immutable();

CREATE TABLE inbox_messages (
    id           UUID PRIMARY KEY,
    consumer_name TEXT        NOT NULL,
    message_id   TEXT        NOT NULL,
    payload_hash TEXT        NOT NULL,
    status       TEXT        NOT NULL,
    received_at  TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    CONSTRAINT inbox_status CHECK (status IN ('RECEIVED', 'COMPLETED')),
    CONSTRAINT inbox_consumer_message_unique UNIQUE (consumer_name, message_id)
);

CREATE TABLE outbox_events (
    event_id       UUID PRIMARY KEY,
    aggregate_id   UUID        NOT NULL,
    event_type     TEXT        NOT NULL,
    payload        JSONB       NOT NULL,
    occurred_at    TIMESTAMPTZ NOT NULL,
    attempts       INTEGER     NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL,
    published_at   TIMESTAMPTZ,
    locked_by      TEXT,
    locked_at      TIMESTAMPTZ,
    last_error     TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT outbox_attempts_non_negative CHECK (attempts >= 0)
);

-- Publisher claim scan: only unpublished events whose backoff has elapsed.
CREATE INDEX outbox_events_pending_idx
    ON outbox_events (next_attempt_at, occurred_at)
    WHERE published_at IS NULL;

CREATE INDEX outbox_events_aggregate_idx
    ON outbox_events (aggregate_id, occurred_at);
