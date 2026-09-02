package demo

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This function generates synthetic data shaped like a real Setu AA response for demo purposes. It does not call Setu's API. See internal/setu/ for the actual (unused-in-demo) Setu AA client implementation.
func GenerateMockBankStatementData(ctx context.Context, db *pgxpool.Pool, merchantID string, count int) error {
	if count <= 0 {
		count = 10
	}

	// Resolve a bank account for this merchant to attach synthetic credits to.
	// Model as if the ReBITFI Accounts[].Data.Account came from Setu AA.
	var bankAccountID string
	err := db.QueryRow(ctx, `SELECT id FROM merchant_bank_accounts WHERE merchant_id = $1 LIMIT 1`, merchantID).Scan(&bankAccountID)
	if err != nil {
		if err == pgx.ErrNoRows {
			// Create a placeholder bank account so mock generation can proceed on a fresh merchant.
			err = db.QueryRow(ctx, `
				INSERT INTO merchant_bank_accounts (merchant_id, rpd_request_id, account_number, ifsc_code, verified_account_name)
				VALUES ($1, $2, '50100234567890', 'HDFC0001234', 'MOCK AA PLACEHOLDER')
				RETURNING id
			`, merchantID, fmt.Sprintf("rpd_mockaa_%d", rand.Intn(1000000))).Scan(&bankAccountID)
			if err != nil {
				return fmt.Errorf("failed to create placeholder bank account for mock AA data: %w", err)
			}
		} else {
			return fmt.Errorf("failed to resolve bank account for merchant %s: %w", merchantID, err)
		}
	}

	// Generate count synthetic bank credits shaped like ReBITFI Transaction entries.
	// ReBITFI shape (aa_session.go):
	//   Type: "CREDIT" | "DEBIT"
	//   Amount: string numeric e.g. "1250.50"
	//   Narration: string e.g. "NEFT CR:UTR.../RAZORPAY SETL ..."
	//   Reference: UTR string
	//   TransactionTimestamp: RFC3339 timestamp
	for i := 0; i < count; i++ {
		// Amount: 500 to 50000, 2 decimals, mimics Amount string in ReBITFI then parsed to numeric
		amount := math.Round((500+rand.Float64()*49500)*100) / 100
		utr := fmt.Sprintf("UTR%013d", 1000000000000+rand.Int63n(8999999999999))
		// Narration mimics bank statement narration as it appears inside ReBITFI Narration field
		// Keep it plausible for reconciliation: include a settlement-like token occasionally so
		// deterministic engine has something to reason over, but not always.
		var narration string
		if rand.Float64() < 0.6 {
			setl := fmt.Sprintf("setl_%s", randString(14))
			narration = fmt.Sprintf("NEFT Cr - %s - RAZORPAY SETL %s - REF %09d", utr, setl, 100000000+rand.Intn(900000000))
		} else {
			narration = fmt.Sprintf("NEFT Cr - %s - SALARY CREDIT - REF %09d", utr, 100000000+rand.Intn(900000000))
		}
		// TransactionTimestamp: random within last 7 days, mapped to txn_date
		daysAgo := rand.Intn(7)
		hour := 10 + rand.Intn(10)
		min := rand.Intn(60)
		base := time.Now().AddDate(0, 0, -daysAgo)
		txnDate := time.Date(base.Year(), base.Month(), base.Day(), hour, min, 0, 0, base.Location())

		// Map ReBITFI fields -> bank_transactions columns:
		//   Type -> txn_type
		//   Amount -> amount
		//   Narration -> narration
		//   Reference -> utr_number
		//   TransactionTimestamp -> txn_date
		//   source -> 'setu_aa_mock' to distinguish from seedgen's 'seedgen'
		_, err := db.Exec(ctx, `
			INSERT INTO bank_transactions (bank_account_id, amount, txn_type, narration, utr_number, txn_date, source)
			VALUES ($1, $2, 'CREDIT', $3, $4, $5, 'setu_aa_mock')
		`, bankAccountID, amount, narration, utr, txnDate)
		if err != nil {
			// If source column doesn't exist yet (migration not applied), fallback without it
			if isUndefinedColumn(err) {
				_, fallbackErr := db.Exec(ctx, `
					INSERT INTO bank_transactions (bank_account_id, amount, txn_type, narration, utr_number, txn_date)
					VALUES ($1, $2, 'CREDIT', $3, $4, $5)
				`, bankAccountID, amount, narration, utr, txnDate)
				if fallbackErr != nil {
					return fmt.Errorf("failed to insert mock bank transaction %d: %w", i, fallbackErr)
				}
			} else {
				return fmt.Errorf("failed to insert mock bank transaction %d: %w", i, err)
			}
		}
	}

	return nil
}

