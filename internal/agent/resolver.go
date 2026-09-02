package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AgentConfidenceThreshold is the minimum confidence the agent must report
// before its decision is allowed to override the deterministic engine's
// "unresolved" verdict. 0.75 is a deliberately chosen bar for this system's
// risk tolerance - high enough to keep plausible-sounding guesses out of the
// ledger, low enough to recover genuinely ambiguous-but-correct cases - not a
// figure borrowed from industry convention.
const AgentConfidenceThreshold = 0.75

// AgentDecision is the structured verdict we ask the model to return for one
// unresolved vendor transaction.
//
// Expected JSON shape from the model:
// {"is_match": true, "bank_transaction_id": "<uuid or empty string>", "confidence": 0.0-1.0, "reasoning": "<short explanation>"}
type AgentDecision struct {
	IsMatch           bool    `json:"is_match"`
	BankTransactionID string  `json:"bank_transaction_id"` // empty when IsMatch is false
	Confidence        float64 `json:"confidence"`
	Reasoning         string  `json:"reasoning"`
}

// resolverSystemInstruction pins the model to the judge role and forbids
// prose, so GenerateJSON's output stays directly decodable.
const resolverSystemInstruction = `You are a financial reconciliation assistant for an Indian payment gateway. You will be given one vendor-side settlement transaction and a short list of candidate bank credit transactions. Each candidate carries three pre-computed evidence signals: amount_ratio (bank amount divided by vendor amount; roughly 0.98 is expected because Indian gateway platform fees run about 2%, so values near 0.98 are strong evidence and values far outside the 0.90-1.02 band are weak), date_gap_days (smaller gaps are stronger evidence, but T+1 overnight credits and weekend or holiday delays of up to 2-3 days are normal banking behavior, not disqualifying), and settlement_id_overlap (how many characters of the vendor's settlement_id appear in the candidate narration; higher overlap out of the total length is stronger evidence, especially where bank systems truncate narrations). Base your reasoning primarily on these three computed signals rather than re-deriving evidence from raw amounts or narration text yourself. If no candidate clears the bar confidently, say so explicitly rather than guessing. Respond ONLY with JSON matching this exact shape: {"is_match": boolean, "bank_transaction_id": string, "confidence": number between 0 and 1, "reasoning": string under 200 characters}.`

// unresolvedTxn is one vendor record the deterministic engine gave up on.
type unresolvedTxn struct {
	ID             string
	SettlementID   *string
	UTRNumber      *string
	Amount         float64
	SettlementDate *time.Time
	PriorReasoning string // why Phase 1 skipped it - context for the model
}

// bankCandidate is one possible counterpart shown to the model.
type bankCandidate struct {
	ID        string
	Amount    float64
	TxnDate   time.Time
	Narration string
	UTRNumber string
}

