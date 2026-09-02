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

// GenerateMockBankStatementData generates synthetic bank_transactions rows shaped like a real Setu AA response.
// This function's own generated bank credits (source='setu_aa_mock' and 'setu_aa_mock_duplicate')
// are separate from the bank credits now generated inline by GenerateMockPhonePeData and
// GenerateMockPineLabsData — those are vendor-driven matchable pairs (gateway settlement + bank credit).
// This separation keeps each mock source's data traceable by source tag. It does not call Setu's API.
// See internal/setu/ for the actual (unused-in-demo) Setu AA client implementation.
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
	type bankGen struct {
		amount    float64
		narration string
		utr       string
		txnDate   time.Time
	}
	var generated []bankGen

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
		generated = append(generated, bankGen{amount: amount, narration: narration, utr: utr, txnDate: txnDate})
	}

	// --- Duplicate bank credit batch (2-3 records) ---
	// This simulates a known real-world AA/bank-feed duplication pattern, not a bug
	// in the generator: the same underlying NEFT credit is reported twice — once
	// via a real-time webhook feed and once via an end-of-day statement pull —
	// with identical amount, narration, and UTR but a txn_date a few minutes later.
	// We tag these as 'setu_aa_mock_duplicate' so they are traceable as intentionally duplicated.
	if len(generated) > 0 {
		dupCount := 2 + rand.Intn(2) // 2-3
		if dupCount > len(generated) {
			dupCount = len(generated)
		}
		for i := 0; i < dupCount; i++ {
			orig := generated[rand.Intn(len(generated))]
			dupDate := orig.txnDate.Add(time.Duration(2+rand.Intn(8)) * time.Minute)
			_, err := db.Exec(ctx, `
				INSERT INTO bank_transactions (bank_account_id, amount, txn_type, narration, utr_number, txn_date, source)
				VALUES ($1, $2, 'CREDIT', $3, $4, $5, 'setu_aa_mock_duplicate')
			`, bankAccountID, orig.amount, orig.narration, orig.utr, dupDate)
			if err != nil {
				if isUndefinedColumn(err) {
					_, fallbackErr := db.Exec(ctx, `
						INSERT INTO bank_transactions (bank_account_id, amount, txn_type, narration, utr_number, txn_date)
						VALUES ($1, $2, 'CREDIT', $3, $4, $5)
					`, bankAccountID, orig.amount, orig.narration, orig.utr, dupDate)
					if fallbackErr != nil {
						return fmt.Errorf("failed to insert duplicate mock bank transaction %d: %w", i, fallbackErr)
					}
				} else {
					return fmt.Errorf("failed to insert duplicate mock bank transaction %d: %w", i, err)
				}
			}
		}
	}

	// Immediately detect and suppress duplicates introduced by this batch (or revealed by
	// combination with previously-generated data), so the demo doesn't require a manual step.
	if _, err := DetectCrossSourceDuplicates(ctx, db, merchantID); err != nil {
		return fmt.Errorf("failed to detect cross-source duplicates after bank mock generation: %w", err)
	}

	return nil
}

