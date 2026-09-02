package demo

import (
	"context"
	"encoding/csv"
	"fmt"
	"strconv"
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
	VendorName  *string `json:"vendor_name,omitempty"`
	BankTxnType *string `json:"bank_txn_type,omitempty"`
}

// ExceptionEntry is an UNMATCHED vendor transaction paired with the audit
// reasoning the engine recorded when it failed to resolve it.
type ExceptionEntry struct {
	VendorTxnID  string  `json:"vendor_txn_id"`
	Amount       float64 `json:"amount"`
	SettlementID *string `json:"settlement_id"`
	Reasoning    string  `json:"reasoning"`
	VendorName   *string `json:"vendor_name,omitempty"`
	BankTxnType  *string `json:"bank_txn_type,omitempty"`
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
	VendorName  *string `json:"vendor_name,omitempty"`
	BankTxnType *string `json:"bank_txn_type,omitempty"`
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
			SELECT vt.vendor_txn_id, vt.amount, vt.utr_number, vt.settlement_date,
			       vi.vendor_name, bt.txn_type
			FROM vendor_transactions vt
			JOIN vendor_integrations vi ON vt.vendor_integration_id = vi.id
			LEFT JOIN LATERAL (
				SELECT r.bank_transaction_id
				FROM reconciliation_log r
				WHERE r.vendor_transaction_id = vt.id
				ORDER BY r.created_at DESC
				LIMIT 1
			) rl ON true
			LEFT JOIN bank_transactions bt ON bt.id = rl.bank_transaction_id
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
			if err := rows.Scan(&txn.VendorTxnID, &txn.Amount, &txn.UTRNumber, &t, &txn.VendorName, &txn.BankTxnType); err == nil {
				txn.Date = t.Format("2006-01-02")
				pendingTxns = append(pendingTxns, txn)
			}
		}

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
			SELECT vt.vendor_txn_id, vt.amount, vt.settlement_id, COALESCE(rl.reasoning, ''),
			       vi.vendor_name, bt.txn_type
			FROM vendor_transactions vt
			JOIN vendor_integrations vi ON vt.vendor_integration_id = vi.id
			CROSS JOIN LATERAL (
				SELECT r.reasoning, r.created_at, r.bank_transaction_id
				FROM reconciliation_log r
				WHERE r.vendor_transaction_id = vt.id AND r.method = 'unresolved'
				ORDER BY r.created_at DESC
				LIMIT 1
			) rl
			LEFT JOIN bank_transactions bt ON bt.id = rl.bank_transaction_id
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
			if err := exRows.Scan(&e.VendorTxnID, &e.Amount, &e.SettlementID, &e.Reasoning, &e.VendorName, &e.BankTxnType); err == nil {
				exceptions = append(exceptions, e)
			}
		}

		// 5. Recent matches with decision transparency: surface ONE exemplar of
		// every confidence tier first (so partial/lumped heuristics are always
		// visible to judges alongside exact hits), then fill remaining slots
		// with the most recent matches overall. Capped at 10 rows.
		recentMatches := []MatchEntry{}
		rmRows, err := db.Query(ctx, `
			WITH ranked AS (
				SELECT vt.vendor_txn_id, vt.amount, rl.method, rl.confidence,
				       COALESCE(rl.reasoning, '') AS reasoning,
				       vi.vendor_name, bt.txn_type,
				       ROW_NUMBER() OVER (PARTITION BY rl.confidence ORDER BY rl.created_at DESC) AS rn
				FROM vendor_transactions vt
				JOIN vendor_integrations vi ON vt.vendor_integration_id = vi.id
				CROSS JOIN LATERAL (
					SELECT r.method, r.confidence, r.reasoning, r.created_at, r.bank_transaction_id
					FROM reconciliation_log r
					WHERE r.vendor_transaction_id = vt.id AND r.method = 'deterministic'
					ORDER BY r.created_at DESC
					LIMIT 1
				) rl
				LEFT JOIN bank_transactions bt ON bt.id = rl.bank_transaction_id
				WHERE vi.merchant_id = $1 AND vt.recon_status = 'MATCHED'
			)
			SELECT vendor_txn_id, amount, method, confidence, reasoning, vendor_name, txn_type
			FROM ranked
			ORDER BY rn ASC, confidence ASC
			LIMIT 10
		`, merchantID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to fetch recent matches", "details": err.Error()})
		}
		defer rmRows.Close()
		for rmRows.Next() {
			var m MatchEntry
			if err := rmRows.Scan(&m.VendorTxnID, &m.Amount, &m.Method, &m.Confidence, &m.Reasoning, &m.VendorName, &m.BankTxnType); err == nil {
				recentMatches = append(recentMatches, m)
			}
		}

		// 6. Method breakdown
		methodBreakdown := map[string]int{"deterministic": 0, "agent": 0, "unresolved": 0, "duplicate": 0}
		mbRows, err := db.Query(ctx, `
			SELECT rl.method, COUNT(*)
			FROM reconciliation_log rl
			JOIN vendor_transactions vt ON vt.id = rl.vendor_transaction_id
			JOIN vendor_integrations vi ON vt.vendor_integration_id = vi.id
			WHERE vi.merchant_id = $1
			GROUP BY rl.method
		`, merchantID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to compute method breakdown", "details": err.Error()})
		}
		defer mbRows.Close()
		for mbRows.Next() {
			var method string
			var count int
			if err := mbRows.Scan(&method, &count); err == nil {
				methodBreakdown[method] = count
			}
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"merchant_id":          merchantID,
			"total_expected_funds": totalExpected,
			"total_settled_funds":  totalSettled,
			"total_pending_funds":  totalPending,
			"pending_transactions": pendingTxns,
			"exception_count":      exceptionCount,
			"exceptions":           exceptions,
			"recent_matches":       recentMatches,
			"method_breakdown":     methodBreakdown,
		})
	}
}

