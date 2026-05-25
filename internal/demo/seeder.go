package demo

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"time"

	"passcheck/internal/vendors"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SeedMockRazorpayData grabs 10 real bank transaction UTRs and creates matching dummy vendor txns,
// plus 3 fake pending txns, so we can showcase the reconciliation engine.
func SeedMockRazorpayData(ctx context.Context, db *pgxpool.Pool, merchantID string, vendorIntegrationID string) error {
	log.Printf("[DEMO] Starting Mock Razorpay Data Seeder for Merchant %s", merchantID)

	// Step 1: Query 10 random credit bank transactions for this merchant's bank accounts
	query := `
		SELECT bt.utr_number, bt.amount, bt.txn_date
		FROM bank_transactions bt
		JOIN merchant_bank_accounts mba ON bt.bank_account_id = mba.id
		LEFT JOIN reconciled_matches rm ON rm.bank_transaction_id = bt.id
		WHERE mba.merchant_id = $1 AND bt.txn_type = 'CREDIT' AND rm.id IS NULL
		ORDER BY RANDOM()
		LIMIT 10
	`
	rows, err := db.Query(ctx, query, merchantID)
	if err != nil {
		return fmt.Errorf("failed to fetch bank transactions: %w", err)
	}
	defer rows.Close()

	var vendorTxns []vendors.StandardVendorTxn

	// Step 2: Create 10 Matching records
	for rows.Next() {
		var utr string
		var amount float64
		var txnDate time.Time

		if err := rows.Scan(&utr, &amount, &txnDate); err != nil {
			return fmt.Errorf("failed to scan row: %w", err)
		}

		vendorTxns = append(vendorTxns, vendors.StandardVendorTxn{
			VendorTxnID:    fmt.Sprintf("pay_demo_%s", utr[len(utr)-6:]),
			Amount:         amount,
			UTRNumber:      utr,
			SettlementDate: txnDate,
			VendorName:     "Razorpay",
		})
	}

	if len(vendorTxns) == 0 {
		return fmt.Errorf("no CREDIT bank transactions found for merchant. Have you run the Setu bank sync yet?")
	}

	// Step 3: Create 3 Pending records
	for i := 1; i <= 3; i++ {
		amount := rand.Float64() * 5000.0
		// round to 2 decimal places
		amount = float64(int(amount*100)) / 100

		vendorTxns = append(vendorTxns, vendors.StandardVendorTxn{
			VendorTxnID:    fmt.Sprintf("pay_pending_%d", rand.Intn(99999)),
			Amount:         amount,
			UTRNumber:      fmt.Sprintf("PENDING_UTR_%d", rand.Intn(999999)),
			SettlementDate: time.Now(),
			VendorName:     "Razorpay",
		})
	}

	// Step 4: Batch insert all 13 into the vendor_transactions table
	insertedCount := 0
	for _, txn := range vendorTxns {
		_, err := db.Exec(ctx, `
			INSERT INTO vendor_transactions (vendor_integration_id, vendor_txn_id, amount, utr_number, settlement_date, recon_status)
			VALUES ($1, $2, $3, $4, $5, 'UNMATCHED')
			ON CONFLICT (vendor_integration_id, vendor_txn_id) DO NOTHING
		`, vendorIntegrationID, txn.VendorTxnID, txn.Amount, txn.UTRNumber, txn.SettlementDate)

		if err != nil {
			log.Printf("[DEMO] Failed to insert mock vendor txn %s: %v", txn.VendorTxnID, err)
			continue
		}
		insertedCount++
	}

	log.Printf("[DEMO] Successfully seeded %d mock vendor transactions!", insertedCount)
	return nil
}
