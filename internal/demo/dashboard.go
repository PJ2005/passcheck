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

// ExceptionEntry is an UNMATCHED vendor transaction paired with the audit
// reasoning the engine recorded when it failed to resolve it.
type ExceptionEntry struct {
	VendorTxnID  string   `json:"vendor_txn_id"`
	Amount       float64  `json:"amount"`
	SettlementID *string  `json:"settlement_id"`
	Reasoning    string   `json:"reasoning"`
}

// MatchEntry explains WHY a recent match succeeded - tier method, confidence,
// and the engine's own reasoning - so decisions are transparent to judges
// and reviewers instead of being an opaque status flip.
type MatchEntry struct {
	VendorTxnID string  `json:"vendor_txn_id"`
	Amount      float64 `json:"amount"`
	Method      string  `json:"method"`
	Confidence  float64 `json:"confidence"`
	Reasoning   string  `json:"reasoning"`
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

		// 3. Exception count: vendor rows the engine could not resolve
		var exceptionCount int
		err = db.QueryRow(ctx, `
			SELECT COUNT(*)
			FROM vendor_transactions vt
			JOIN vendor_integrations vi ON vt.vendor_integration_id = vi.id
			WHERE vi.merchant_id = $1 AND vt.recon_status = 'UNMATCHED'
		`, merchantID).Scan(&exceptionCount)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to count exceptions", "details": err.Error()})
		}

		// 4. Exceptions with reasoning: each unresolved row paired with its most
		// recent 'unresolved' audit entry, capped at 20 to bound the response.
		exceptions := []ExceptionEntry{}
		exRows, err := db.Query(ctx, `
			SELECT vt.vendor_txn_id, vt.amount, vt.settlement_id, COALESCE(rl.reasoning, '')
			FROM vendor_transactions vt
			JOIN vendor_integrations vi ON vt.vendor_integration_id = vi.id
			CROSS JOIN LATERAL (
				SELECT r.reasoning, r.created_at
				FROM reconciliation_log r
				WHERE r.vendor_transaction_id = vt.id AND r.method = 'unresolved'
				ORDER BY r.created_at DESC
				LIMIT 1
			) rl
			WHERE vi.merchant_id = $1 AND vt.recon_status = 'UNMATCHED'
			ORDER BY rl.created_at DESC
			LIMIT 20
		`, merchantID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to fetch exceptions", "details": err.Error()})
		}
		defer exRows.Close()
		for exRows.Next() {
			var e ExceptionEntry
			if err := exRows.Scan(&e.VendorTxnID, &e.Amount, &e.SettlementID, &e.Reasoning); err == nil {
				exceptions = append(exceptions, e)
			}
		}

		// 5. Recent matches with decision transparency: last 10 resolved rows,
		// showing which tier matched and at what confidence.
		recentMatches := []MatchEntry{}
		rmRows, err := db.Query(ctx, `
			SELECT vt.vendor_txn_id, vt.amount, rl.method, rl.confidence, COALESCE(rl.reasoning, '')
			FROM vendor_transactions vt
			JOIN vendor_integrations vi ON vt.vendor_integration_id = vi.id
			CROSS JOIN LATERAL (
				SELECT r.method, r.confidence, r.reasoning, r.created_at
				FROM reconciliation_log r
				WHERE r.vendor_transaction_id = vt.id AND r.method = 'deterministic'
				ORDER BY r.created_at DESC
				LIMIT 1
			) rl
			WHERE vi.merchant_id = $1 AND vt.recon_status = 'MATCHED'
			ORDER BY rl.created_at DESC
			LIMIT 10
		`, merchantID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to fetch recent matches", "details": err.Error()})
		}
		defer rmRows.Close()
		for rmRows.Next() {
			var m MatchEntry
			if err := rmRows.Scan(&m.VendorTxnID, &m.Amount, &m.Method, &m.Confidence, &m.Reasoning); err == nil {
				recentMatches = append(recentMatches, m)
			}
		}

		// 6. Return JSON payload
		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"merchant_id":           merchantID,
			"total_expected_funds":  totalExpected,
			"total_settled_funds":   totalSettled,
			"total_pending_funds":   totalPending,
			"pending_transactions":  pendingTxns,
			"exception_count":       exceptionCount,
			"exceptions":            exceptions,
			"recent_matches":        recentMatches,
		})
	}
}
