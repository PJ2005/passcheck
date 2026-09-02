-- Migration 006: Duplicate ingestion detection and suppression
--
-- Multiple ingestion paths (e.g. Setu AA webhook vs end-of-day statement pull,
-- or a gateway webhook retry) can report the same underlying money twice.
-- Rather than sending known duplicates through matching, we tag the later
-- record as a suppressed duplicate and keep the earliest as canonical.
-- This migration adds the supporting columns and widens the allowed enums
-- so the suppression can be audited.

-- 1. Extend recon_status to allow suppressed duplicates.
--    Existing rows stay UNMATCHED/MATCHED; new duplicates become DUPLICATE_SUPPRESSED
--    and are then excluded from the engine's UNMATCHED work queue by definition.
ALTER TYPE recon_status_enum ADD VALUE 'DUPLICATE_SUPPRESSED';

-- 2. Allow reconciliation_log to record a 'duplicate' decision.
--    This is a positive identification, not a failed match, so 'unresolved'
--    would be misleading.
ALTER TABLE reconciliation_log DROP CONSTRAINT reconciliation_log_method_check;
ALTER TABLE reconciliation_log ADD CONSTRAINT reconciliation_log_method_check
    CHECK (method IN ('deterministic', 'agent', 'unresolved', 'duplicate'));

-- 3. Track which record a duplicate is a copy of (self-referential, nullable).
ALTER TABLE vendor_transactions ADD COLUMN duplicate_of UUID REFERENCES vendor_transactions(id) ON DELETE SET NULL;
CREATE INDEX idx_vendor_txn_duplicate_of ON vendor_transactions(duplicate_of);

ALTER TABLE bank_transactions ADD COLUMN duplicate_of UUID REFERENCES bank_transactions(id) ON DELETE SET NULL;
CREATE INDEX idx_bank_txn_duplicate_of ON bank_transactions(duplicate_of);