// ResolveExceptions runs the Phase 2 agent pass over every vendor transaction
// still UNMATCHED after the deterministic engine. Each row's plausible bank
// credits are gathered and submitted to Gemini for judgment; accepted verdicts
// are written straight into the ledger, and every other outcome - model said
// no, low confidence, unparsable response, API failure, no candidates - is
// recorded in reconciliation_log as an audited 'unresolved' decision rather
// than silently dropped.
//
// All DB work happens in a single transaction (all-or-nothing batch, same as
// Phase 1). Gemini calls run sequentially per row; one failing API call logs
// that row as unresolved and moves on instead of sinking the batch.
//
// It returns how many previously-unresolved rows the agent successfully
// matched this run.
func ResolveExceptions(ctx context.Context, merchantID string, db *pgxpool.Pool, agentClient *GeminiClient) (resolvedCount int, err error) {
	log.Printf("Starting agent exception resolution for merchant: %s", merchantID)

	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		SELECT vt.id, vt.settlement_id, vt.utr_number, vt.amount, vt.settlement_date,
		       COALESCE(prior.reasoning, '')
		FROM vendor_transactions vt
		JOIN vendor_integrations vi ON vt.vendor_integration_id = vi.id
		CROSS JOIN LATERAL (
			SELECT r.reasoning
			FROM reconciliation_log r
			WHERE r.vendor_transaction_id = vt.id AND r.method = 'unresolved'
			ORDER BY r.created_at DESC
			LIMIT 1
		) prior
		WHERE vi.merchant_id = $1 AND vt.recon_status = 'UNMATCHED'
		ORDER BY vt.created_at
	`, merchantID)
	if err != nil {
		return 0, fmt.Errorf("failed to query unresolved vendor transactions: %w", err)
	}

	var pending []unresolvedTxn
	for rows.Next() {
		var v unresolvedTxn
		if err := rows.Scan(&v.ID, &v.SettlementID, &v.UTRNumber, &v.Amount, &v.SettlementDate, &v.PriorReasoning); err != nil {
			rows.Close()
			return 0, fmt.Errorf("failed to scan unresolved vendor transaction row: %w", err)
		}
		pending = append(pending, v)
	}
	rows.Close()

	log.Printf("Agent pass: %d exception rows to review.", len(pending))

	for _, v := range pending {
		candidates, err := fetchCandidates(ctx, tx, merchantID, v)
		if err != nil {
			return 0, fmt.Errorf("failed to fetch candidates for vendor txn %s: %w", v.ID, err)
		}

		// Nothing plausible to compare against: calling the model would burn
		// tokens to hear "no". Audit it as unresolved and move on.
		if len(candidates) == 0 {
			if err := writeAuditRow(ctx, tx, v.ID, nil,
				"No candidate bank transactions found within the extended date window; likely a genuinely missing or delayed settlement"); err != nil {
				return 0, fmt.Errorf("failed to log unresolved decision for vendor txn %s: %w", v.ID, err)
			}
			continue
		}

		rawResponse, err := agentClient.GenerateJSON(ctx, resolverSystemInstruction, buildResolverPrompt(v, candidates))
		if err != nil {
			// Network/rate-limit/model failure on ONE row must not sink the
			// rest of the batch: audit it and continue.
			log.Printf("Agent call failed for vendor txn %s: %v", v.ID, err)
			if auditErr := writeAuditRow(ctx, tx, v.ID, nil,
				fmt.Sprintf("Agent call failed: %s", err.Error())); auditErr != nil {
				return 0, fmt.Errorf("failed to log agent failure for vendor txn %s: %w", v.ID, auditErr)
			}
			continue
		}

		var decision AgentDecision
		if err := json.Unmarshal([]byte(rawResponse), &decision); err != nil {
			if auditErr := writeAuditRow(ctx, tx, v.ID, nil,
				fmt.Sprintf("Agent response could not be parsed as valid JSON: %s", firstN(rawResponse, 100))); auditErr != nil {
				return 0, fmt.Errorf("failed to log parse failure for vendor txn %s: %w", v.ID, auditErr)
			}
			continue
		}

		// Confidence gate: the model saying "yes" is not enough on its own -
		// low-confidence verdicts stay unresolved with their reasoning logged
		// for human review rather than being written into the ledger.
		if !decision.IsMatch || decision.Confidence < AgentConfidenceThreshold {
			if auditErr := writeAuditRow(ctx, tx, v.ID, decision.Confidence,
				fmt.Sprintf("Agent reviewed but did not clear confidence threshold: %s", decision.Reasoning)); auditErr != nil {
				return 0, fmt.Errorf("failed to log below-threshold decision for vendor txn %s: %w", v.ID, auditErr)
			}
			continue
		}

		// Guard against hallucinated identifiers: the chosen bank row must be
		// one of the candidates we actually offered. Writing an unknown UUID
		// would violate the foreign key and abort the entire batch.
		chosenID := ""
		for _, c := range candidates {
			if c.ID == decision.BankTransactionID {
				chosenID = c.ID
				break
			}
		}
		if chosenID == "" {
			if auditErr := writeAuditRow(ctx, tx, v.ID, decision.Confidence,
				fmt.Sprintf("Agent selected bank transaction %q which was not among the provided candidates; rejected", decision.BankTransactionID)); auditErr != nil {
				return 0, fmt.Errorf("failed to log invalid candidate selection for vendor txn %s: %w", v.ID, auditErr)
			}
			continue
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO reconciled_matches (vendor_transaction_id, bank_transaction_id)
			VALUES ($1, $2)
		`, v.ID, chosenID); err != nil {
			return 0, fmt.Errorf("failed to insert agent match for vendor txn %s: %w", v.ID, err)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE vendor_transactions SET recon_status = 'MATCHED' WHERE id = $1
		`, v.ID); err != nil {
			return 0, fmt.Errorf("failed to update recon_status for vendor txn %s: %w", v.ID, err)
		}

		if err := writeAgentMatchAuditRow(ctx, tx, v.ID, chosenID, decision.Confidence, decision.Reasoning); err != nil {
			return 0, fmt.Errorf("failed to log agent match for vendor txn %s: %w", v.ID, err)
		}

		resolvedCount++
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("failed to commit transaction: %w", err)
	}

	log.Printf("Agent pass complete. Resolved %d of %d exceptions.", resolvedCount, len(pending))
	return resolvedCount, nil
}

// fetchCandidates gathers the plausible bank credits for one unresolved row.
// Two deterministic guards run in the database before anything reaches the
// model: a date window (settlement day through day+3 inclusive - one day
// wider than Phase 1, since the agent exists to catch cases the narrow
// window missed) and an amount band of 90%-102% of the vendor amount, which
// admits the ~2% platform-fee relationship with headroom while keeping
// obviously unrelated amounts from wasting model attention. Survivors are
// ordered by amount proximity so the likeliest candidates lead the prompt,
// and capped at 5 to bound prompt size and cost.
func fetchCandidates(ctx context.Context, tx pgx.Tx, merchantID string, v unresolvedTxn) ([]bankCandidate, error) {
	if v.SettlementDate == nil {
		return nil, nil // no date anchor -> no meaningful window to search
	}
	d := *v.SettlementDate
	windowLo := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, d.Location())
	windowHi := windowLo.AddDate(0, 0, 4) // exclusive: covers all of day+3

	rows, err := tx.Query(ctx, `
		SELECT bt.id, bt.amount, bt.txn_date, COALESCE(bt.narration, ''), COALESCE(bt.utr_number, '')
		FROM bank_transactions bt
		JOIN merchant_bank_accounts mba ON bt.bank_account_id = mba.id
		WHERE mba.merchant_id = $1
		  AND bt.txn_date >= $2 AND bt.txn_date < $3
		  AND bt.amount >= $4 * 0.90 AND bt.amount <= $4 * 1.02
		  AND NOT EXISTS (
		      SELECT 1 FROM reconciled_matches rm WHERE rm.bank_transaction_id = bt.id AND rm.vendor_transaction_id = $5
		  )
		ORDER BY ABS(bt.amount - $4)
		LIMIT 5
	`, merchantID, windowLo, windowHi, v.Amount, v.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var candidates []bankCandidate
	for rows.Next() {
		var c bankCandidate
		if err := rows.Scan(&c.ID, &c.Amount, &c.TxnDate, &c.Narration, &c.UTRNumber); err != nil {
			return nil, err
		}
		candidates = append(candidates, c)
	}
	return candidates, rows.Err()
}

// buildResolverPrompt renders one vendor row plus its candidates as labeled
// blocks. For every candidate it pre-computes the three evidence signals the
// model is asked to reason over - amount_ratio, date_gap_days, and
// settlement_id_overlap - so Gemini judges stated numbers instead of
// re-deriving them (imperfectly) from raw narration text.
func buildResolverPrompt(v unresolvedTxn, candidates []bankCandidate) string {
	var b strings.Builder
	fmt.Fprintf(&b, "VENDOR TRANSACTION (needs matching)\n")
	fmt.Fprintf(&b, "- settlement_id: %s\n", derefStr(v.SettlementID))
	fmt.Fprintf(&b, "- utr_number: %s\n", derefStr(v.UTRNumber))
	fmt.Fprintf(&b, "- amount: %.2f\n", v.Amount)
	fmt.Fprintf(&b, "- settlement_date: %s\n", formatDate(v.SettlementDate))
	if v.PriorReasoning != "" {
		fmt.Fprintf(&b, "- prior deterministic pass note: %s\n", v.PriorReasoning)
	}

	sid := derefStr(v.SettlementID)

	fmt.Fprintf(&b, "\nCANDIDATE BANK CREDITS (%d)\n", len(candidates))
	for i, c := range candidates {
		fmt.Fprintf(&b, "\n%d. Candidate:\n", i+1)
		fmt.Fprintf(&b, "   - id: %s\n", c.ID)
		fmt.Fprintf(&b, "   - amount: %.2f\n", c.Amount)
		fmt.Fprintf(&b, "   - txn_date: %s\n", c.TxnDate.Format("2006-01-02 15:04"))
		fmt.Fprintf(&b, "   - utr: %s\n", c.UTRNumber)
		fmt.Fprintf(&b, "   - narration: %q\n", c.Narration)

		if v.Amount != 0 {
			fmt.Fprintf(&b, "   - amount_ratio (bank/vendor, ~0.98 expected for typical 2%% platform fee): %.4f\n",
				c.Amount/v.Amount)
		} else {
			fmt.Fprintf(&b, "   - amount_ratio (bank/vendor): n/a\n")
		}

		fmt.Fprintf(&b, "   - date_gap_days: %d\n", dateGapDays(v.SettlementDate, &c.TxnDate))

		if sid != "" && sid != "(none)" {
			fmt.Fprintf(&b, "   - settlement_id_overlap: %d of %d characters matched\n",
				longestPrefixOverlap(sid, c.Narration), len(sid))
		} else {
			fmt.Fprintf(&b, "   - settlement_id_overlap: n/a (vendor has no settlement_id)\n")
		}
	}
	return b.String()
}

// dateGapDays returns the whole-day distance between two timestamps, rounded
// to the nearest day so a 23:45 -> 01:15 overnight bleed reads as 1 day.
func dateGapDays(a, b *time.Time) int {
	if a == nil || b == nil {
		return -1 // unknown; the model should weigh other signals instead
	}
	gap := a.Sub(*b)
	if gap < 0 {
		gap = -gap
	}
	return int(math.Round(gap.Hours() / 24))
}

// longestPrefixOverlap is a deliberately simple overlap proxy: anchored at the
// settlement_id's first character, it finds the longest run of consecutive ID
// characters appearing anywhere in the text. Bank truncation cuts the TAIL of
// narrations, so true counterparts keep an unbroken prefix of the ID - exactly
// what this measures. Returns the character count; compare against len(id).
func longestPrefixOverlap(id, text string) int {
	if id == "" || text == "" {
		return 0
	}
	best := 0
	for i := 0; i < len(text); i++ {
		if text[i] != id[0] {
			continue
		}
		n := 0
		for n < len(id) && i+n < len(text) && text[i+n] == id[n] {
			n++
		}
		if n > best {
			best = n
		}
	}
	return best
}

// writeAuditRow records an 'unresolved' decision in the audit trail. A nil
// confidence (used for API/parse/no-candidate outcomes where the model
// produced nothing usable) is stored as SQL NULL.
func writeAuditRow(ctx context.Context, tx pgx.Tx, vendorTxnID string, confidence any, reasoning string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO reconciliation_log (vendor_transaction_id, bank_transaction_id, method, confidence, reasoning)
		VALUES ($1, NULL, 'unresolved', $2, $3)
	`, vendorTxnID, confidence, reasoning)
	return err
}

// writeAgentMatchAuditRow records a successful agent resolution with its own
// method ('agent') and the matched bank transaction, distinct from Phase 1's
// 'deterministic' entries so the two passes remain separable in reporting.
func writeAgentMatchAuditRow(ctx context.Context, tx pgx.Tx, vendorTxnID, bankTxnID string, confidence float64, reasoning string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO reconciliation_log (vendor_transaction_id, bank_transaction_id, method, confidence, reasoning)
		VALUES ($1, $2, 'agent', $3, $4)
	`, vendorTxnID, bankTxnID, confidence, reasoning)
	return err
}

func derefStr(s *string) string {
	if s == nil {
		return "(none)"
	}
	return *s
}

func formatDate(t *time.Time) string {
	if t == nil {
		return "(none)"
	}
	return t.Format("2006-01-02")
}

// firstN truncates s to at most n characters, for embedding raw model output
// into audit reasoning without bloating rows with runaway responses.
func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
