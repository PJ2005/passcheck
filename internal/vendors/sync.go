package vendors

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SyncVendorData acts as the bridge between the external PaymentProvider and our database.
func SyncVendorData(ctx context.Context, db *pgxpool.Pool, provider PaymentProvider, merchantID string, date time.Time) error {
	// 1. We need the vendor integration ID to save the transactions.
	// We'll pass a dummy vendorName here just to get the interface working for Razorpay.
	// In reality, the sync engine would iterate all active integrations.
	creds, err := GetVendorCredentials(ctx, db, merchantID, "Razorpay")
	if err != nil {
		return fmt.Errorf("failed to get integration ID: %w", err)
	}

	// 2. Fetch the standardized data
	log.Printf("Fetching vendor transactions for merchant %s on %s...", merchantID, date.Format("2006-01-02"))
	txns, err := provider.FetchSettlements(ctx, merchantID, date)
	if err != nil {
		return fmt.Errorf("failed to fetch settlements from provider: %w", err)
	}

	if len(txns) == 0 {
		log.Printf("No vendor transactions found for this date.")
		return nil
	}

	// 3. Ingest into the database
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	insertedCount := 0
	for _, txn := range txns {
		// Use ON CONFLICT DO NOTHING to ensure idempotency.
		// If the same VendorTxnID + IntegrationID exists, we skip it.
		_, err := tx.Exec(ctx, `
			INSERT INTO vendor_transactions (
				vendor_integration_id, 
				vendor_txn_id, 
				amount, 
				utr_number, 
				settlement_date, 
				recon_status
			) VALUES ($1, $2, $3, $4, $5, 'UNMATCHED')
			ON CONFLICT (vendor_integration_id, vendor_txn_id) DO NOTHING
		`, creds.IntegrationID, txn.VendorTxnID, txn.Amount, txn.UTRNumber, txn.SettlementDate)

		if err != nil {
			log.Printf("Failed to insert vendor transaction %s: %v", txn.VendorTxnID, err)
			continue
		}
		insertedCount++
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit vendor sync transaction: %w", err)
	}

	log.Printf("Successfully synced %d vendor transactions into the ledger.", insertedCount)
	return nil
}
