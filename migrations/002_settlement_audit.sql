-- Migration 002: Settlement-key matching + reconciliation audit trail
--
-- Indian bank NEFT credits settle in batches identified by a settlement_id,
-- not by payment-level UTR numbers. settlement_id becomes the primary match
-- key; utr_number remains available as a fallback match key only.

ALTER TABLE vendor_transactions ADD COLUMN settlement_id VARCHAR(255);

CREATE INDEX idx_vendor_txn_settlement_id ON vendor_transactions(settlement_id);

-- Audit trail for every reconciliation decision.
-- method is VARCHAR with a CHECK instead of a Postgres ENUM so new methods
-- can be introduced later without a migration.
CREATE TABLE reconciliation_log (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    vendor_transaction_id UUID REFERENCES vendor_transactions(id) ON DELETE CASCADE,
    bank_transaction_id UUID REFERENCES bank_transactions(id) ON DELETE SET NULL,
    method VARCHAR(20) NOT NULL CHECK (method IN ('deterministic', 'agent', 'unresolved')),
    confidence NUMERIC(4, 3) CHECK (confidence IS NULL OR (confidence >= 0.000 AND confidence <= 1.000)),
    reasoning TEXT,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX idx_reconciliation_log_vendor_txn ON reconciliation_log(vendor_transaction_id);
