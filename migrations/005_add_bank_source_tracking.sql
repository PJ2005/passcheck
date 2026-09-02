-- Migration 005: Track bank statement source for demo visibility
--
-- Synthetic bank data in this demo can come from two distinct paths:
--   * cmd/seedgen's 56-record reconciliation batch (seedgen)
--   * internal/demo/mock_sources.go's Setu-AA-shaped mock (setu_aa_mock)
-- A source column makes the two distinguishable without conflating them,
-- and mirrors how a production system would tag statement ingestion origin.

ALTER TABLE bank_transactions ADD COLUMN source VARCHAR(50) DEFAULT 'seedgen';

CREATE INDEX idx_bank_txn_source ON bank_transactions(source);
