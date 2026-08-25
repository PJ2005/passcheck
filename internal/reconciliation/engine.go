package reconciliation

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Confidence scores per matching tier. These are written to
// reconciliation_log.confidence so the audit trail records how sure the
// engine was about each decision, not just the decision itself.
const (
	tier1Confidence = 1.0  // exact full settlement_id in a single-settlement narration
	tier2Confidence = 0.9  // settlement_id inside a lumped multi-settlement narration (sum unverified)
	tier3Confidence = 0.7  // unique partial (first-8-chars) settlement_id match, truncated narration
	tier4Confidence = 0.85 // legacy UTR + amount-within-fee-tolerance fallback
)

// Indian banks process NEFT credits with delays: late-night settlements land
// in the NEXT day's bank batch (T+1 bleed), and weekends/holidays push
// processing out further still. A bank credit dated anywhere from the vendor
// settlement day through the END of settlement_day + dateWindowExtraDays is
// therefore a valid candidate in every tier.
const dateWindowExtraDays = 2

// Minimum usable settlement_id length for the truncated-narration tier.
// Shorter prefixes are too loose to be trusted as a match key.
const minPartialIDLen = 8

// Razorpay charges merchants roughly a 2% platform fee, so a legacy vendor
// row (which stores GROSS) can legitimately differ from its bank credit
// (NET) by that much. We allow 3% headroom: loose enough to absorb fees,
// tight enough to reject coincidentally similar amounts.
const legacyFeeTolerance = 0.03

// A "lumped" bank narration concatenates several settlement IDs separated by
// commas or slashes (e.g. "... MULTI SETL setl_A,setl_B,setl_C /REF ...").
// This pattern detects two or more setl_* tokens chained through
// delimiters, which is how the engine tells a lumped narration apart from a
// clean single-settlement one: clean narrations contain exactly one setl_*
// token and never match.
const lumpedNarrationPattern = `(setl_[A-Za-z0-9]+[^A-Za-z0-9]+)+setl_[A-Za-z0-9]+`

// vendorTxn is one UNMATCHED vendor-side record awaiting resolution.
type vendorTxn struct {
	ID             string
	UTRNumber      *string // nil on modern rows; populated mainly on legacy data
	Amount         float64 // gross amount charged to the payer
	SettlementID   *string // primary match key (nil/empty simulates older data)
	SettlementDate *time.Time
}

// matchContext bundles everything the per-tier queries need. Each tier method
// returns the matched bank transaction ID ("" when that tier finds nothing).
type matchContext struct {
	ctx        context.Context
	tx         pgx.Tx
	merchantID string
	v          vendorTxn
	windowLo   time.Time // settlement day, 00:00 (zero => no usable date window)
	windowHi   time.Time // exclusive upper bound, covers T+1 bleed + slack days
}

