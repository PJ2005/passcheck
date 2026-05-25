-- DB Seeder Script: Seed Vendor Transactions from Bank Transactions for testing the reconciliation engine

-- We will copy over the bank transactions directly into vendor_transactions to guarantee exact matches!
DO $$
DECLARE
    merchant_id_val UUID;
    integration_id_val UUID;
    v_txn_id_prefix VARCHAR := 'MOCK_VENDOR_TXN_';
    counter INT := 1;
    bt RECORD;
BEGIN
    -- 1. Get the first merchant
    SELECT id INTO merchant_id_val FROM merchants LIMIT 1;
    
    IF merchant_id_val IS NULL THEN
        RAISE NOTICE 'No merchants found, skipping vendor transaction seed.';
        RETURN;
    END IF;

    -- 2. Create a mock vendor integration if it doesn't exist
    SELECT id INTO integration_id_val FROM vendor_integrations WHERE merchant_id = merchant_id_val LIMIT 1;
    
    IF integration_id_val IS NULL THEN
        INSERT INTO vendor_integrations (merchant_id, vendor_name, encrypted_credentials)
        VALUES (merchant_id_val, 'Mock Payment Gateway', '{"mock": "keys"}')
        RETURNING id INTO integration_id_val;
    END IF;

    -- 3. Loop through bank transactions and create matching vendor transactions
    FOR bt IN SELECT * FROM bank_transactions WHERE utr_number IS NOT NULL AND utr_number != '' LOOP
        BEGIN
            INSERT INTO vendor_transactions (
                vendor_integration_id, 
                vendor_txn_id, 
                amount, 
                utr_number, 
                settlement_date, 
                recon_status
            ) VALUES (
                integration_id_val, 
                v_txn_id_prefix || counter, 
                bt.amount, 
                bt.utr_number, 
                bt.txn_date::DATE, 
                'UNMATCHED'
            );
            counter := counter + 1;
        EXCEPTION WHEN unique_violation THEN
            -- Ignore if it already exists
        END;
    END LOOP;
    
    RAISE NOTICE 'Seeded % vendor transactions for testing!', counter - 1;
END $$;