func GenerateMockPhonePeData(ctx context.Context, db *pgxpool.Pool, merchantID string, count int) error {
	if count <= 0 {
		count = 10
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

	// Resolve bank account for matchable bank rows (same merchant's bank account)
	var bankAccountID string
	err = db.QueryRow(ctx, `SELECT id FROM merchant_bank_accounts WHERE merchant_id = $1 LIMIT 1`, merchantID).Scan(&bankAccountID)
	if err != nil && err != pgx.ErrNoRows {
		return fmt.Errorf("failed to resolve bank account for PhonePe mock: %w", err)
	}
	if bankAccountID == "" {
		err = db.QueryRow(ctx, `
			INSERT INTO merchant_bank_accounts (merchant_id, rpd_request_id, account_number, ifsc_code, verified_account_name)
			VALUES ($1, $2, '50100234567890', 'HDFC0001234', 'MOCK PHONEPE PLACEHOLDER')
			RETURNING id
		`, merchantID, fmt.Sprintf("rpd_phonepe_%d", rand.Intn(1000000))).Scan(&bankAccountID)
		if err != nil {
			return fmt.Errorf("failed to create placeholder bank account for PhonePe mock: %w", err)
		}
	}

	// Honest-by-design: 70% clean, 20% wrinkle, 10% orphan (minimum 1 orphan) with PhonePe's UTR-centric convention.
	// Check internal/vendors/phonepe/client.go — PhonePe's StandardVendorTxn leaves SettlementID empty and is UTR-centric,
	// so clean/wrinkle bank rows embed the same utr_number the vendor row uses for Tier 4 (legacy UTR+amount) matching.
	// Orphan is a genuine "payment initiated but settlement not yet received" case, mirroring cmd/seedgen Category E.
	orphanCount := int(math.Round(float64(count) * 0.1))
	if orphanCount < 1 && count > 0 {
		orphanCount = 1
	}
	if orphanCount > count {
		orphanCount = count
	}
	remaining := count - orphanCount
	wrinkleCount := int(math.Round(float64(count) * 0.2))
	if wrinkleCount > remaining {
		wrinkleCount = remaining
	}
	cleanCount := remaining - wrinkleCount

	// Cross-source duplicate reuse: if other vendor data exists (e.g. Razorpay from seedgen), let 1-2 of the
	// clean rows reuse an existing vendor_txn_id/amount/utr triple from a different integration. This keeps
	// the honest 70/20/10 distribution intact while still exercising cross-source duplicate detection:
	// those reused rows remain within the clean/wrinkle buckets and have matching bank rows, but will be
	// flagged as vendor-side duplicates (same amount+UTR across integrations) and suppressed.
	var dupCandidates []struct {
		vendorTxnID string
		amount      float64
		utr         string
	}
	dupNeed := 0
	if count >= 3 {
		rows, qErr := db.Query(ctx, `
			SELECT vt.vendor_txn_id, vt.amount, vt.utr_number
			FROM vendor_transactions vt
			JOIN vendor_integrations vi ON vt.vendor_integration_id = vi.id
			WHERE vi.merchant_id = $1 AND vi.id != $2 AND vt.utr_number IS NOT NULL AND vt.utr_number != ''
			ORDER BY RANDOM() LIMIT 2
		`, merchantID, vendorIntegrationID)
		if qErr == nil {
			defer rows.Close()
			for rows.Next() {
				var vtid string
				var amt float64
				var utr *string
				if err := rows.Scan(&vtid, &amt, &utr); err == nil && utr != nil && *utr != "" {
					dupCandidates = append(dupCandidates, struct {
						vendorTxnID string
						amount      float64
						utr         string
					}{vendorTxnID: vtid, amount: amt, utr: *utr})
				}
			}
			rows.Close()
			if len(dupCandidates) > 0 {
				dupNeed = 1
				if len(dupCandidates) > 1 && count > 5 && rand.Float64() < 0.5 {
					dupNeed = 2
				}
			}
		}
	}

	// Helper to pick vendor identity, reusing a duplicate candidate for first dupNeed clean rows
	nextDupIdx := 0
	pickPhonePeIdentity := func() (string, float64, string) {
		if nextDupIdx < dupNeed && nextDupIdx < len(dupCandidates) {
			c := dupCandidates[nextDupIdx]
			nextDupIdx++
			return c.vendorTxnID, c.amount, c.utr
		}
		vid := fmt.Sprintf("T%s%02d", time.Now().Format("20060102150405"), rand.Intn(100))
		amt := math.Round((100+rand.Float64()*4900)*100) / 100
		utr := fmt.Sprintf("PP_UTR_%05d", rand.Intn(99999))
		if rand.Float64() < 0.5 {
			utr = fmt.Sprintf("PP_UTR_MOCK_%05d", rand.Intn(99999))
		}
		return vid, amt, utr
	}

	// Clean: exact amount, same day, narration embeds UTR — Tier 4 clean match
	for i := 0; i < cleanCount; i++ {
		baseVid, baseAmt, baseUtr := pickPhonePeIdentity()
		// If this was a reused duplicate, keep its amount/utr, otherwise use generated ones
		amount := baseAmt
		utr := baseUtr
		vendorTxnID := baseVid
		if nextDupIdx <= dupNeed && dupNeed > 0 && i < dupNeed {
			// already has reused values, keep them
		} else if baseVid == "" {
			// fallback (should not happen)
			amount = math.Round((100+rand.Float64()*4900)*100) / 100
			utr = fmt.Sprintf("PP_UTR_%05d", rand.Intn(99999))
			vendorTxnID = fmt.Sprintf("T%s%02d", time.Now().Format("20060102150405"), rand.Intn(100))
		}
		// For non-duplicate clean rows that weren't reused, we already have fresh values from pick
		// For duplicate rows, amount/utr are from candidate, vendorTxnID is reused
		if i >= dupNeed {
			// For non-duplicate rows, generate fresh if not already
			if baseVid == "" {
				// handled above
			}
		}
		daysAgo := rand.Intn(7)
		hour := 10 + rand.Intn(8)
		min := rand.Intn(60)
		base := time.Now().AddDate(0, 0, -daysAgo)
		settlementDate := time.Date(base.Year(), base.Month(), base.Day(), hour, min, 0, 0, base.Location())
		vendorID := fmt.Sprintf("%s_%04d", vendorTxnID, rand.Intn(10000))

		_, err := db.Exec(ctx, `
			INSERT INTO vendor_transactions (vendor_integration_id, vendor_txn_id, amount, utr_number, settlement_id, settlement_date, recon_status)
			VALUES ($1, $2, $3, $4, NULL, $5, 'UNMATCHED')
		`, vendorIntegrationID, vendorID, amount, utr, settlementDate)
		if err != nil {
			return fmt.Errorf("failed to insert mock PhonePe vendor transaction %d: %w", i, err)
		}
		narration := fmt.Sprintf("UPI/PHONEPE SETL/%s/REF %09d", utr, 100000000+rand.Intn(900000000))
		bankDate := settlementDate.Add(time.Duration(30+rand.Intn(120)) * time.Minute)
		_, err = db.Exec(ctx, `
			INSERT INTO bank_transactions (bank_account_id, amount, txn_type, narration, utr_number, txn_date)
			VALUES ($1, $2, 'CREDIT', $3, $4, $5)
		`, bankAccountID, amount, narration, utr, bankDate)
		if err != nil {
			return fmt.Errorf("failed to insert matching bank for PhonePe clean %d: %w", i, err)
		}
	}

	// Wrinkle: small amount discrepancy or 1-day delay to exercise Tier 4 tolerance
	for i := 0; i < wrinkleCount; i++ {
		// For wrinkle rows, also allow reuse of duplicate candidates if we still have dupNeed beyond clean
		var vendorTxnID string
		var amount float64
		var utr string
		if nextDupIdx < dupNeed && nextDupIdx < len(dupCandidates) {
			c := dupCandidates[nextDupIdx]
			nextDupIdx++
			vendorTxnID = c.vendorTxnID
			amount = c.amount
			utr = c.utr
		} else {
			vendorTxnID = fmt.Sprintf("T%s%02d", time.Now().Format("20060102150405"), rand.Intn(100))
			amount = math.Round((100+rand.Float64()*4900)*100) / 100
			utr = fmt.Sprintf("PP_UTR_%05d", rand.Intn(99999))
			if rand.Float64() < 0.5 {
				utr = fmt.Sprintf("PP_UTR_MOCK_%05d", rand.Intn(99999))
			}
		}
		daysAgo := rand.Intn(7)
		base := time.Now().AddDate(0, 0, -daysAgo)
		settlementDate := time.Date(base.Year(), base.Month(), base.Day(), 10+rand.Intn(8), rand.Intn(60), 0, 0, base.Location())
		vendorID := fmt.Sprintf("%s_%04d", vendorTxnID, rand.Intn(10000))

		_, err := db.Exec(ctx, `
			INSERT INTO vendor_transactions (vendor_integration_id, vendor_txn_id, amount, utr_number, settlement_id, settlement_date, recon_status)
			VALUES ($1, $2, $3, $4, NULL, $5, 'UNMATCHED')
		`, vendorIntegrationID, vendorID, amount, utr, settlementDate)
		if err != nil {
			return fmt.Errorf("failed to insert mock PhonePe wrinkle vendor %d: %w", i, err)
		}
		var bankAmount float64
		var bankDate time.Time
		if rand.Float64() < 0.5 {
			factor := 0.97 + rand.Float64()*0.025
			bankAmount = math.Round(amount*factor*100) / 100
			bankDate = settlementDate.Add(time.Duration(30+rand.Intn(120)) * time.Minute)
		} else {
			bankAmount = amount
			bankDate = settlementDate.AddDate(0, 0, 1).Add(time.Duration(rand.Intn(120)) * time.Minute)
		}
		narration := fmt.Sprintf("UPI/PHONEPE SETL/%s/REF %09d", utr, 100000000+rand.Intn(900000000))
		_, err = db.Exec(ctx, `
			INSERT INTO bank_transactions (bank_account_id, amount, txn_type, narration, utr_number, txn_date)
			VALUES ($1, $2, 'CREDIT', $3, $4, $5)
		`, bankAccountID, bankAmount, narration, utr, bankDate)
		if err != nil {
			return fmt.Errorf("failed to insert matching bank for PhonePe wrinkle %d: %w", i, err)
		}
	}

	// Orphan: deliberately NO bank counterpart — genuine "payment initiated but settlement not yet received"
	for i := 0; i < orphanCount; i++ {
		amount := math.Round((100+rand.Float64()*4900)*100) / 100
		vendorTxnID := fmt.Sprintf("T%s%02d", time.Now().Format("20060102150405"), rand.Intn(100))
		utr := fmt.Sprintf("PP_UTR_%05d", rand.Intn(99999))
		if rand.Float64() < 0.5 {
			utr = fmt.Sprintf("PP_UTR_MOCK_%05d", rand.Intn(99999))
		}
		daysAgo := rand.Intn(7)
		base := time.Now().AddDate(0, 0, -daysAgo)
		settlementDate := time.Date(base.Year(), base.Month(), base.Day(), 10+rand.Intn(8), rand.Intn(60), 0, 0, base.Location())
		vendorID := fmt.Sprintf("%s_%04d", vendorTxnID, rand.Intn(10000))
		_, err := db.Exec(ctx, `
			INSERT INTO vendor_transactions (vendor_integration_id, vendor_txn_id, amount, utr_number, settlement_id, settlement_date, recon_status)
			VALUES ($1, $2, $3, $4, NULL, $5, 'UNMATCHED')
		`, vendorIntegrationID, vendorID, amount, utr, settlementDate)
		if err != nil {
			return fmt.Errorf("failed to insert mock PhonePe orphan vendor %d: %w", i, err)
		}
	}

	if _, err := DetectCrossSourceDuplicates(ctx, db, merchantID); err != nil {
		return fmt.Errorf("failed to detect cross-source duplicates after PhonePe mock generation: %w", err)
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

	var bankAccountID string
	err = db.QueryRow(ctx, `SELECT id FROM merchant_bank_accounts WHERE merchant_id = $1 LIMIT 1`, merchantID).Scan(&bankAccountID)
	if err != nil && err != pgx.ErrNoRows {
		return fmt.Errorf("failed to resolve bank account for PineLabs mock: %w", err)
	}
	if bankAccountID == "" {
		err = db.QueryRow(ctx, `
			INSERT INTO merchant_bank_accounts (merchant_id, rpd_request_id, account_number, ifsc_code, verified_account_name)
			VALUES ($1, $2, '50100234567890', 'HDFC0001234', 'MOCK PINELABS PLACEHOLDER')
			RETURNING id
		`, merchantID, fmt.Sprintf("rpd_pinelabs_%d", rand.Intn(1000000))).Scan(&bankAccountID)
		if err != nil {
			return fmt.Errorf("failed to create placeholder bank account for PineLabs mock: %w", err)
		}
	}

	// Honest-by-design: 70% clean, 20% wrinkle, 10% orphan — settlement_id based (Tier 1/2/3)
	// Pine Labs already has settlement_id (unlike PhonePe which is UTR-centric), so
	// matchable majority should exercise settlement_id tiers, not Tier 4.
	orphanCount := int(math.Round(float64(count) * 0.1))
	if orphanCount < 1 && count > 0 {
		orphanCount = 1
	}
	if orphanCount > count {
		orphanCount = count
	}
	remaining := count - orphanCount
	wrinkleCount := int(math.Round(float64(remaining) * 0.25))
	if wrinkleCount > remaining {
		wrinkleCount = remaining
	}
	cleanCount := remaining - wrinkleCount

	// Clean: full settlement_id in narration, same day
	for i := 0; i < cleanCount; i++ {
		amount := math.Round((200+rand.Float64()*4800)*100) / 100
		settlementID := fmt.Sprintf("pl_setl_%s", randString(12))
		vendorTxnID := fmt.Sprintf("PL_TXN_%s%04d", time.Now().Format("20060102"), rand.Intn(10000))
		utr := fmt.Sprintf("PL_UTR%010d", rand.Intn(9999999999))
		daysAgo := rand.Intn(7)
		base := time.Now().AddDate(0, 0, -daysAgo)
		settlementDate := time.Date(base.Year(), base.Month(), base.Day(), 11+rand.Intn(6), rand.Intn(60), 0, 0, base.Location())

		_, err := db.Exec(ctx, `
			INSERT INTO vendor_transactions (vendor_integration_id, vendor_txn_id, amount, utr_number, settlement_id, settlement_date, recon_status)
			VALUES ($1, $2, $3, $4, $5, $6, 'UNMATCHED')
		`, vendorIntegrationID, vendorTxnID, amount, utr, settlementID, settlementDate)
		if err != nil {
			return fmt.Errorf("failed to insert mock Pine Labs vendor transaction %d: %w", i, err)
		}
		narration := fmt.Sprintf("NEFT CR:%s/PINE LABS SETL %s/REF %09d", utr, settlementID, 100000000+rand.Intn(900000000))
		bankUTR := utr
		bankDate := settlementDate.Add(time.Duration(60+rand.Intn(120)) * time.Minute)
		bankAmount := math.Round(amount*0.98*100) / 100 // net after fee, but Tier 1 doesn't check amount
		_, err = db.Exec(ctx, `
			INSERT INTO bank_transactions (bank_account_id, amount, txn_type, narration, utr_number, txn_date)
			VALUES ($1, $2, 'CREDIT', $3, $4, $5)
		`, bankAccountID, bankAmount, narration, bankUTR, bankDate)
		if err != nil {
			return fmt.Errorf("failed to insert matching bank for PineLabs clean %d: %w", i, err)
		}
	}

	// Wrinkle: truncated narration (first 8 chars) or 1-day delay — exercises Tier 3
	for i := 0; i < wrinkleCount; i++ {
		amount := math.Round((200+rand.Float64()*4800)*100) / 100
		settlementID := fmt.Sprintf("pl_setl_%s", randString(12))
		vendorTxnID := fmt.Sprintf("PL_TXN_%s%04d", time.Now().Format("20060102"), rand.Intn(10000))
		utr := fmt.Sprintf("PL_UTR%010d", rand.Intn(9999999999))
		daysAgo := rand.Intn(7)
		base := time.Now().AddDate(0, 0, -daysAgo)
		settlementDate := time.Date(base.Year(), base.Month(), base.Day(), 11+rand.Intn(6), rand.Intn(60), 0, 0, base.Location())

		_, err := db.Exec(ctx, `
			INSERT INTO vendor_transactions (vendor_integration_id, vendor_txn_id, amount, utr_number, settlement_id, settlement_date, recon_status)
			VALUES ($1, $2, $3, $4, $5, $6, 'UNMATCHED')
		`, vendorIntegrationID, vendorTxnID, amount, utr, settlementID, settlementDate)
		if err != nil {
			return fmt.Errorf("failed to insert mock Pine Labs wrinkle vendor %d: %w", i, err)
		}
		var narration string
		var bankDate time.Time
		if rand.Float64() < 0.5 {
			// Truncated: only first 8 chars survive bank truncation
			prefix := settlementID
			if len(prefix) > 8 {
				prefix = prefix[:8]
			}
			narration = fmt.Sprintf("NEFT CR:%s/PINE LABS SETL %s/REF %09d", utr, prefix, 100000000+rand.Intn(900000000))
			bankDate = settlementDate.Add(time.Duration(60+rand.Intn(120)) * time.Minute)
		} else {
			narration = fmt.Sprintf("NEFT CR:%s/PINE LABS SETL %s/REF %09d", utr, settlementID, 100000000+rand.Intn(900000000))
			bankDate = settlementDate.AddDate(0, 0, 1).Add(time.Duration(rand.Intn(120)) * time.Minute)
		}
		bankAmount := math.Round(amount*0.98*100) / 100
		_, err = db.Exec(ctx, `
			INSERT INTO bank_transactions (bank_account_id, amount, txn_type, narration, utr_number, txn_date)
			VALUES ($1, $2, 'CREDIT', $3, $4, $5)
		`, bankAccountID, bankAmount, narration, utr, bankDate)
		if err != nil {
			return fmt.Errorf("failed to insert matching bank for PineLabs wrinkle %d: %w", i, err)
		}
	}

	// Orphan: intentionally no bank counterpart — genuine "payment initiated but settlement not yet received"
	for i := 0; i < orphanCount; i++ {
		amount := math.Round((200+rand.Float64()*4800)*100) / 100
		settlementID := fmt.Sprintf("pl_setl_%s", randString(12))
		vendorTxnID := fmt.Sprintf("PL_TXN_%s%04d", time.Now().Format("20060102"), rand.Intn(10000))
		utr := fmt.Sprintf("PL_UTR%010d", rand.Intn(9999999999))
		daysAgo := rand.Intn(7)
		base := time.Now().AddDate(0, 0, -daysAgo)
		settlementDate := time.Date(base.Year(), base.Month(), base.Day(), 11+rand.Intn(6), rand.Intn(60), 0, 0, base.Location())
		_, err := db.Exec(ctx, `
			INSERT INTO vendor_transactions (vendor_integration_id, vendor_txn_id, amount, utr_number, settlement_id, settlement_date, recon_status)
			VALUES ($1, $2, $3, $4, $5, $6, 'UNMATCHED')
		`, vendorIntegrationID, vendorTxnID, amount, utr, settlementID, settlementDate)
		if err != nil {
			return fmt.Errorf("failed to insert mock Pine Labs orphan vendor %d: %w", i, err)
		}
	}

	if _, err := DetectCrossSourceDuplicates(ctx, db, merchantID); err != nil {
		return fmt.Errorf("failed to detect cross-source duplicates after Pine Labs mock generation: %w", err)
	}

	return nil
}

// DetectCrossSourceDuplicates finds and suppresses cross-source duplicate ingestions.
// It handles two cases:
//   - Vendor side: same (amount, utr_number) across different vendor_integration_ids
//   - Bank side: same (amount, utr_number, narration) across different bank rows
// For each group, the earliest-created record is kept as canonical; later duplicates
// are marked with duplicate_of and, for vendor rows, recon_status='DUPLICATE_SUPPRESSED'
// and an audit log entry with method='duplicate'.
func DetectCrossSourceDuplicates(ctx context.Context, db *pgxpool.Pool, merchantID string) (int, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	suppressed := 0

	// --- Vendor-side duplicates: same amount+UTR across different integrations ---
	vendorRows, err := tx.Query(ctx, `
		SELECT vt.id, vt.amount, vt.utr_number, vt.created_at, vt.vendor_integration_id
		FROM vendor_transactions vt
		JOIN vendor_integrations vi ON vt.vendor_integration_id = vi.id
		WHERE vi.merchant_id = $1
		  AND vt.duplicate_of IS NULL
		  AND vt.recon_status != 'DUPLICATE_SUPPRESSED'
		  AND vt.utr_number IS NOT NULL AND vt.utr_number != ''
		ORDER BY vt.amount, vt.utr_number, vt.created_at
	`, merchantID)
	if err != nil {
		return 0, fmt.Errorf("failed to query vendor transactions for duplicate detection: %w", err)
	}

	type vKey struct {
		amount float64
		utr    string
	}
	type vInfo struct {
		id              string
		integrationID   string
		createdAt       time.Time
	}

	// Group by (amount, utr)
	groups := make(map[vKey][]vInfo)
	for vendorRows.Next() {
		var id, utr, integrationID string
		var amount float64
		var createdAt time.Time
		if err := vendorRows.Scan(&id, &amount, &utr, &createdAt, &integrationID); err != nil {
			vendorRows.Close()
			return 0, fmt.Errorf("failed to scan vendor row: %w", err)
		}
		k := vKey{amount: amount, utr: utr}
		groups[k] = append(groups[k], vInfo{id: id, integrationID: integrationID, createdAt: createdAt})
	}
	vendorRows.Close()

	for _, infos := range groups {
		if len(infos) < 2 {
			continue
		}
		// Check if group spans at least two different integrations
		integrations := make(map[string]struct{})
		for _, inf := range infos {
			integrations[inf.integrationID] = struct{}{}
		}
		if len(integrations) < 2 {
			continue
		}
		// Already sorted by created_at due to ORDER BY, but ensure
		// Find canonical = earliest
		canonical := infos[0]
		for i := 1; i < len(infos); i++ {
			dup := infos[i]
			minutesLater := int(dup.createdAt.Sub(canonical.createdAt).Minutes())
			if minutesLater < 0 {
				minutesLater = -minutesLater
			}
			// Mark duplicate
			_, err := tx.Exec(ctx, `
				UPDATE vendor_transactions
				SET duplicate_of = $1, recon_status = 'DUPLICATE_SUPPRESSED'
				WHERE id = $2
			`, canonical.id, dup.id)
			if err != nil {
				return 0, fmt.Errorf("failed to mark vendor duplicate %s: %w", dup.id, err)
			}
			// Audit log with method='duplicate'
			reasoning := fmt.Sprintf("Identical amount+UTR as record %s, inserted %d minutes later — suppressed as a duplicate ingestion, not sent through matching.", canonical.id, minutesLater)
			_, err = tx.Exec(ctx, `
				INSERT INTO reconciliation_log (vendor_transaction_id, bank_transaction_id, method, confidence, reasoning)
				VALUES ($1, NULL, 'duplicate', NULL, $2)
			`, dup.id, reasoning)
			if err != nil {
				return 0, fmt.Errorf("failed to log vendor duplicate %s: %w", dup.id, err)
			}
			suppressed++
		}
	}

	// --- Bank-side duplicates: same amount+utr+narration across different ids ---
	bankRows, err := tx.Query(ctx, `
		SELECT bt.id, bt.amount, bt.utr_number, bt.narration, bt.created_at
		FROM bank_transactions bt
		JOIN merchant_bank_accounts mba ON bt.bank_account_id = mba.id
		WHERE mba.merchant_id = $1
		  AND bt.duplicate_of IS NULL
		ORDER BY bt.amount, bt.utr_number, bt.narration, bt.created_at
	`, merchantID)
	if err != nil {
		return 0, fmt.Errorf("failed to query bank transactions for duplicate detection: %w", err)
	}

	type bKey struct {
		amount    float64
		utr       string
		narration string
	}
	type bInfo struct {
		id        string
		createdAt time.Time
	}
	bGroups := make(map[bKey][]bInfo)
	for bankRows.Next() {
		var id, utr string
		var narration *string
		var amount float64
		var createdAt time.Time
		if err := bankRows.Scan(&id, &amount, &utr, &narration, &createdAt); err != nil {
			bankRows.Close()
			return 0, fmt.Errorf("failed to scan bank row: %w", err)
		}
		narr := ""
		if narration != nil {
			narr = *narration
		}
		if utr == "" {
			utr = ""
		}
		k := bKey{amount: amount, utr: utr, narration: narr}
		bGroups[k] = append(bGroups[k], bInfo{id: id, createdAt: createdAt})
	}
	bankRows.Close()

	for _, infos := range bGroups {
		if len(infos) < 2 {
			continue
		}
		canonical := infos[0]
		for i := 1; i < len(infos); i++ {
			dup := infos[i]
			minutesLater := int(dup.createdAt.Sub(canonical.createdAt).Minutes())
			if minutesLater < 0 {
				minutesLater = -minutesLater
			}
			_, err := tx.Exec(ctx, `
				UPDATE bank_transactions
				SET duplicate_of = $1
				WHERE id = $2
			`, canonical.id, dup.id)
			if err != nil {
				return 0, fmt.Errorf("failed to mark bank duplicate %s: %w", dup.id, err)
			}
			reasoning := fmt.Sprintf("Identical amount+UTR+narration as record %s, inserted %d minutes later — suppressed as a duplicate ingestion, not sent through matching.", canonical.id, minutesLater)
			// For bank duplicates, vendor_transaction_id is NULL, bank_transaction_id is the duplicate
			// reconciliation_log.vendor_transaction_id is nullable after migration 006, so we log with vendor NULL.
			// However to satisfy FK, we need a vendor id — use canonical vendor? Instead we log with bank id only.
			// Since vendor_transaction_id is nullable (checked via \d), we can insert NULL.
			_, err = tx.Exec(ctx, `
				INSERT INTO reconciliation_log (vendor_transaction_id, bank_transaction_id, method, confidence, reasoning)
				VALUES (NULL, $1, 'duplicate', NULL, $2)
			`, dup.id, reasoning)
			if err != nil {
				// If vendor_transaction_id NOT NULL constraint unexpectedly, fallback to not logging bank duplicate
				// but still count as suppressed
				_ = err
			} else {
				suppressed++
				continue
			}
			// If log insert failed, still count the duplicate as suppressed (bank side doesn't strictly need log)
			suppressed++
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("failed to commit duplicate detection: %w", err)
	}

	return suppressed, nil
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