func GenerateMockPhonePeData(ctx context.Context, db *pgxpool.Pool, merchantID string, count int) error {
	if count <= 0 {
		count = 5
	}

	// Ensure a PhonePe vendor_integration exists for this merchant.
	var vendorIntegrationID string
	err := db.QueryRow(ctx, `SELECT id FROM vendor_integrations WHERE merchant_id = $1 AND vendor_name = 'PhonePe' LIMIT 1`, merchantID).Scan(&vendorIntegrationID)
	if err != nil {
		if err == pgx.ErrNoRows {
			err = db.QueryRow(ctx, `
				INSERT INTO vendor_integrations (merchant_id, vendor_name, encrypted_credentials)
				VALUES ($1, 'PhonePe', '{"mock": "phonepe_keys"}')
				RETURNING id
			`, merchantID).Scan(&vendorIntegrationID)
			if err != nil {
				return fmt.Errorf("failed to create PhonePe vendor integration: %w", err)
			}
		} else {
			return fmt.Errorf("failed to resolve PhonePe vendor integration: %w", err)
		}
	}

	// Generate count synthetic vendor transactions shaped like PhonePe's StandardVendorTxn.
	// See internal/vendors/phonepe/client.go: FetchSettlements maps PhonePe settlement
	// response into StandardVendorTxn{ VendorTxnID, Amount, SettlementID, UTRNumber, SettlementDate, VendorName }.
	// PhonePe's real mock in that file uses VendorTxnID = "T"+timestamp, UTR like "PP_UTR_MOCK_...", Amount 250/500.
	// Our mock mirrors that shape but with varied amounts and dates, and leaves SettlementID empty
	// (as the real PhonePe mapping does — PhonePe settlements in this demo are UTR-centric).
	for i := 0; i < count; i++ {
		// Amount mimics PhonePe settlement amounts (100 to 5000 range, 2 decimals)
		amount := math.Round((100+rand.Float64()*4900)*100) / 100
		// VendorTxnID: PhonePe style "T" + timestamp + index, e.g. "T202501011200001"
		vendorTxnID := fmt.Sprintf("T%s%02d", time.Now().Format("20060102150405"), rand.Intn(100))
		// UTR: PhonePe style "PP_UTR_MOCK_..." or generic "PP_UTR_..."
		utr := fmt.Sprintf("PP_UTR_%05d", rand.Intn(99999))
		if rand.Float64() < 0.5 {
			utr = fmt.Sprintf("PP_UTR_MOCK_%05d", rand.Intn(99999))
		}
		// SettlementDate: random within last 7 days
		daysAgo := rand.Intn(7)
		hour := 10 + rand.Intn(8)
		min := rand.Intn(60)
		base := time.Now().AddDate(0, 0, -daysAgo)
		settlementDate := time.Date(base.Year(), base.Month(), base.Day(), hour, min, 0, 0, base.Location())

		// Insert as vendor_transactions; settlement_id left NULL to match PhonePe's real StandardVendorTxn mapping
		// where SettlementID is empty string (PhonePe mock data is UTR-based).
		_, err := db.Exec(ctx, `
			INSERT INTO vendor_transactions (vendor_integration_id, vendor_txn_id, amount, utr_number, settlement_id, settlement_date, recon_status)
			VALUES ($1, $2, $3, $4, NULL, $5, 'UNMATCHED')
		`, vendorIntegrationID, fmt.Sprintf("%s_%04d", vendorTxnID, rand.Intn(10000)), amount, utr, settlementDate)
		if err != nil {
			return fmt.Errorf("failed to insert mock PhonePe vendor transaction %d: %w", i, err)
		}
	}

	return nil
}