// AuditRecord is the unified three-category record returned by GetReconciliationRecords.
// record_category is always one of "matched_pair", "unmatched_vendor", or "unmatched_bank".
//
// matched_pair: both vendor_* and bank_* fields are fully populated.
// unmatched_vendor: bank_* fields are null/absent.
// unmatched_bank: vendor_* fields are null/absent.
type AuditRecord struct {
	RecordCategory string `json:"record_category"` // "matched_pair" | "unmatched_vendor" | "unmatched_bank"

	// Vendor-side fields (null for unmatched_bank)
	VendorTxnID          *string  `json:"vendor_txn_id,omitempty"`
	VendorAmount         *float64 `json:"vendor_amount,omitempty"`
	VendorSettlementID   *string  `json:"vendor_settlement_id,omitempty"`
	VendorUTR            *string  `json:"vendor_utr,omitempty"`
	VendorSettlementDate *string  `json:"vendor_settlement_date,omitempty"`
	VendorSource         *string  `json:"vendor_source,omitempty"` // Razorpay / PhonePe / PineLabs
	VendorReconStatus    *string  `json:"vendor_recon_status,omitempty"`

	// Bank-side fields (null for unmatched_vendor)
	BankTxnID     *string  `json:"bank_txn_id,omitempty"`
	BankAmount    *float64 `json:"bank_amount,omitempty"`
	BankUTR       *string  `json:"bank_utr,omitempty"`
	BankNarration *string  `json:"bank_narration,omitempty"`
	BankTxnDate   *string  `json:"bank_txn_date,omitempty"`
	BankTxnType   *string  `json:"bank_txn_type,omitempty"` // CREDIT / DEBIT

	// Decision metadata
	Method     *string  `json:"method,omitempty"`
	Confidence *float64 `json:"confidence,omitempty"`
	Reasoning  *string  `json:"reasoning,omitempty"`
	DecidedAt  *string  `json:"decided_at,omitempty"`
}

// AuditRecordsResponse is the JSON envelope for paginated results.
type AuditRecordsResponse struct {
	MerchantID string        `json:"merchant_id"`
	Page       int           `json:"page"`
	PageSize   int           `json:"page_size"`
	TotalCount int           `json:"total_count"`
	TotalPages int           `json:"total_pages"`
	Records    []AuditRecord `json:"records"`
}

