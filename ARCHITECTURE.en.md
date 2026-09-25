# Architecture

> Language: this is the English version. The default Portuguese version is at
> [ARCHITECTURE.md](ARCHITECTURE.md).

Distributed wager processing service in Go, composed with Uber Fx, backed by
PostgreSQL, AWS SQS (LocalStack locally) and Keycloak as the external OAuth
2.0/OIDC identity provider.

- [1. Component overview](#1-component-overview)
- [2. Money](#2-money)
- [3. Transaction boundaries](#3-transaction-boundaries)
- [4. Idempotency](#4-idempotency)
- [5. Concurrency control](#5-concurrency-control)
- [6. Wager transaction state machine](#6-wager-transaction-state-machine)
- [7. Operations and reversals](#7-operations-and-reversals)
- [8. Pending references](#8-pending-references)
- [9. Inbox](#9-inbox)
- [10. Transactional outbox](#10-transactional-outbox)
- [11. Authentication and authorization](#11-authentication-and-authorization)
- [12. SQS contract](#12-sqs-contract)
- [13. Fx composition and lifecycle](#13-fx-composition-and-lifecycle)
- [14. Failure taxonomy](#14-failure-taxonomy)
- [15. Observability](#15-observability)
- [16. Testing strategy](#16-testing-strategy)
- [17. Interpretations, limitations and unfinished work](#17-interpretations-limitations-and-unfinished-work)

## 1. Component overview

```
                       ┌──────────────────────────────┐
   providers ──HTTPS──▶│  HTTP API (chi)              │
   internal  ──HTTPS──▶│  authn: Keycloak OIDC/JWKS   │
                       │  authz: roles + providerId   │
                       └──────────────┬───────────────┘
                                      │
                       ┌──────────────▼───────────────┐
                       │ application use cases        │
                       │  wager.Process / ResumeDue   │
                       │  wallets.Open/Reconcile      │
                       └───────┬──────────────┬───────┘
                               │              │
                 ┌─────────────▼───┐   ┌──────▼────────────────┐
   SQS ──consume─▶│ PostgreSQL      │   │ outbox publisher      │──publish──▶ SQS
   (inbox)        │ wallets         │   │ reference resolver    │            (events)
                 │ wager_tx        │   └───────────────────────┘
                 │ ledger (append) │
                 │ inbox / outbox  │
                 └─────────────────┘
```

Package layout:

| Path | Responsibility |
| --- | --- |
| `internal/domain` | Money, Wallet, WagerTransaction, LedgerEntry, events, error taxonomy. No infrastructure imports. |
| `internal/ports` | Interfaces and value types the application depends on (repositories, tx manager, clock, metrics, messaging). |
| `internal/app` | Use cases: `wager`, `wallets`, `outbox`, `references`, `consumer`. |
| `internal/adapters` | pgx repositories, SQS client/consumer, OIDC verifier, HTTP handlers. |
| `internal/platform` | Migration runner, worker loop, logging, metrics. |
| `internal/bootstrap` | Fx modules and lifecycle wiring. |
| `migrations` | Versioned SQL, embedded into the binaries. |

The domain and application layers never import pgx, SQS, HTTP or Fx.

## 2. Money

`internal/domain/money` is an immutable value object:

- Representation: `int64` **minor units** (centavos) plus an ISO 4217
  `Currency`. BRL range: `[-92_233_720_368_547_758.08, +92_233_720_368_547_758.07]`.
- Parsing is strict: plain decimal notation, at most 2 fraction digits,
  no scientific notation, no `NaN`/`Infinity`, no `+`, no thousands
  separators, no whitespace. JSON amounts must be **strings**; JSON numbers are
  rejected, so `float32`/`float64` never participate in parsing, arithmetic,
  serialization or persistence.
- Overflow is checked in parsing, addition, subtraction and negation.
- `Parse` accepts negative values because internal calculations (differences,
  rollback movements) need them; external financial inputs go through
  `ParseNonNegative`/`ParsePositive` (and additionally the per-kind rules).
- Currencies must match for arithmetic and comparison.
- Serialization is always fixed-scale: `{"amount":"25.00","currency":"BRL"}`.
- Persistence uses `BIGINT` minor units; the ledger stores balances in the same
  representation, so reads/writes are exact.

## 3. Transaction boundaries

Access to PostgreSQL uses `pgx` with explicit SQL. `ports.TxManager` owns
transactions; repositories resolve the current transaction from the context
(`postgres.querierFrom`). Nested `WithinTransaction` calls create savepoints,
which lets the SQS consumer wrap the inbox claim, the domain changes and the
inbox completion in one transaction while the wager service still retries
internally.

Per use case the atomic unit is:

| Use case | Single transaction |
| --- | --- |
| `wallets.Open` | wallet row + OPENING transaction + ledger credit + 2 outbox events |
| `wager.Process` (BET/WIN/LOSS) | transaction row + wallet update + ledger entry + outbox events |
| `wager.Process` (REFUND/ROLLBACK resolved) | transaction row (+ resolved reference) + wallet update + ledger entry + outbox events |
| `wager.Process` (unresolved reference) | transaction row (`PENDING_REFERENCE`) + outbox pending event |
| SQS message | inbox claim + wager processing + inbox completion |
| outbox publisher | claim/mark only (publication happens outside the transaction, see §10) |

No event is published before the transaction that produced it commits, because
events are only ever written to `outbox_events` inside that transaction and
published by a separate worker.

## 4. Idempotency

### Key and payload

- HTTP requires the `Idempotency-Key` header. The server uses the header value
  verbatim and never substitutes `{providerId}:{externalTransactionId}`.
- SQS uses `data.idempotencyKey` and additionally deduplicates by the envelope
  `messageId` through the inbox.
- `(providerId, externalTransactionId)` is the financial identity: it has a
  unique index and cannot be replayed with a different key or payload.
- `(providerId, idempotencyKey)` has its own unique index; reusing a key for a
  different external transaction is a conflict.

### Canonical payload hash

`wager.ComputePayloadHash` computes SHA-256 over canonical JSON
(`internal/app/wager/hash.go`):

- keys sorted lexicographically (Go map marshaling);
- `amount` normalized to the fixed-scale decimal string, `currency` to the ISO
  code (so `"25"` and `"25.00"` hash identically);
- UUIDs in lower-case canonical form;
- `referenceExternalTransactionId` always present, empty when absent;
- the idempotency key and all transport metadata (headers, timestamps, message
  ids, correlation ids) are excluded.

Both HTTP and SQS build the same `wager.ProcessCommand`, so the hash is
identical across transports.

### Outcomes

| Situation | Result |
| --- | --- |
| Same key, same payload | persisted result returned, `idempotentReplay: true` |
| Same external transaction, other key, same payload | replay (no reapplication) |
| Same key or external transaction, different payload | `409 IDEMPOTENCY_CONFLICT` |
| Same `messageId`, same body (SQS) | inbox row completed, message acknowledged, no reapplication |
| Same `messageId`, different body | permanent failure → DLQ |

Replay returns the balance observed at the original processing, persisted in
`wager_transactions.result_balance_minor`, not the current wallet balance.

## 5. Concurrency control

Coordination is **per wallet** using optimistic concurrency control:

```sql
UPDATE wallets
SET balance_minor = $2, version = version + 1, updated_at = $3
WHERE id = $1 AND version = $4
```

- Zero rows affected means another writer won; the operation is retried from a
  fresh read, bounded by `MaxWalletRetries` (default 5).
- Debits are validated against the in-memory aggregate **and** by the
  `wallets_balance_non_negative` check constraint, so a lost update can never
  produce a negative balance even if the application logic were wrong.
- `wallet_ledger_entries (wallet_id, transaction_id)` is unique, so a
  transaction can move a wallet at most once. The ledger is append-only:
  triggers reject `UPDATE`, `DELETE` and `TRUNCATE`.
- Unique violations abort the PostgreSQL transaction; the retry loop starts a
  new transaction and re-reads the committed winner (this is why duplicate
  inserts are also retryable errors, see `wager.isRetryable`).
- There is no global lock. Different wallets update independent rows and are
  processed in parallel.

Reversal races are additionally serialized by re-checking `FindReversal` after
the wallet version conflict and by the partial unique index
`wager_tx_reference_reversal_unique (reference_transaction_id, kind) WHERE
status = 'PROCESSED'`.

## 6. Wager transaction state machine

```
            ┌─────────┐
            │ PENDING │────────────┐
            └────┬────┘            │
                 │                 │
   reference     │                 │  synchronous success
   unavailable   ▼                 ▼
      ┌────────────────────┐   ┌───────────┐
      │ PENDING_REFERENCE  │──▶│ PROCESSED │
      └─────────┬──────────┘   └───────────┘
                │              ┌──────────┐
                ├─────────────▶│ REJECTED │
                │              └──────────┘
                │              ┌────────┐
                └─────────────▶│ FAILED │
                               └────────┘
```

- `PENDING`: accepted, not yet concluded. New operations are inserted and
  concluded in the same transaction, so a committed `PENDING` is only possible
  if an explicit asynchronous acceptance is introduced. The recovery worker
  still claims stale `PENDING` rows, satisfying the durable-resume requirement.
- `PENDING_REFERENCE`: waiting for a reference; retried with exponential
  backoff by the reference resolver.
- `PROCESSED`, `REJECTED`, `FAILED`: terminal. `Update` only touches rows whose
  status is `PENDING`/`PENDING_REFERENCE`, and the database trigger
  `wager_transactions_no_terminal_update` rejects any `UPDATE` on a terminal
  row, so terminal states are immutable at both layers.
- Transitions are validated by `wagering.assertTransition`; `panic` is never
  used for business rejections.
- Transient vs permanent: DB/broker errors are retried (SQS visibility,
  outbox backoff). Business rejections are persisted with a stable
  `failure_code`; permanent infrastructure failures would be recorded as
  `FAILED` (the code path exists and is unit-tested, but no current adapter
  classifies an error as permanent `FAILED` — see §17).

## 7. Operations and reversals

| Kind | Movement | Rules |
| --- | --- | --- |
| `BET` | DEBIT | positive amount, sufficient balance |
| `WIN` | CREDIT | positive amount, optional BET reference in the same round |
| `LOSS` | none | `money.amount == "0.00"`; no ledger, no version bump; emits `WagerTransactionProcessed` |
| `REFUND` | CREDIT | references a `PROCESSED` BET, same amount |
| `ROLLBACK` | opposite of the reference | references `BET` (credit), `WIN` (debit) or `REFUND` (debit), same amount |

Reference resolution requires agreement on provider, player, wallet, currency
and round. Reversal amounts must equal the referenced amount; partial reversals
are out of scope.

Reversal conflict matrix (each row is rejected with
`failureCode = REVERSAL_CONFLICT`):

| Already successful | New attempt | Allowed? |
| --- | --- | --- |
| REFUND of BET | REFUND of same BET | no |
| ROLLBACK of BET | ROLLBACK of same BET | no |
| REFUND of BET | ROLLBACK of that BET | no (would return the debit twice) |
| ROLLBACK of BET | REFUND of that BET | no |
| any reversal | ROLLBACK of that reversal | no |
| REFUND of BET | ROLLBACK of that REFUND | yes (undoes the credit) |

A rollback that would debit more than the available balance is rejected with
`REVERSAL_INSUFFICIENT_FUNDS`, distinct from `INSUFFICIENT_FUNDS` for a bet.

`OPENING` is internal only: HTTP and SQS reject it (400 / DLQ). It is created
with the wallet and always `PROCESSED`.

## 8. Pending references

When a reversal (or a WIN with a reference) points to an unknown or in-flight
transaction:

1. The operation is committed as `PENDING_REFERENCE` with `attempts = 1` and
   `next_attempt_at = now + backoff`.
2. `WagerTransactionPendingReference` is written to the outbox.
3. The reference resolver (`references.Resolver`) claims due rows with
   `SELECT ... FOR UPDATE SKIP LOCKED LIMIT 1` and resumes them.
4. When a transaction is processed successfully, `WakeDependents` sets
   `next_attempt_at = now()` for all operations waiting on it, so resolution is
   usually immediate rather than waiting for the next backoff tick.
5. Backoff is exponential (`base * 2^(attempt-1)`, capped). After
   `REFERENCE_MAX_ATTEMPTS` the operation is rejected with
   `REFERENCE_NOT_FOUND` and `WagerTransactionRejected` is emitted.
6. If the reference exists but is terminal without success (`REJECTED`/`FAILED`)
   the operation is rejected immediately with `REFERENCE_NOT_PROCESSED`.
7. If the reference is still `PENDING`/`PENDING_REFERENCE`, the wait continues.

All of this survives restarts: state is in PostgreSQL and the worker scans on
every instance.

## 9. Inbox

`inbox_messages (consumer_name, message_id)` is unique. The SQS handler runs,
in one transaction:

1. `INSERT ... ON CONFLICT DO NOTHING RETURNING id` (claim);
2. domain changes through the wager service;
3. `UPDATE ... SET status = 'COMPLETED'`.

Consequences:

- Redelivery after a crash between commit and `DeleteMessage` finds the row and
  is ignored; the message is then deleted.
- Redelivery with a different body for the same `messageId` is a permanent
  failure routed to the DLQ.
- A pending reference is committed with the inbox completion, so the message is
  acknowledged and the resolver takes over.

## 10. Transactional outbox

`outbox_events` stores an immutable JSONB snapshot of the full event envelope.
The publisher (`outbox.Publisher`):

1. claims a batch in a short transaction (`FOR UPDATE SKIP LOCKED`, lock TTL);
2. publishes to SQS outside the transaction;
3. marks the event published (or reschedules with backoff on failure).

Per-aggregate order is preserved by only claiming the earliest unpublished
event of each aggregate, even if another publisher holds its lock. Abandoned
locks are reclaimed after `OUTBOX_LOCK_TTL`. A crash between publish and
confirmation causes a republication with the **same `eventId`**; consumers
deduplicate by `eventId`.

Event envelope:

```json
{
  "eventId": "01a0...",
  "eventType": "WalletBalanceChanged",
  "aggregateId": "01a0...",
  "correlationId": "request or message id",
  "causationId": "transactionId",
  "occurredAt": "2026-09-24T19:33:37.424Z",
  "version": 1,
  "data": { "...": "typed per event" }
}
```

`WalletBalanceChanged.data` contains `walletId`, `transactionId`, `direction`,
`money`, `balanceBefore`, `balanceAfter`, `walletVersion`. Timestamps are UTC
RFC 3339 and amounts are decimal strings.

## 11. Authentication and authorization

- OIDC discovery + JWKS verification via `coreos/go-oidc` against Keycloak.
  Issuer and audience are validated; missing, malformed or expired tokens are
  rejected with 401. No password storage or token issuance in the service.
- Realm roles: `provider` (external providers) and `internal` (wallet service).
- The authenticated identity determines the authorized `providerId` through the
  `provider_id` claim (a hardcoded claim mapper per Keycloak client).
- Providers may only submit operations for their own `providerId` and read
  their own transactions (by internal id or external id); any other provider
  gets 403 and never sees data.
- Wallet creation, wallet reads, ledger reads and reconciliation require the
  `internal` role. `OPENING` is additionally rejected at the domain level.
- Health checks and `/metrics` are public.
- SQS access is controlled by broker credentials; the consumer still enforces
  all domain validations, so a valid broker credential cannot bypass business
  rules.

Keycloak realm, clients, secrets and service-account roles are provisioned
automatically from `deploy/keycloak/realm.json`.

## 12. SQS contract

| Queue | Purpose | MessageGroupId | MessageDeduplicationId |
| --- | --- | --- | --- |
| `wager-transactions.fifo` | inbound operations | `walletId` (per-wallet order) | `idempotencyKey` |
| `wager-transactions-dlq.fifo` | redrive after 5 receives or explicit DLQ routing | `dlq` | `dlq-<messageId>-<nano>` |
| `wager-events.fifo` | outbox integration events | `aggregateId` (per-wallet order) | `eventId` (stable across republications) |
| `wager-events-dlq.fifo` | outbox event DLQ | — | — |

- Long polling, `SQS_VISIBILITY_TIMEOUT` (default 30s), `maxReceiveCount = 5`.
- The message is deleted only after the handler confirms the durable commit.
- Business rejections are durable outcomes and are acknowledged (deleted).
- Invalid or unprocessable messages are explicitly moved to the DLQ and
  deleted; transient failures are left for redelivery and eventually reach the
  DLQ through the redrive policy.
- On `SIGTERM` the consumer stops polling and completes the in-flight message
  within the shutdown grace period; if it cannot, the visibility timeout
  expires and the message is redelivered. The inbox makes the redelivery safe.

Inbound envelope (see `internal/app/consumer/wager.go`):

```json
{
  "messageId": "msg-123",
  "type": "WagerTransactionRequested",
  "occurredAt": "2026-09-08T12:00:00.000Z",
  "data": {
    "providerId": "provider-a",
    "externalTransactionId": "transaction-123",
    "idempotencyKey": "provider-a:transaction-123",
    "playerId": "...",
    "walletId": "...",
    "roundId": "round-987",
    "gameId": "fortune-chimp",
    "kind": "BET",
    "money": { "amount": "25.00", "currency": "BRL" },
    "referenceExternalTransactionId": "optional"
  }
}
```

## 13. Fx composition and lifecycle

`internal/bootstrap/app.go` defines one `fx.Module` per layer:
`config`, `observability`, `postgres`, `messaging`, `auth`, `app`, `http`,
`workers`. Constructors receive their dependencies explicitly; interfaces are
provided with small adapter functions so `ports` stays framework-free.

Lifecycle:

- **Start**: configuration is validated by `config.Load`; migrations run as an
  `fx.Invoke` before any hook; the HTTP server, the SQS consumer and the two
  workers start in `OnStart` hooks.
- **Stop** (LIFO): HTTP stops accepting connections and drains in-flight
  requests (`http.Server.Shutdown`); the SQS consumer stops polling and finishes
  the current message; the outbox publisher and reference resolver stop their
  loops; finally the pgx pool is closed (`fx.OnStop` on the pool provider,
  registered first so it runs last).
- Stop timeout is 30s (`fx.StopTimeout`), and the SQS consumer uses the HTTP
  shutdown timeout as its processing grace.
- `fxevent` events are routed to the structured logger.

## 14. Failure taxonomy

Domain errors carry a `kind` (transport mapping) and a stable `code`
(`internal/domain/apperr`). HTTP mapping:

| Kind | HTTP | Examples |
| --- | --- | --- |
| invalid | 400 | malformed JSON, invalid UUID, invalid kind, missing key |
| unauthorized | 401 | missing/invalid/expired token |
| forbidden | 403 | provider accessing another provider |
| not_found | 404 | unknown wallet or transaction |
| conflict | 409 | idempotency conflict, duplicate wallet |
| rejected | 422 | business rejection with `failureCode` |
| unavailable/internal | 503/500 | transient infrastructure |

Business failure codes (persisted, returned in responses and events):

| Code | Meaning | Correctable |
| --- | --- | --- |
| `INSUFFICIENT_FUNDS` | BET debit exceeds balance | yes (after deposit) |
| `REVERSAL_INSUFFICIENT_FUNDS` | rollback debit exceeds balance | yes |
| `REFERENCE_NOT_FOUND` | reference never arrived (attempts exhausted) | yes (new request) |
| `REFERENCE_NOT_PROCESSED` | reference is terminal without success | no |
| `REFERENCE_MISMATCH` | reference disagrees on player/wallet/currency/round/kind | no |
| `REFERENCE_AMOUNT_MISMATCH` | reversal amount differs | no |
| `REVERSAL_CONFLICT` | duplicate or cross reversal | no |
| `WALLET_NOT_FOUND` | wallet does not exist | yes (after creation) |
| `WALLET_PLAYER_MISMATCH` | wallet belongs to another player | no |
| `CURRENCY_MISMATCH` | wallet and operation currencies differ | no |
| `INVALID_AMOUNT` | zero/negative where positive required | yes |
| `UNSUPPORTED_KIND` | unsupported operation kind | no |

`GET /wagering/transactions/:id` returns `failureCorrectable` to distinguish
correctable from definitive outcomes.

## 15. Observability

- JSON logs (`slog`) with `requestId`, `messageId`, `transactionId`, `walletId`,
  `providerId` where available. No credentials or full financial payloads are
  logged.
- Prometheus metrics at `/metrics`: outcomes by status/kind/failure code,
  idempotent replays, wallet version conflicts, worker retries, DLQ messages,
  reconciliation outcomes, outbox backlog and publish latency, pending
  transactions, HTTP counters and latency.
- `GET /health/live` (process) and `GET /health/ready` (PostgreSQL + SQS).
- OpenTelemetry tracing and dashboards were not implemented (optional).

## 16. Testing strategy

- **Unit** (`go test ./...`): Money parsing/scale/overflow/currency mismatch,
  wallet invariants, transaction state machine and per-kind rules, ledger math,
  event constructors, canonical hash.
- **Integration** (`-tags=integration`, real PostgreSQL/Keycloak/LocalStack):
  migrations, constraints, ledger immutability, terminal-transaction
  immutability, financial flows, idempotency replay/conflict, inbox dedup and
  DLQ routing, SQS redelivery after a simulated crash between commit and
  delete, outbox concurrency, retry, abandoned-lock recovery and
  republication-with-stable-eventId, pending reference resolution and expiry,
  restart preservation, Fx composition start/stop, HTTP authentication
  (missing, malformed and expired tokens)/provider isolation, and the mandatory
  concurrency scenarios (50× same bet, two 80.00 bets over 100.00, distinct
  wallets in parallel) using three independent instances (separate pools).
- **Unit** additionally covers idempotency conflict semantics with in-memory
  repository fakes (`internal/app/wager/service_test.go`).
- `go test -race` is run for both unit and integration suites.

## 17. Interpretations, limitations and unfinished work

Interpretations adopted:

- `idempotencyKey` is mandatory on both transports (the challenge makes the
  HTTP header mandatory and shows it in the SQS envelope).
- Scale is fixed at two decimal places for every currency.
- Rejected operations that cannot reference an existing wallet (unknown
  `walletId`) are not persisted, because `wager_transactions.wallet_id` is a
  foreign key. They are returned as `422 WALLET_NOT_FOUND` and routed to the
  DLQ on the SQS path. All other rejections are persisted and auditable.
- LOSS succeeds without a balance change and stores the observed balance as its
  result; `WalletBalanceChanged` is not emitted.
- For events originating from a resumed transaction the `correlationId` falls
  back to the transaction id, since the original transport correlation is not
  stored on the aggregate.
- A WIN reference is optional; when present it must be a `PROCESSED` BET in the
  same round and the WIN amount may differ from the bet.
- Reversal references must match the original round; this is stricter than
  "same player/wallet/currency" but matches the domain wording.

Known limitations / unfinished work:

- `FAILED` is modeled, persisted and unit-tested, but no adapter currently
  classifies an error as a permanent infrastructure failure; transient errors
  are always retried. A production deployment would add a classifier for
  non-retryable broker/DB errors.
- Outbox events are retried indefinitely with capped backoff (critical
  financial events must not be dropped). There is no administrative "park to
  DLQ after N attempts" switch for the outbox publisher; the events queue DLQ
  covers broker-side failures.
- No load-testing artifacts (optional differential).
- No OpenTelemetry tracing or dashboards (optional differential).
- Double-entry ledger is not implemented (optional differential).