// RunDailyReconciliation resolves UNMATCHED vendor transactions against bank
// credits for a merchant using a tiered strategy, and writes EVERY decision -
// matched or unresolved - to the reconciliation_log audit table.
//
// Tier order (first hit wins):
//
//  1. Exact settlement_id in a single-settlement narration   (confidence 1.00)
//  2. settlement_id inside a lumped multi-settlement list     (confidence 0.90)
//  3. Unique partial (first 8 chars) settlement_id match      (confidence 0.70)
//  4. Legacy exact UTR + amount within fee tolerance          (confidence 0.85)
//
// It returns the number of transactions resolved this run. Unresolved rows
// stay UNMATCHED but are logged for the Phase 2 agent layer / human review.
func RunDailyReconciliation(merchantID string, db *pgxpool.Pool) (int, error) {
	log.Printf("Starting reconciliation engine for merchant: %s", merchantID)

	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// Snapshot the work queue up front so per-vendor queries reuse the same
	// transaction without holding a result cursor open across writes.
	rows, err := tx.Query(ctx, `
		SELECT vt.id, vt.utr_number, vt.amount, vt.settlement_id, vt.settlement_date
		FROM vendor_transactions vt
		JOIN vendor_integrations vi ON vt.vendor_integration_id = vi.id
		WHERE vi.merchant_id = $1 AND vt.recon_status = 'UNMATCHED'
	`, merchantID)
	if err != nil {
		return 0, fmt.Errorf("failed to query vendor transactions: %w", err)
	}

	var unmatched []vendorTxn
	for rows.Next() {
		var txn vendorTxn
		if err := rows.Scan(&txn.ID, &txn.UTRNumber, &txn.Amount, &txn.SettlementID, &txn.SettlementDate); err != nil {
			rows.Close()
			return 0, fmt.Errorf("failed to scan vendor transaction row: %w", err)
		}
		unmatched = append(unmatched, txn)
	}
	rows.Close() // explicit close so the connection is free for per-vendor queries

	matchedCount := 0
	for _, v := range unmatched {
		mc := newMatchContext(ctx, tx, merchantID, v)

		bankTxnID, confidence, reasoning, err := mc.match()
		if err != nil {
			// Unexpected DB failure: aborting rolls back the whole batch,
			// leaving no half-reconciled state behind.
			return 0, fmt.Errorf("matching failed for vendor txn %s: %w", v.ID, err)
		}

		if bankTxnID == "" {
			// Every tier exhausted. recon_status stays UNMATCHED (already its
			// default - deliberately untouched), but the attempt is audited.
			if err := mc.logUnresolved(reasoning); err != nil {
				return 0, fmt.Errorf("failed to log unresolved decision for vendor txn %s: %w", v.ID, err)
			}
			continue
		}

		if err := mc.applyMatch(bankTxnID, confidence, reasoning); err != nil {
			return 0, fmt.Errorf("failed to apply match for vendor txn %s: %w", v.ID, err)
		}
		matchedCount++
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("failed to commit transaction: %w", err)
	}

	log.Printf("Reconciliation complete. Matched %d of %d vendor transactions.", matchedCount, len(unmatched))
	return matchedCount, nil
}

// newMatchContext derives the date tolerance window from the vendor row's
// settlement date. The window spans settlement day 00:00 through an EXCLUSIVE
// bound of day+N+1 00:00 - i.e. inclusively covering the whole of day+N -
// which absorbs same-day credits, the T+1 overnight bleed, and weekend/holiday slack.
func newMatchContext(ctx context.Context, tx pgx.Tx, merchantID string, v vendorTxn) *matchContext {
	mc := &matchContext{ctx: ctx, tx: tx, merchantID: merchantID, v: v}
	if v.SettlementDate != nil {
		d := *v.SettlementDate
		mc.windowLo = time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, d.Location())
		mc.windowHi = mc.windowLo.AddDate(0, 0, dateWindowExtraDays+1)
	}
	return mc
}

