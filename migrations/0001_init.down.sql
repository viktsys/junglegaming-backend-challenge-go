-- 0001_init.down.sql
DROP TRIGGER IF EXISTS wallet_ledger_entries_no_update ON wallet_ledger_entries;
DROP TRIGGER IF EXISTS wallet_ledger_entries_no_truncate ON wallet_ledger_entries;
DROP FUNCTION IF EXISTS wallet_ledger_entries_immutable();
DROP TRIGGER IF EXISTS wager_transactions_no_terminal_update ON wager_transactions;
DROP FUNCTION IF EXISTS wager_transactions_terminal_immutable();
DROP TABLE IF EXISTS outbox_events;
DROP TABLE IF EXISTS inbox_messages;
DROP TABLE IF EXISTS wallet_ledger_entries;
DROP TABLE IF EXISTS wager_transactions;
DROP TABLE IF EXISTS wallets;