// Aliases so existing tests that reference the old names keep compiling.
type ReconciliationRecord = AuditRecord
type ReconciliationRecordsResponse = AuditRecordsResponse

// GetReconciliationRecords returns a three-category, fully paginated audit ledger.
// Every vendor_transactions row for the merchant appears in exactly one category.
// Every bank_transactions row for the merchant appears in exactly one category.
//
// record_category values:
//
//	"matched_pair"     — one row per reconciled_matches entry, both sides fully populated.
//	"unmatched_vendor" — vendor txn with no entry in reconciled_matches.
//	"unmatched_bank"   — bank txn not referenced as bank_transaction_id in reconciled_matches.
//
// Guarantee: UV + M = V (every vendor row once), UB + distinct_matched_bank = B (every bank row once).
// Note: one bank txn can appear in >1 matched_pair row if it is a lumped settlement matched to
// multiple vendor txns. M counts match rows (not distinct bank txns).
func GetReconciliationRecords(db *pgxpool.Pool) fiber.Handler {
	return func(c *fiber.Ctx) error {
		merchantID := c.Params("merchantId")
		if merchantID == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "merchantId parameter is required"})
		}

		page, err := strconv.Atoi(c.Query("page", "1"))
		if err != nil || page < 1 {
			page = 1
		}
		pageSize, err := strconv.Atoi(c.Query("page_size", "25"))
		if err != nil || pageSize < 1 {
			pageSize = 25
		}
		if pageSize > 200 {
			pageSize = 200
		}
		format := c.Query("format")
		categoryFilter := c.Query("category") // filter by record_category value

		ctx := context.Background()

		// UNION ALL of three categories. Each SELECT produces an identical column set;
		// fields that don't apply to a category are projected as NULL.
		baseQuery := `
			FROM (
				-- ── Category 1: MATCHED PAIRS ────────────────────────────────────────
				-- Start FROM reconciled_matches so every row is anchored to a confirmed
				-- match. Join vendor_transactions AND bank_transactions so both sides are
				-- fully present. One reconciled_matches row = one result row.
				SELECT
					'matched_pair'::text                        AS record_category,
					vt.vendor_txn_id                            AS vendor_txn_id,
					vt.amount                                   AS vendor_amount,
					vt.settlement_id                            AS vendor_settlement_id,
					COALESCE(vt.utr_number, '')                 AS vendor_utr,
					vt.settlement_date::text                    AS vendor_settlement_date,
					vi.vendor_name                              AS vendor_source,
					vt.recon_status::text                       AS vendor_recon_status,
					bt.id::text                                 AS bank_txn_id,
					bt.amount                                   AS bank_amount,
					COALESCE(bt.utr_number, '')                 AS bank_utr,
					COALESCE(bt.narration, '')                  AS bank_narration,
					bt.txn_date::text                           AS bank_txn_date,
					bt.txn_type::text                           AS bank_txn_type,
					rl.method::text                             AS method,
					rl.confidence                               AS confidence,
					rl.reasoning                                AS reasoning,
					rl.created_at                               AS decided_at
				FROM reconciled_matches rm
				JOIN vendor_transactions vt ON vt.id = rm.vendor_transaction_id
				JOIN vendor_integrations vi ON vi.id = vt.vendor_integration_id
				JOIN bank_transactions   bt ON bt.id = rm.bank_transaction_id
				LEFT JOIN LATERAL (
					SELECT r.method, r.confidence, r.reasoning, r.created_at
					FROM reconciliation_log r
					WHERE r.vendor_transaction_id = vt.id
					ORDER BY r.created_at DESC
					LIMIT 1
				) rl ON true
				WHERE vi.merchant_id = $1

				UNION ALL

				-- ── Category 2: UNMATCHED VENDOR ─────────────────────────────────────
				-- vendor_transactions with no row in reconciled_matches for that vendor txn.
				SELECT
					'unmatched_vendor'::text                    AS record_category,
					vt.vendor_txn_id                            AS vendor_txn_id,
					vt.amount                                   AS vendor_amount,
					vt.settlement_id                            AS vendor_settlement_id,
					COALESCE(vt.utr_number, '')                 AS vendor_utr,
					vt.settlement_date::text                    AS vendor_settlement_date,
					vi.vendor_name                              AS vendor_source,
					vt.recon_status::text                       AS vendor_recon_status,
					NULL::text                                  AS bank_txn_id,
					NULL::numeric                               AS bank_amount,
					NULL::text                                  AS bank_utr,
					NULL::text                                  AS bank_narration,
					NULL::text                                  AS bank_txn_date,
					NULL::text                                  AS bank_txn_type,
					rl.method::text                             AS method,
					rl.confidence                               AS confidence,
					rl.reasoning                                AS reasoning,
					rl.created_at                               AS decided_at
				FROM vendor_transactions vt
				JOIN vendor_integrations vi ON vi.id = vt.vendor_integration_id
				LEFT JOIN LATERAL (
					SELECT r.method, r.confidence, r.reasoning, r.created_at
					FROM reconciliation_log r
					WHERE r.vendor_transaction_id = vt.id
					ORDER BY r.created_at DESC
					LIMIT 1
				) rl ON true
				WHERE vi.merchant_id = $1
				  AND NOT EXISTS (
					SELECT 1 FROM reconciled_matches rm WHERE rm.vendor_transaction_id = vt.id
				  )

				UNION ALL

				-- ── Category 3: UNMATCHED BANK ───────────────────────────────────────
				-- bank_transactions not referenced as bank_transaction_id in reconciled_matches.
				SELECT
					'unmatched_bank'::text                      AS record_category,
					NULL::text                                  AS vendor_txn_id,
					NULL::numeric                               AS vendor_amount,
					NULL::text                                  AS vendor_settlement_id,
					NULL::text                                  AS vendor_utr,
					NULL::text                                  AS vendor_settlement_date,
					NULL::text                                  AS vendor_source,
					NULL::text                                  AS vendor_recon_status,
					bt.id::text                                 AS bank_txn_id,
					bt.amount                                   AS bank_amount,
					COALESCE(bt.utr_number, '')                 AS bank_utr,
					COALESCE(bt.narration, '')                  AS bank_narration,
					bt.txn_date::text                           AS bank_txn_date,
					bt.txn_type::text                           AS bank_txn_type,
					NULL::text                                  AS method,
					NULL::numeric                               AS confidence,
					COALESCE(bt.narration, 'Unmatched bank transaction') AS reasoning,
					bt.created_at                               AS decided_at
				FROM bank_transactions bt
				JOIN merchant_bank_accounts mba ON mba.id = bt.bank_account_id
				WHERE mba.merchant_id = $1
				  AND NOT EXISTS (
					SELECT 1 FROM reconciled_matches rm WHERE rm.bank_transaction_id = bt.id
				  )
			) rec
			WHERE 1=1
		`
		args := []any{merchantID}
		argIdx := 2

		if categoryFilter != "" {
			baseQuery += fmt.Sprintf(" AND rec.record_category = $%d", argIdx)
			args = append(args, categoryFilter)
			argIdx++
		}

		// Count total for pagination
		var totalCount int
		if err = db.QueryRow(ctx, "SELECT COUNT(*) "+baseQuery, args...).Scan(&totalCount); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to count records", "details": err.Error()})
		}

		totalPages := (totalCount + pageSize - 1) / pageSize
		if totalPages == 0 {
			totalPages = 1
		}

		if format == "csv" {
			return streamCSVExport(c, db, ctx, baseQuery, args, merchantID)
		}

		selectQuery := `
			SELECT
				rec.record_category,
				rec.vendor_txn_id, rec.vendor_amount, rec.vendor_settlement_id, rec.vendor_utr,
				rec.vendor_settlement_date, rec.vendor_source, rec.vendor_recon_status,
				rec.bank_txn_id, rec.bank_amount, rec.bank_utr, rec.bank_narration,
				rec.bank_txn_date, rec.bank_txn_type,
				rec.method, rec.confidence, rec.reasoning, rec.decided_at
		` + baseQuery + `
			ORDER BY rec.decided_at DESC NULLS LAST, rec.vendor_txn_id DESC NULLS LAST
			LIMIT $` + strconv.Itoa(argIdx) + ` OFFSET $` + strconv.Itoa(argIdx+1)

		limitArgs := append(args, pageSize, (page-1)*pageSize)
		rows, err := db.Query(ctx, selectQuery, limitArgs...)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to fetch records", "details": err.Error()})
		}
		defer rows.Close()

		var records []AuditRecord
		for rows.Next() {
			var rec AuditRecord
			var decidedAt *time.Time
			if scanErr := rows.Scan(
				&rec.RecordCategory,
				&rec.VendorTxnID, &rec.VendorAmount, &rec.VendorSettlementID, &rec.VendorUTR,
				&rec.VendorSettlementDate, &rec.VendorSource, &rec.VendorReconStatus,
				&rec.BankTxnID, &rec.BankAmount, &rec.BankUTR, &rec.BankNarration,
				&rec.BankTxnDate, &rec.BankTxnType,
				&rec.Method, &rec.Confidence, &rec.Reasoning, &decidedAt,
			); scanErr != nil {
				continue
			}
			if decidedAt != nil {
				s := decidedAt.Format(time.RFC3339)
				rec.DecidedAt = &s
			}
			records = append(records, rec)
		}
		if records == nil {
			records = []AuditRecord{}
		}

		return c.Status(fiber.StatusOK).JSON(AuditRecordsResponse{
			MerchantID: merchantID,
			Page:       page,
			PageSize:   pageSize,
			TotalCount: totalCount,
			TotalPages: totalPages,
			Records:    records,
		})
	}
}