// match walks the tiers in priority order and stops at the first candidate.
// An empty bankTxnID return means every tier was exhausted; reasoning then
// explains what was attempted, feeding the unresolved audit entry.
func (mc *matchContext) match() (bankTxnID string, confidence float64, reasoning string, err error) {
	sid := ""
	if mc.v.SettlementID != nil {
		sid = *mc.v.SettlementID
	}

	// Settlement-based tiers need both an ID to look for and a date window to
	// look inside. Rows missing either skip straight to the legacy fallback.
	if sid != "" && mc.hasDateWindow() {
		// ---- TIER 1: exact settlement_id, single-settlement narration ----
		// Bank NEFT credit narrations embed the gateway's settlement batch ID.
		// This is THE primary match key for the domain: banks settle gateways
		// in batches, so payment-level UTRs simply never appear on bank-side
		// credits. Lumped narrations are excluded here because several IDs
		// appear in them; those belong to Tier 2's lower-confidence handling.
		id, found, err := mc.findSettlementInNarration(sid, false)
		if err != nil {
			return "", 0, "", err
		}
		if found {
			return id, tier1Confidence,
				fmt.Sprintf("Tier 1: exact settlement_id match (%s) within date window", shortID(sid)), nil
		}

		// ---- TIER 2: settlement_id inside a lumped settlement narration ----
		// Banks sometimes merge several small settlements into ONE NEFT credit
		// whose narration lists every batch ID. Multiple vendor rows legitimately
		// map to the same bank row here, so amounts are NOT compared (the bank
		// row holds the summed net of ALL constituents). Verifying that sum is
		// deliberately left to the Phase 2 agent layer, hence confidence < 1.0.
		id, found, err = mc.findSettlementInNarration(sid, true)
		if err != nil {
			return "", 0, "", err
		}
		if found {
			return id, tier2Confidence,
				fmt.Sprintf("Tier 2: settlement_id %s found in lumped multi-settlement narration; total sum not verified in deterministic pass", shortID(sid)), nil
		}

		// ---- TIER 3: unique partial settlement_id (truncated narration) ----
		// Bank systems cap narration width and routinely chop long settlement
		// IDs mid-string. If exactly ONE bank credit in the date window contains
		// the ID's first 8 characters, it is almost certainly the counterpart -
		// but "almost certainly" via string heuristic earns less trust than an
		// exact hit. Multiple candidates means we refuse to guess.
		id, found, err = mc.findPartialSettlement(sid)
		if err != nil {
			return "", 0, "", err
		}
		if found {
			return id, tier3Confidence,
				fmt.Sprintf("Tier 3: unique partial settlement_id match on first %d chars of %s; confidence lowered due to truncated narration", minPartialIDLen, shortID(sid)), nil
		}
	}

	// ---- TIER 4: legacy UTR + tolerant-amount fallback ----
	// Rows predating the settlement_id era carry only a payment-level UTR.
	// They resolve the old way: exact UTR equality plus amount agreement
	// within the platform-fee band. This tier exists purely for backward
	// compatibility with historical data shapes.
	if mc.v.UTRNumber != nil && *mc.v.UTRNumber != "" {
		id, found, err := mc.findLegacyUTRMatch(*mc.v.UTRNumber)
		if err != nil {
			return "", 0, "", err
		}
		if found {
			return id, tier4Confidence,
				fmt.Sprintf("Tier 4: legacy fallback - exact UTR match with amount within %.0f%% fee tolerance", legacyFeeTolerance*100), nil
		}
	}

	// Nothing matched anywhere: describe the search for the audit log.
	attempted := "No bank transaction found"
	if sid != "" {
		attempted = fmt.Sprintf("No bank transaction containing settlement_id %s (full, partial, or lumped) within date window", shortID(sid))
	}
	if mc.v.UTRNumber != nil && *mc.v.UTRNumber != "" {
		attempted += ", and UTR+amount fallback also failed"
	}
	return "", 0, attempted + ".", nil
}

