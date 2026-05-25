package reconciliation

import (
	"context"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RunDailyReconciliation performs exact matching between vendor transactions and bank transactions
func RunDailyReconciliation(merchantID string, db *pgxpool.Pool) (int, error) {
	log.Printf("Starting reconciliation engine for merchant: %s", merchantID)
	
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// Get all unmatched vendor transactions for this merchant
	rows, err := tx.Query(ctx, `
		SELECT vt.id, vt.utr_number, vt.amount 
		FROM vendor_transactions vt
		JOIN vendor_integrations vi ON vt.vendor_integration_id = vi.id
		WHERE vi.merchant_id = $1 AND vt.recon_status = 'UNMATCHED' AND vt.utr_number IS NOT NULL
	`, merchantID)
	
	if err != nil {
		return 0, fmt.Errorf("failed to query vendor transactions: %w", err)
	}
	defer rows.Close()

	type VendorTxn struct {
		ID        string
		UTRNumber string
		Amount    float64
	}

	var unmatchedTxns []VendorTxn
	for rows.Next() {
		var txn VendorTxn
		if err := rows.Scan(&txn.ID, &txn.UTRNumber, &txn.Amount); err != nil {
			log.Printf("Failed to scan vendor transaction row: %v", err)
			continue
		}
		unmatchedTxns = append(unmatchedTxns, txn)
	}
	rows.Close() // Explicit close since we reuse tx

	matchedCount := 0
	for _, vTxn := range unmatchedTxns {
		var bankTxnID string
		// Find a matching bank transaction by UTR and exact amount
		err := tx.QueryRow(ctx, `
			SELECT bt.id FROM bank_transactions bt
			LEFT JOIN reconciled_matches rm ON rm.bank_transaction_id = bt.id
			WHERE bt.utr_number = $1 AND bt.amount = $2 AND rm.id IS NULL
			LIMIT 1
		`, vTxn.UTRNumber, vTxn.Amount).Scan(&bankTxnID)

		if err != nil {
			// No match found or error
			continue
		}

		// Match found! Create the reconciled link
		_, err = tx.Exec(ctx, `
			INSERT INTO reconciled_matches (vendor_transaction_id, bank_transaction_id)
			VALUES ($1, $2)
		`, vTxn.ID, bankTxnID)
		if err != nil {
			log.Printf("Failed to insert reconciled match for vendor txn %s: %v", vTxn.ID, err)
			continue
		}

		// Update vendor transaction status to MATCHED
		_, err = tx.Exec(ctx, `
			UPDATE vendor_transactions 
			SET recon_status = 'MATCHED' 
			WHERE id = $1
		`, vTxn.ID)
		if err != nil {
			log.Printf("Failed to update recon_status for vendor txn %s: %v", vTxn.ID, err)
			continue
		}
		
		matchedCount++
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("failed to commit transaction: %w", err)
	}

	log.Printf("Reconciliation complete. matched %d transactions.", matchedCount)
	return matchedCount, nil
}
