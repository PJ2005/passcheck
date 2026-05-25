package demo

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PendingTransaction represents a single unmatched record in the dashboard
type PendingTransaction struct {
	VendorTxnID string  `json:"vendor_txn_id"`
	Amount      float64 `json:"amount"`
	UTRNumber   string  `json:"utr_number"`
	Date        string  `json:"date"`
}

// GetReconciliationDashboard returns the metrics and pending list for the demo showcase
func GetReconciliationDashboard(db *pgxpool.Pool) fiber.Handler {
	return func(c *fiber.Ctx) error {
		merchantID := c.Params("merchantId")
		if merchantID == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "merchantId parameter is required"})
		}

		ctx := context.Background()

		// 1. Calculate Aggregates (All-time for demo purposes)
		var totalExpected, totalSettled, totalPending float64
		
		err := db.QueryRow(ctx, `
			SELECT 
				COALESCE(SUM(vt.amount), 0) as total_expected,
				COALESCE(SUM(CASE WHEN vt.recon_status = 'MATCHED' THEN vt.amount ELSE 0 END), 0) as total_settled,
				COALESCE(SUM(CASE WHEN vt.recon_status = 'UNMATCHED' THEN vt.amount ELSE 0 END), 0) as total_pending
			FROM vendor_transactions vt
			JOIN vendor_integrations vi ON vt.vendor_integration_id = vi.id
			WHERE vi.merchant_id = $1
		`, merchantID).Scan(&totalExpected, &totalSettled, &totalPending)

		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to aggregate metrics", "details": err.Error()})
		}

		// 2. Fetch the specific pending transactions
		rows, err := db.Query(ctx, `
			SELECT vt.vendor_txn_id, vt.amount, vt.utr_number, vt.settlement_date
			FROM vendor_transactions vt
			JOIN vendor_integrations vi ON vt.vendor_integration_id = vi.id
			WHERE vi.merchant_id = $1 AND vt.recon_status = 'UNMATCHED'
			ORDER BY vt.settlement_date DESC
		`, merchantID)
		
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to fetch pending transactions", "details": err.Error()})
		}
		defer rows.Close()

		var pendingTxns []PendingTransaction
		for rows.Next() {
			var txn PendingTransaction
			var t time.Time
			if err := rows.Scan(&txn.VendorTxnID, &txn.Amount, &txn.UTRNumber, &t); err == nil {
				txn.Date = t.Format("2006-01-02")
				pendingTxns = append(pendingTxns, txn)
			}
		}

		// Prevent returning nil slice (which encodes to JSON null instead of [])
		if pendingTxns == nil {
			pendingTxns = []PendingTransaction{}
		}

		// 3. Return JSON payload
		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"merchant_id":           merchantID,
			"total_expected_funds":  totalExpected,
			"total_settled_funds":   totalSettled,
			"total_pending_funds":   totalPending,
			"pending_transactions":  pendingTxns,
		})
	}
}
