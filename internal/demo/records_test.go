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

func TestReconciliationRecordsCounts(t *testing.T) {
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

	// 1. Direct DB counts
	var vendorCount int
	err = db.Pool.QueryRow(ctx, `
		SELECT COUNT(*) 
		FROM vendor_transactions vt
		JOIN vendor_integrations vi ON vt.vendor_integration_id = vi.id
		WHERE vi.merchant_id = $1
	`, merchantID).Scan(&vendorCount)
	if err != nil {
		t.Fatalf("failed to count vendor_transactions: %v", err)
	}

	var bankCount int
	err = db.Pool.QueryRow(ctx, `
		SELECT COUNT(*) 
		FROM bank_transactions bt
		JOIN merchant_bank_accounts mba ON bt.bank_account_id = mba.id
		WHERE mba.merchant_id = $1
	`, merchantID).Scan(&bankCount)
	if err != nil {
		t.Fatalf("failed to count bank_transactions: %v", err)
	}

	var matchedBankCount int
	err = db.Pool.QueryRow(ctx, `
		SELECT COUNT(DISTINCT bt.id) 
		FROM bank_transactions bt
		JOIN merchant_bank_accounts mba ON bt.bank_account_id = mba.id
		JOIN reconciled_matches rm ON rm.bank_transaction_id = bt.id
		WHERE mba.merchant_id = $1
	`, merchantID).Scan(&matchedBankCount)
	if err != nil {
		t.Fatalf("failed to count matched bank txns: %v", err)
	}

	var unmatchedBankCount int
	err = db.Pool.QueryRow(ctx, `
		SELECT COUNT(*) 
		FROM bank_transactions bt
		JOIN merchant_bank_accounts mba ON bt.bank_account_id = mba.id
		WHERE mba.merchant_id = $1
		  AND NOT EXISTS (SELECT 1 FROM reconciled_matches rm WHERE rm.bank_transaction_id = bt.id)
	`, merchantID).Scan(&unmatchedBankCount)
	if err != nil {
		t.Fatalf("failed to count unmatched bank txns: %v", err)
	}

	// 2. Call Fiber endpoint via httptest
	app := fiber.New()
	app.Get("/api/v1/demo/records/:merchantId", GetReconciliationRecords(db.Pool))

	req := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/demo/records/%s?page_size=100", merchantID), nil)
	resp, err := app.Test(req, 10000)
	if err != nil {
		t.Fatalf("app.Test error: %v", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body error: %v", err)
	}

	var recResp ReconciliationRecordsResponse
	if err := json.Unmarshal(bodyBytes, &recResp); err != nil {
		t.Fatalf("json unmarshal error: %v, raw: %s", err, string(bodyBytes))
	}

	fmt.Printf("\n--- TEST RECONCILIATION RESULTS ---\n")
	fmt.Printf("Total vendor_transactions for merchant: %d\n", vendorCount)
	fmt.Printf("Total bank_transactions for merchant:   %d\n", bankCount)
	fmt.Printf("  - Matched bank transactions:          %d (reachable & represented via vendor rows)\n", matchedBankCount)
	fmt.Printf("  - Unmatched bank transactions:        %d (independently listed in bank rows)\n", unmatchedBankCount)
	fmt.Printf("Endpoint returned total_count:          %d\n", recResp.TotalCount)
	fmt.Printf("Records returned in page:               %d\n", len(recResp.Records))

	vendorInRecords := 0
	bankInRecords := 0
	for _, r := range recResp.Records {
		if r.RecordType == "vendor" {
			vendorInRecords++
		} else if r.RecordType == "bank" {
			bankInRecords++
		}
	}
	fmt.Printf("  - Vendor records in response:         %d\n", vendorInRecords)
	fmt.Printf("  - Bank records in response:           %d\n", bankInRecords)

	expectedTotal := vendorCount + bankCount - matchedBankCount
	if recResp.TotalCount != expectedTotal || recResp.TotalCount != vendorCount+unmatchedBankCount {
		t.Errorf("MISMATCH: expected %d, got %d", expectedTotal, recResp.TotalCount)
	} else {
		fmt.Printf("SUCCESS: Reconciled perfectly! %d vendor + %d bank - %d matched bank = %d total_count\n",
			vendorCount, bankCount, matchedBankCount, recResp.TotalCount)
	}
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

	lines := string(bodyBytes)
	fmt.Printf("CSV output generated, length: %d bytes\n", len(lines))
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

	// Razorpay must be 56
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

	// 1. Simulate the 3 other sources
	if err := GenerateMockPhonePeData(ctx, db.Pool, merchantID, 10); err != nil {
		t.Fatalf("PhonePe simulation failed: %v", err)
	}
	if err := GenerateMockPineLabsData(ctx, db.Pool, merchantID, 10); err != nil {
		t.Fatalf("PineLabs simulation failed: %v", err)
	}
	if err := GenerateMockBankStatementData(ctx, db.Pool, merchantID, 10); err != nil {
		t.Fatalf("Bank simulation failed: %v", err)
	}

	// 2. Query source status endpoint
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
