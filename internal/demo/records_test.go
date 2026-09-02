package demo

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"testing"

	"passcheck/internal/database"

	"github.com/gofiber/fiber/v2"
	"github.com/joho/godotenv"
)

// TestAuditLedgerCategoryCounts is the mandatory verification test.
// It checks the exact arithmetic guarantee:
//   UV + M = V  (every vendor row appears exactly once)
//   UB + distinct_matched_bank = B  (every bank row appears exactly once)
func TestAuditLedgerCategoryCounts(t *testing.T) {
	if err := godotenv.Load("../../.env"); err != nil {
		t.Logf("env load note: %v", err)
	}

	db, err := database.NewConnectionPool()
	if err != nil {
		t.Fatalf("database pool error: %v", err)
	}
	defer db.Close()

	ctx := t.Context()

	var merchantID string
	err = db.Pool.QueryRow(ctx, "SELECT id FROM merchants LIMIT 1").Scan(&merchantID)
	if err != nil {
		t.Fatalf("failed to query merchant: %v", err)
	}

	// ── Step 1: Direct DB counts ───────────────────────────────────────────
	// V: total vendor_transactions for this merchant
	var V int
	err = db.Pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM vendor_transactions vt
		JOIN vendor_integrations vi ON vt.vendor_integration_id = vi.id
		WHERE vi.merchant_id = $1
	`, merchantID).Scan(&V)
	if err != nil {
		t.Fatalf("failed to count vendor_transactions (V): %v", err)
	}

	// B: total bank_transactions for this merchant
	var B int
	err = db.Pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM bank_transactions bt
		JOIN merchant_bank_accounts mba ON bt.bank_account_id = mba.id
		WHERE mba.merchant_id = $1
	`, merchantID).Scan(&B)
	if err != nil {
		t.Fatalf("failed to count bank_transactions (B): %v", err)
	}

	// M_db: total rows in reconciled_matches for this merchant (= matched_pair count)
	var M_db int
	err = db.Pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM reconciled_matches rm
		JOIN vendor_transactions vt ON vt.id = rm.vendor_transaction_id
		JOIN vendor_integrations vi ON vi.id = vt.vendor_integration_id
		WHERE vi.merchant_id = $1
	`, merchantID).Scan(&M_db)
	if err != nil {
		t.Fatalf("failed to count reconciled_matches (M_db): %v", err)
	}

	// UV_db: vendor rows with no reconciled_matches entry
	var UV_db int
	err = db.Pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM vendor_transactions vt
		JOIN vendor_integrations vi ON vi.id = vt.vendor_integration_id
		WHERE vi.merchant_id = $1
		  AND NOT EXISTS (SELECT 1 FROM reconciled_matches rm WHERE rm.vendor_transaction_id = vt.id)
	`, merchantID).Scan(&UV_db)
	if err != nil {
		t.Fatalf("failed to count unmatched_vendor (UV_db): %v", err)
	}

	// UB_db: bank rows with no reconciled_matches entry
	var UB_db int
	err = db.Pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM bank_transactions bt
		JOIN merchant_bank_accounts mba ON mba.id = bt.bank_account_id
		WHERE mba.merchant_id = $1
		  AND NOT EXISTS (SELECT 1 FROM reconciled_matches rm WHERE rm.bank_transaction_id = bt.id)
	`, merchantID).Scan(&UB_db)
	if err != nil {
		t.Fatalf("failed to count unmatched_bank (UB_db): %v", err)
	}

	// distinct_matched_bank: distinct bank txns involved in at least one match
	var distinctMatchedBank int
	err = db.Pool.QueryRow(ctx, `
		SELECT COUNT(DISTINCT rm.bank_transaction_id)
		FROM reconciled_matches rm
		JOIN vendor_transactions vt ON vt.id = rm.vendor_transaction_id
		JOIN vendor_integrations vi ON vi.id = vt.vendor_integration_id
		WHERE vi.merchant_id = $1
	`, merchantID).Scan(&distinctMatchedBank)
	if err != nil {
		t.Fatalf("failed to count distinct matched bank txns: %v", err)
	}

	fmt.Println("\n═══════════════════════════════════════════════════════════")
	fmt.Println("  MANDATORY VERIFICATION — AUDIT LEDGER CATEGORY COUNTS")
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("V  (total vendor_transactions):         %d\n", V)
	fmt.Printf("B  (total bank_transactions):           %d\n", B)
	fmt.Printf("M_db (reconciled_matches rows):         %d\n", M_db)
	fmt.Printf("UV_db (vendor with no match):           %d\n", UV_db)
	fmt.Printf("UB_db (bank with no match):             %d\n", UB_db)
	fmt.Printf("distinct_matched_bank:                  %d\n", distinctMatchedBank)

	// DB-level arithmetic checks
	if UV_db+M_db != V {
		t.Errorf("DB ARITHMETIC FAIL: UV_db(%d) + M_db(%d) = %d ≠ V(%d)", UV_db, M_db, UV_db+M_db, V)
	} else {
		fmt.Printf("✓ DB CHECK 1: UV_db + M_db = %d + %d = %d = V\n", UV_db, M_db, UV_db+M_db)
	}
	if UB_db+distinctMatchedBank != B {
		t.Errorf("DB ARITHMETIC FAIL: UB_db(%d) + distinct_matched_bank(%d) = %d ≠ B(%d)", UB_db, distinctMatchedBank, UB_db+distinctMatchedBank, B)
	} else {
		fmt.Printf("✓ DB CHECK 2: UB_db + distinct_matched_bank = %d + %d = %d = B\n", UB_db, distinctMatchedBank, UB_db+distinctMatchedBank)
	}

	// ── Step 2: Call endpoint and verify response counts ───────────────────
	app := fiber.New()
	app.Get("/api/v1/demo/records/:merchantId", GetReconciliationRecords(db.Pool))

	// Use page_size=200 to get everything in one page (we have at most ~200 rows in test data)
	req := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/demo/records/%s?page_size=200", merchantID), nil)
	resp, err := app.Test(req, 15000)
	if err != nil {
		t.Fatalf("app.Test error: %v", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body error: %v", err)
	}

	var recResp AuditRecordsResponse
	if err := json.Unmarshal(bodyBytes, &recResp); err != nil {
		t.Fatalf("json unmarshal error: %v, raw: %s", err, string(bodyBytes))
	}

	// Count by category from endpoint response
	var M_ep, UV_ep, UB_ep int
	for _, r := range recResp.Records {
		switch r.RecordCategory {
		case "matched_pair":
			M_ep++
		case "unmatched_vendor":
			UV_ep++
		case "unmatched_bank":
			UB_ep++
		}
	}

	fmt.Println("\n───────────────────────────────────────────────────────────")
	fmt.Println("  ENDPOINT RESPONSE COUNTS")
	fmt.Println("───────────────────────────────────────────────────────────")
	fmt.Printf("total_count (from envelope):            %d\n", recResp.TotalCount)
	fmt.Printf("Records in page:                        %d\n", len(recResp.Records))
	fmt.Printf("M  (matched_pair rows):                 %d\n", M_ep)
	fmt.Printf("UV (unmatched_vendor rows):             %d\n", UV_ep)
	fmt.Printf("UB (unmatched_bank rows):               %d\n", UB_ep)

	// Endpoint arithmetic checks
	if UV_ep+M_ep != V {
		t.Errorf("ENDPOINT FAIL: UV(%d) + M(%d) = %d ≠ V(%d)  — vendor rows missing or duplicated", UV_ep, M_ep, UV_ep+M_ep, V)
	} else {
		fmt.Printf("✓ ENDPOINT CHECK 1: UV + M = %d + %d = %d = V ✓\n", UV_ep, M_ep, UV_ep+M_ep)
	}
	if UB_ep+distinctMatchedBank != B {
		t.Errorf("ENDPOINT FAIL: UB(%d) + distinct_matched_bank(%d) = %d ≠ B(%d)  — bank rows missing or duplicated", UB_ep, distinctMatchedBank, UB_ep+distinctMatchedBank, B)
	} else {
		fmt.Printf("✓ ENDPOINT CHECK 2: UB + distinct_matched_bank = %d + %d = %d = B ✓\n", UB_ep, distinctMatchedBank, UB_ep+distinctMatchedBank)
	}
	if recResp.TotalCount != M_ep+UV_ep+UB_ep {
		t.Errorf("ENVELOPE MISMATCH: total_count=%d but M+UV+UB=%d", recResp.TotalCount, M_ep+UV_ep+UB_ep)
	} else {
		fmt.Printf("✓ ENVELOPE CHECK: total_count = M + UV + UB = %d ✓\n", recResp.TotalCount)
	}

	fmt.Println("═══════════════════════════════════════════════════════════")
}

// TestReconciliationRecordsCounts kept for backward compat but now delegates to the above test.
func TestReconciliationRecordsCounts(t *testing.T) {
	TestAuditLedgerCategoryCounts(t)
}

func TestReconciliationRecordsCSV(t *testing.T) {
	if err := godotenv.Load("../../.env"); err != nil {
		t.Logf("env load note: %v", err)
	}

	db, err := database.NewConnectionPool()
	if err != nil {
		t.Fatalf("database pool error: %v", err)
	}
	defer db.Close()

	ctx := t.Context()

	var merchantID string
	err = db.Pool.QueryRow(ctx, "SELECT id FROM merchants LIMIT 1").Scan(&merchantID)
	if err != nil {
		t.Fatalf("failed to query merchant: %v", err)
	}

	app := fiber.New()
	app.Get("/api/v1/demo/records/:merchantId", GetReconciliationRecords(db.Pool))

	req := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/demo/records/%s?format=csv", merchantID), nil)
	resp, err := app.Test(req, 10000)
	if err != nil {
		t.Fatalf("app.Test error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body error: %v", err)
	}

	fmt.Printf("CSV output generated, length: %d bytes\n", len(bodyBytes))
	// Verify first line is the correct header
	lines := string(bodyBytes)
	expectedHeader := "record_category,vendor_txn_id,vendor_amount,vendor_settlement_id,vendor_utr,vendor_settlement_date,vendor_source,vendor_recon_status,bank_txn_id,bank_amount,bank_utr,bank_narration,bank_txn_date,bank_txn_type,method,confidence,reasoning,decided_at"
	firstLine := ""
	for i, c := range lines {
		if c == '\n' {
			firstLine = lines[:i]
			break
		}
	}
	if firstLine != expectedHeader {
		t.Errorf("CSV header mismatch\n got:  %s\nwant: %s", firstLine, expectedHeader)
	} else {
		fmt.Println("✓ CSV header matches expected columns")
	}
}

func TestSourceStatus(t *testing.T) {
	if err := godotenv.Load("../../.env"); err != nil {
		t.Logf("env load note: %v", err)
	}

	db, err := database.NewConnectionPool()
	if err != nil {
		t.Fatalf("database pool error: %v", err)
	}
	defer db.Close()

	ctx := t.Context()

	var merchantID string
	err = db.Pool.QueryRow(ctx, "SELECT id FROM merchants LIMIT 1").Scan(&merchantID)
	if err != nil {
		t.Fatalf("failed to query merchant: %v", err)
	}

	app := fiber.New()
	app.Get("/api/v1/demo/sources/:merchantId", GetSourceStatus(db.Pool))

	req := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/demo/sources/%s", merchantID), nil)
	resp, err := app.Test(req, 10000)
	if err != nil {
		t.Fatalf("app.Test error: %v", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	var data struct {
		Sources []SourceStatus `json:"sources"`
	}
	if err := json.Unmarshal(bodyBytes, &data); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	total := 0
	fmt.Println("\n--- CURRENT SOURCE STATUS BREAKDOWN ---")
	for _, s := range data.Sources {
		fmt.Printf("%-25s: %d records (status: %s)\n", s.Name, s.RecordCount, s.Status)
		total += s.RecordCount
	}
	fmt.Printf("Total across sources: %d\n", total)

	for _, s := range data.Sources {
		if s.Name == "Razorpay" && s.RecordCount != 56 {
			t.Errorf("Expected Razorpay to have 56 records, got %d", s.RecordCount)
		}
	}
}

func TestSimulateAllSourcesAndVerifyCounts(t *testing.T) {
	if err := godotenv.Load("../../.env"); err != nil {
		t.Logf("env load note: %v", err)
	}

	db, err := database.NewConnectionPool()
	if err != nil {
		t.Fatalf("database pool error: %v", err)
	}
	defer db.Close()

	ctx := t.Context()

	var merchantID string
	err = db.Pool.QueryRow(ctx, "SELECT id FROM merchants LIMIT 1").Scan(&merchantID)
	if err != nil {
		t.Fatalf("failed to query merchant: %v", err)
	}

	// Clean up previous mock simulation data to keep test idempotent
	_, _ = db.Pool.Exec(ctx, `
		DELETE FROM vendor_transactions vt
		USING vendor_integrations vi
		WHERE vt.vendor_integration_id = vi.id
		  AND vi.merchant_id = $1
		  AND vi.vendor_name IN ('PhonePe', 'PineLabs')
	`, merchantID)
	_, _ = db.Pool.Exec(ctx, `
		DELETE FROM bank_transactions bt
		USING merchant_bank_accounts mba
		WHERE bt.bank_account_id = mba.id
		  AND mba.merchant_id = $1
		  AND bt.source = 'setu_aa_mock'
	`, merchantID)

	if err := GenerateMockPhonePeData(ctx, db.Pool, merchantID, 10); err != nil {
		t.Fatalf("PhonePe simulation failed: %v", err)
	}
	if err := GenerateMockPineLabsData(ctx, db.Pool, merchantID, 10); err != nil {
		t.Fatalf("PineLabs simulation failed: %v", err)
	}
	if err := GenerateMockBankStatementData(ctx, db.Pool, merchantID, 10); err != nil {
		t.Fatalf("Bank simulation failed: %v", err)
	}

	app := fiber.New()
	app.Get("/api/v1/demo/sources/:merchantId", GetSourceStatus(db.Pool))

	req := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/demo/sources/%s", merchantID), nil)
	resp, err := app.Test(req, 10000)
	if err != nil {
		t.Fatalf("app.Test error: %v", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	var data struct {
		Sources []SourceStatus `json:"sources"`
	}
	if err := json.Unmarshal(bodyBytes, &data); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	total := 0
	counts := make(map[string]int)
	fmt.Println("\n--- AFTER CONNECTING ALL 4 SOURCES ---")
	for _, s := range data.Sources {
		fmt.Printf("%-25s: %d records (status: %s)\n", s.Name, s.RecordCount, s.Status)
		total += s.RecordCount
		counts[s.Name] = s.RecordCount
	}
	fmt.Printf("Total across all sources: %d\n", total)

	if counts["Razorpay"] != 56 {
		t.Errorf("Expected Razorpay to have 56 records, got %d", counts["Razorpay"])
	}
	if counts["PhonePe"] != 10 {
		t.Errorf("Expected PhonePe to have 10 records, got %d", counts["PhonePe"])
	}
	if counts["Pine Labs"] != 10 {
		t.Errorf("Expected Pine Labs to have 10 records, got %d", counts["Pine Labs"])
	}
	if counts["Bank Statement (Setu AA)"] != 10 {
		t.Errorf("Expected Bank Statement to have 10 records, got %d", counts["Bank Statement (Setu AA)"])
	}
	if total != 86 {
		t.Errorf("Expected total of 86 (56 + 10 + 10 + 10), got %d", total)
	} else {
		fmt.Println("SUCCESS: Exactly 56 Razorpay + 10 PhonePe + 10 Pine Labs + 10 Bank = 86 Total!")
	}
}
