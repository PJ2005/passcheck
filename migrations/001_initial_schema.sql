CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

-- KYC States based on Setu DG sandbox APIs
CREATE TYPE kyc_status_enum AS ENUM ('PENDING', 'SUCCESS', 'FAILED');

-- AA Consent & Session States
CREATE TYPE consent_status_enum AS ENUM ('PENDING', 'ACTIVE', 'REJECTED', 'REVOKED');
CREATE TYPE session_status_enum AS ENUM ('PENDING', 'COMPLETED', 'FAILED');

-- Transaction & Reconciliation States
CREATE TYPE txn_type_enum AS ENUM ('CREDIT', 'DEBIT');
CREATE TYPE recon_status_enum AS ENUM ('UNMATCHED', 'MATCHED', 'DISPUTED');

CREATE TABLE merchants (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    phone_number VARCHAR(15) UNIQUE NOT NULL,
    
    -- PAN Verification Data
    pan VARCHAR(10) UNIQUE,
    pan_status kyc_status_enum DEFAULT 'PENDING',
    pan_registered_name VARCHAR(255), -- Extracted from Setu response
    
    -- GST Verification Data
    gstin VARCHAR(15) UNIQUE,
    gst_status kyc_status_enum DEFAULT 'PENDING',
    gst_registered_name VARCHAR(255), -- Extracted from Setu response
    
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE merchant_bank_accounts (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    merchant_id UUID REFERENCES merchants(id) ON DELETE CASCADE,
    
    -- Setu RPD specific fields
    rpd_request_id VARCHAR(255) UNIQUE NOT NULL, -- Returned upon initiating RPD
    rpd_status kyc_status_enum DEFAULT 'PENDING',
    
    -- Populated ONLY AFTER the merchant makes the ₹1 UPI payment
    account_number VARCHAR(50), 
    ifsc_code VARCHAR(11),
    verified_account_name VARCHAR(255), 
    
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE aa_consents (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    merchant_id UUID REFERENCES merchants(id) ON DELETE CASCADE,
    bank_account_id UUID REFERENCES merchant_bank_accounts(id),
    
    -- Setu AA specific fields
    setu_request_id VARCHAR(255) UNIQUE NOT NULL, -- Initial handle before user approval
    setu_consent_id VARCHAR(255) UNIQUE,          -- Final ID after approval (used for data fetch)
    vua VARCHAR(255),                             -- e.g., 9999999999@onemoney
    status consent_status_enum DEFAULT 'PENDING',
    
    -- Timeframes allowed by the consent
    valid_from TIMESTAMPTZ,
    valid_until TIMESTAMPTZ,
    
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE aa_data_sessions (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    consent_id UUID REFERENCES aa_consents(id) ON DELETE CASCADE,
    
    -- Setu Session specific fields
    setu_session_id VARCHAR(255) UNIQUE NOT NULL,
    status session_status_enum DEFAULT 'PENDING',
    
    -- The specific date range requested in this payload
    data_range_from TIMESTAMPTZ NOT NULL,
    data_range_to TIMESTAMPTZ NOT NULL,
    
    created_at TIMESTAMPTZ DEFAULT NOW(),
    completed_at TIMESTAMPTZ
);

-- Store API keys for Razorpay, Paytm, Pine Labs, etc.
CREATE TABLE vendor_integrations (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    merchant_id UUID REFERENCES merchants(id) ON DELETE CASCADE,
    vendor_name VARCHAR(50) NOT NULL,
    encrypted_credentials TEXT NOT NULL, -- AES encrypted JSON
    is_active BOOLEAN DEFAULT TRUE,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE(merchant_id, vendor_name)
);

-- THE SOURCE: Daily data pulled from Vendor APIs (Razorpay, Pine Labs, etc.)
CREATE TABLE vendor_transactions (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    vendor_integration_id UUID REFERENCES vendor_integrations(id) ON DELETE CASCADE,
    vendor_txn_id VARCHAR(255) NOT NULL,
    amount DECIMAL(12, 2) NOT NULL,
    utr_number VARCHAR(50), -- The Golden Key for matching
    settlement_date DATE,
    recon_status recon_status_enum DEFAULT 'UNMATCHED',
    created_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE(vendor_integration_id, vendor_txn_id)
);

-- THE DESTINATION: Decrypted ReBIT JSON data pulled from Setu AA
CREATE TABLE bank_transactions (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    bank_account_id UUID REFERENCES merchant_bank_accounts(id) ON DELETE CASCADE,
    amount DECIMAL(12, 2) NOT NULL,
    txn_type txn_type_enum NOT NULL,
    narration TEXT,
    utr_number VARCHAR(50), -- Extracted from FI data
    txn_date TIMESTAMPTZ NOT NULL,
    closing_balance DECIMAL(12, 2),
    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- THE LEDGER: Locking in successful UTR matches
CREATE TABLE reconciled_matches (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    vendor_transaction_id UUID REFERENCES vendor_transactions(id) UNIQUE,
    bank_transaction_id UUID REFERENCES bank_transactions(id) UNIQUE,
    matched_at TIMESTAMPTZ DEFAULT NOW()
);

-- Indexes for rapid UTR matching during the cron job
CREATE INDEX idx_vendor_txn_utr ON vendor_transactions(utr_number);
CREATE INDEX idx_bank_txn_utr ON bank_transactions(utr_number);

-- Indexes for time-series filtering
CREATE INDEX idx_vendor_txn_date ON vendor_transactions(settlement_date);
CREATE INDEX idx_bank_txn_date ON bank_transactions(txn_date);

-- Index for fetching unresolved transactions for the merchant UI
CREATE INDEX idx_vendor_txn_status ON vendor_transactions(vendor_integration_id, recon_status);