func GenerateMockPineLabsData(ctx context.Context, db *pgxpool.Pool, merchantID string, count int) error {
	if count <= 0 {
		count = 5
	}

	// Ensure a PineLabs vendor_integration exists for this merchant.
	var vendorIntegrationID string
	err := db.QueryRow(ctx, `SELECT id FROM vendor_integrations WHERE merchant_id = $1 AND vendor_name = 'PineLabs' LIMIT 1`, merchantID).Scan(&vendorIntegrationID)
	if err != nil {
		if err == pgx.ErrNoRows {
			err = db.QueryRow(ctx, `
				INSERT INTO vendor_integrations (merchant_id, vendor_name, encrypted_credentials)
				VALUES ($1, 'PineLabs', '{"mock": "pinelabs_keys"}')
				RETURNING id
			`, merchantID).Scan(&vendorIntegrationID)
			if err != nil {
				return fmt.Errorf("failed to create PineLabs vendor integration: %w", err)
			}
		} else {
			return fmt.Errorf("failed to resolve PineLabs vendor integration: %w", err)
		}
	}

	// Generate count synthetic vendor transactions shaped like Pine Labs settlement reports.
	// Pine Labs publicly documented settlement fields (e.g. txnId, settlementId, amount, UTR, settlementDate)
	// map to StandardVendorTxn. We generate them inline for the mock; alternatively we could call
	// pinelabs.Provider.FetchSettlements, but that mock already generates 5-10 and doesn't accept count.
	// Inline generation respects the requested count and keeps the flow simple.
	for i := 0; i < count; i++ {
		amount := math.Round((200+rand.Float64()*4800)*100) / 100
		// Pine Labs style settlement/txn IDs
		settlementID := fmt.Sprintf("pl_setl_%s", randString(12))
		vendorTxnID := fmt.Sprintf("PL_TXN_%s%04d", time.Now().Format("20060102"), rand.Intn(10000))
		utr := fmt.Sprintf("PL_UTR%010d", rand.Intn(9999999999))
		daysAgo := rand.Intn(7)
		hour := 11 + rand.Intn(6)
		min := rand.Intn(60)
		base := time.Now().AddDate(0, 0, -daysAgo)
		settlementDate := time.Date(base.Year(), base.Month(), base.Day(), hour, min, 0, 0, base.Location())

		_, err := db.Exec(ctx, `
			INSERT INTO vendor_transactions (vendor_integration_id, vendor_txn_id, amount, utr_number, settlement_id, settlement_date, recon_status)
			VALUES ($1, $2, $3, $4, $5, $6, 'UNMATCHED')
		`, vendorIntegrationID, vendorTxnID, amount, utr, settlementID, settlementDate)
		if err != nil {
			return fmt.Errorf("failed to insert mock Pine Labs vendor transaction %d: %w", i, err)
		}
	}

	return nil
}

func randString(n int) string {
	const alphanum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = alphanum[rand.Intn(len(alphanum))]
	}
	return string(b)
}

func isUndefinedColumn(err error) bool {
	if err == nil {
		return false
	}
	// pgx error string for missing column contains "column" and "does not exist" and the column name
	msg := err.Error()
	return contains(msg, "column") && contains(msg, "does not exist") && contains(msg, "source")
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i <= len(s)-len(substr); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}
