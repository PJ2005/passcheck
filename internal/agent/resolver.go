package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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
const resolverSystemInstruction = `You are a financial reconciliation assistant for an Indian payment gateway. You will be given one vendor-side settlement transaction and a small list of candidate bank credit transactions. Determine if any single candidate is very likely the true match for the vendor transaction, based on amount similarity, date proximity, and any settlement_id or UTR fragments visible in the bank narration. Indian NEFT bank credits often show truncated or lumped settlement references, so partial textual overlap is meaningful evidence, not disqualifying. Vendor amounts are GROSS while bank credit amounts are NET of roughly 2% platform fees: a candidate within about 2% of the vendor amount is financially consistent, not a mismatch. If no candidate is a confident match, say so explicitly rather than guessing. Respond ONLY with JSON matching this exact shape: {"is_match": boolean, "bank_transaction_id": string, "confidence": number between 0 and 1, "reasoning": string under 200 characters}.`

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

// fetchCandidates gathers the plausible bank credits for one unresolved row:
// credits not yet consumed by ANY reconciled link, dated between the vendor
// settlement day and day+3 inclusive - one day wider than Phase 1's window,
// because the agent exists precisely to catch cases the narrow window missed.
// Ordering by amount proximity puts the most likely candidates near the top of
// the model's attention, and LIMIT 5 bounds prompt size and cost.
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
		  AND NOT EXISTS (
		      SELECT 1 FROM reconciled_matches rm WHERE rm.bank_transaction_id = bt.id
		  )
		ORDER BY ABS(bt.amount - $4)
		LIMIT 5
	`, merchantID, windowLo, windowHi, v.Amount)
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

// buildResolverPrompt renders one vendor row plus its candidates in a stable,
// plainly-labeled layout so the model can ground every claim in a visible field.
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

	fmt.Fprintf(&b, "\nCANDIDATE BANK CREDITS (%d)\n", len(candidates))
	for i, c := range candidates {
		fmt.Fprintf(&b, "%d. id: %s | amount: %.2f | txn_date: %s | utr: %s | narration: %q\n",
			i+1, c.ID, c.Amount, c.TxnDate.Format("2006-01-02 15:04"), c.UTRNumber, c.Narration)
	}
	return b.String()
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
