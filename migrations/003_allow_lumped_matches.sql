-- Migration 003: Allow lumped settlement matches (many-to-one)
--
-- A single bank NEFT credit can settle multiple vendor settlement batches,
-- so one bank transaction may legitimately link to several vendor
-- transactions in reconciled_matches.
--
-- vendor_transaction_id keeps its UNIQUE constraint: each vendor transaction
-- still resolves to exactly one outcome.

-- NOTE: this name follows Postgres's auto-generated convention for a
-- column-level UNIQUE (<table>_<column>_key). If this statement fails with
-- "constraint does not exist", verify the actual name via \d reconciled_matches
-- in psql and adjust accordingly.
ALTER TABLE reconciled_matches DROP CONSTRAINT reconciled_matches_bank_transaction_id_key;

-- Non-unique index to keep bank-transaction lookups fast now that
-- multiple rows can share the same bank_transaction_id value.
CREATE INDEX idx_reconciled_matches_bank_txn ON reconciled_matches(bank_transaction_id);