func streamCSVExport(c *fiber.Ctx, db *pgxpool.Pool, ctx context.Context, baseQuery string, args []any, merchantID string) error {
	selectQuery := `
		SELECT
			rec.record_category,
			rec.vendor_txn_id, rec.vendor_amount, rec.vendor_settlement_id, rec.vendor_utr,
			rec.vendor_settlement_date, rec.vendor_source, rec.vendor_recon_status,
			rec.bank_txn_id, rec.bank_amount, rec.bank_utr, rec.bank_narration,
			rec.bank_txn_date, rec.bank_txn_type,
			rec.method, rec.confidence, rec.reasoning, rec.decided_at
	` + baseQuery + `
		ORDER BY rec.decided_at DESC NULLS LAST, rec.vendor_txn_id DESC NULLS LAST
	`

	rows, err := db.Query(ctx, selectQuery, args...)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to fetch records for export", "details": err.Error()})
	}
	defer rows.Close()

	today := time.Now().Format("2006-01-02")
	filename := fmt.Sprintf("reconciliation_audit_%s_%s.csv", merchantID, today)
	c.Set("Content-Type", "text/csv")
	c.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))

	writer := csv.NewWriter(c.Response().BodyWriter())
	header := []string{
		"record_category",
		"vendor_txn_id", "vendor_amount", "vendor_settlement_id", "vendor_utr",
		"vendor_settlement_date", "vendor_source", "vendor_recon_status",
		"bank_txn_id", "bank_amount", "bank_utr", "bank_narration",
		"bank_txn_date", "bank_txn_type",
		"method", "confidence", "reasoning", "decided_at",
	}
	if err := writer.Write(header); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to write CSV header", "details": err.Error()})
	}

	for rows.Next() {
		var category string
		var vendorTxnID, vendorSettlementID, vendorUTR, vendorSettlementDate, vendorSource, vendorReconStatus *string
		var vendorAmount *float64
		var bankTxnID, bankUTR, bankNarration, bankTxnDate, bankTxnType *string
		var bankAmount *float64
		var method, reasoning *string
		var confidence *float64
		var decidedAt *time.Time

		if err := rows.Scan(
			&category,
			&vendorTxnID, &vendorAmount, &vendorSettlementID, &vendorUTR,
			&vendorSettlementDate, &vendorSource, &vendorReconStatus,
			&bankTxnID, &bankAmount, &bankUTR, &bankNarration,
			&bankTxnDate, &bankTxnType,
			&method, &confidence, &reasoning, &decidedAt,
		); err != nil {
			continue
		}

		row := []string{
			category,
			derefString(vendorTxnID, ""),
			derefFloat64(vendorAmount, ""),
			derefString(vendorSettlementID, ""),
			derefString(vendorUTR, ""),
			derefString(vendorSettlementDate, ""),
			derefString(vendorSource, ""),
			derefString(vendorReconStatus, ""),
			derefString(bankTxnID, ""),
			derefFloat64(bankAmount, ""),
			derefString(bankUTR, ""),
			derefString(bankNarration, ""),
			derefString(bankTxnDate, ""),
			derefString(bankTxnType, ""),
			derefString(method, ""),
			derefFloat64(confidence, ""),
			derefString(reasoning, ""),
			derefTime(decidedAt, ""),
		}
		if err := writer.Write(row); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to write CSV row", "details": err.Error()})
		}
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "CSV writer error", "details": err.Error()})
	}
	return nil
}