// findSettlementInNarration looks for the FULL settlement_id inside a bank
// narration. lumpedOnly=false targets clean single-settlement credits;
// lumpedOnly=true restricts to lumped multi-ID narrations (Tier 2).
//
// The pairing-scoped NOT EXISTS reflects that reconciled_matches.bank_
// transaction_id is no longer UNIQUE: the question is whether THIS vendor row
// has already been linked to THAT bank row, not whether the bank row has any
// links at all - one bank credit may legitimately close out many vendors.
func (mc *matchContext) findSettlementInNarration(settlementID string, lumpedOnly bool) (string, bool, error) {
	var id string
	err := mc.tx.QueryRow(mc.ctx, `
		SELECT bt.id
		FROM bank_transactions bt
		JOIN merchant_bank_accounts mba ON bt.bank_account_id = mba.id
		WHERE mba.merchant_id = $1
		  AND bt.narration LIKE '%' || $2 || '%'
		  AND bt.txn_date >= $3 AND bt.txn_date < $4
		  AND (($5 AND bt.narration ~ $6) OR (NOT $5 AND bt.narration !~ $6))
		  AND NOT EXISTS (
		      SELECT 1 FROM reconciled_matches rm
		      WHERE rm.vendor_transaction_id = $7 AND rm.bank_transaction_id = bt.id
		  )
		ORDER BY bt.txn_date
		LIMIT 1
	`, mc.merchantID, settlementID, mc.windowLo, mc.windowHi, lumpedOnly, lumpedNarrationPattern, mc.v.ID).Scan(&id)

	if err == pgx.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

// findPartialSettlement handles truncated bank narrations: the settlement_id
// was cut off mid-string by the bank's column limit. We search for the first
// minPartialIDLen characters and only accept a UNIQUE survivor - ambiguity
// here would mean guessing with real money, which the engine never does.
func (mc *matchContext) findPartialSettlement(settlementID string) (string, bool, error) {
	if len(settlementID) < minPartialIDLen {
		return "", false, nil
	}
	prefix := settlementID[:minPartialIDLen]

	baseWhere := `
		FROM bank_transactions bt
		JOIN merchant_bank_accounts mba ON bt.bank_account_id = mba.id
		WHERE mba.merchant_id = $1
		  AND bt.narration LIKE '%' || $2 || '%'
		  AND bt.txn_date >= $3 AND bt.txn_date < $4
		  AND NOT EXISTS (
		      SELECT 1 FROM reconciled_matches rm
		      WHERE rm.vendor_transaction_id = $5 AND rm.bank_transaction_id = bt.id
		  )`
	args := []any{mc.merchantID, prefix, mc.windowLo, mc.windowHi, mc.v.ID}

	var count int
	if err := mc.tx.QueryRow(mc.ctx, "SELECT COUNT(*) "+baseWhere, args...).Scan(&count); err != nil {
		return "", false, err
	}
	if count != 1 {
		return "", false, nil // zero candidates, or ambiguous - refuse both
	}

	var id string
	err := mc.tx.QueryRow(mc.ctx, "SELECT bt.id "+baseWhere+" ORDER BY bt.txn_date LIMIT 1", args...).Scan(&id)
	if err == pgx.ErrNoRows {
		return "", false, nil // vanished between the two statements; treat as no-match
	}
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

// findLegacyUTRMatch is the pre-settlement-id matching strategy, retained for
// old data: exact UTR equality, but amount compared within the platform-fee
// band instead of strict equality, because vendor rows store gross while the
// bank credit reflects net-of-fees.
func (mc *matchContext) findLegacyUTRMatch(utr string) (string, bool, error) {
	var id string
	err := mc.tx.QueryRow(mc.ctx, `
		SELECT bt.id
		FROM bank_transactions bt
		JOIN merchant_bank_accounts mba ON bt.bank_account_id = mba.id
		WHERE mba.merchant_id = $1
		  AND bt.utr_number = $2
		  AND ABS(bt.amount - $3) <= $3 * $4
		  AND NOT EXISTS (
		      SELECT 1 FROM reconciled_matches rm
		      WHERE rm.vendor_transaction_id = $5 AND rm.bank_transaction_id = bt.id
		  )
		ORDER BY bt.txn_date
		LIMIT 1
	`, mc.merchantID, utr, mc.v.Amount, legacyFeeTolerance, mc.v.ID).Scan(&id)

	if err == pgx.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

// applyMatch persists a successful resolution: the reconciled link, the
// vendor status flip, and the deterministic audit entry - all inside the
// caller's open transaction.
func (mc *matchContext) applyMatch(bankTxnID string, confidence float64, reasoning string) error {
	if _, err := mc.tx.Exec(mc.ctx, `
		INSERT INTO reconciled_matches (vendor_transaction_id, bank_transaction_id)
		VALUES ($1, $2)
	`, mc.v.ID, bankTxnID); err != nil {
		return err
	}

	if _, err := mc.tx.Exec(mc.ctx, `
		UPDATE vendor_transactions SET recon_status = 'MATCHED' WHERE id = $1
	`, mc.v.ID); err != nil {
		return err
	}

	_, err := mc.tx.Exec(mc.ctx, `
		INSERT INTO reconciliation_log (vendor_transaction_id, bank_transaction_id, method, confidence, reasoning)
		VALUES ($1, $2, 'deterministic', $3, $4)
	`, mc.v.ID, bankTxnID, confidence, reasoning)
	return err
}

// logUnresolved records a fully-exhausted search so exceptions remain
// visible in the audit trail instead of silently dropping out of the run.
func (mc *matchContext) logUnresolved(reasoning string) error {
	_, err := mc.tx.Exec(mc.ctx, `
		INSERT INTO reconciliation_log (vendor_transaction_id, bank_transaction_id, method, confidence, reasoning)
		VALUES ($1, NULL, 'unresolved', NULL, $2)
	`, mc.v.ID, reasoning)
	return err
}

func (mc *matchContext) hasDateWindow() bool { return !mc.windowLo.IsZero() }

// shortID truncates a settlement ID for compact, human-readable log lines.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12] + "..."
	}
	return id
}