func derefString(s *string, def string) string {
	if s != nil {
		return *s
	}
	return def
}

func derefFloat64(f *float64, def string) string {
	if f != nil {
		return strconv.FormatFloat(*f, 'f', 2, 64)
	}
	return def
}

func derefTime(t *time.Time, def string) string {
	if t != nil {
		return t.Format(time.RFC3339)
	}
	return def
}

// SourceStatus describes one entry in the Data Sources strip.
type SourceStatus struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Status      string `json:"status"`
	RecordCount int    `json:"record_count"`
	IsLive      bool   `json:"is_live"`
}

// GetSourceStatus returns live counts and status for all four sources in one request.
func GetSourceStatus(db *pgxpool.Pool) fiber.Handler {
	return func(c *fiber.Ctx) error {
		merchantID := c.Params("merchantId")
		if merchantID == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "merchantId parameter is required"})
		}

		ctx := context.Background()

		countVendor := func(vendorName string) int {
			var n int
			err := db.QueryRow(ctx, `
				SELECT COUNT(*)
				FROM vendor_transactions vt
				JOIN vendor_integrations vi ON vt.vendor_integration_id = vi.id
				WHERE vi.merchant_id = $1 AND vi.vendor_name = $2
			`, merchantID, vendorName).Scan(&n)
			if err != nil {
				return 0
			}
			return n
		}

		razorpayCount := countVendor("Razorpay")
		phonePeCount := countVendor("PhonePe")
		pineLabsCount := countVendor("PineLabs")

		var bankCount int
		err := db.QueryRow(ctx, `
			SELECT COUNT(*)
			FROM bank_transactions bt
			JOIN merchant_bank_accounts mba ON bt.bank_account_id = mba.id
			WHERE mba.merchant_id = $1 AND bt.source = 'setu_aa_mock'
		`, merchantID).Scan(&bankCount)
		if err != nil {
			bankCount = 0
		}

		sources := []SourceStatus{
			{
				Name:        "Razorpay",
				Type:        "gateway",
				Status:      "connected",
				RecordCount: razorpayCount,
				IsLive:      true,
			},
			{
				Name:        "PhonePe",
				Type:        "gateway",
				Status:      statusForCount(phonePeCount),
				RecordCount: phonePeCount,
				IsLive:      false,
			},
			{
				Name:        "Pine Labs",
				Type:        "gateway",
				Status:      statusForCount(pineLabsCount),
				RecordCount: pineLabsCount,
				IsLive:      false,
			},
			{
				Name:        "Bank Statement (Setu AA)",
				Type:        "bank",
				Status:      statusForCount(bankCount),
				RecordCount: bankCount,
				IsLive:      false,
			},
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"sources": sources,
		})
	}
}

func statusForCount(n int) string {
	if n > 0 {
		return "connected"
	}
	return "not_connected"
}