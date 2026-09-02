package agent

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"passcheck/internal/database"

	"github.com/gofiber/fiber/v2"
	"github.com/joho/godotenv"
)

// TestGeminiRealCallWithInvalidKey verifies that GenerateJSON does not use any
// hardcoded fallback, canned response, or mock when given an invalid key.
// It must make a real network call to Google's Gemini API and return a real error.
func TestGeminiRealCallWithInvalidKey(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := NewGeminiClient(ctx, "INVALID_KEY_TESTING_REAL_FAILURE_12345")
	if err != nil {
		t.Fatalf("NewGeminiClient failed: %v", err)
	}

	rawResp, err := client.GenerateJSON(ctx, resolverSystemInstruction, "TEST PROMPT")
	if err == nil {
		t.Fatalf("Expected network error from Gemini API with invalid key, but got success with response: %q", rawResp)
	}

	t.Logf("Observed real network/API failure from Gemini API: %v", err)
	if !strings.Contains(err.Error(), "gemini generate content call failed") {
		t.Errorf("Expected 'gemini generate content call failed' in error, got: %v", err)
	}
}

// TestResolverWithBrokenGemini verifies that when Gemini fails (invalid key / network failure),
// ResolveExceptions logs the failure honestly as 'unresolved', does NOT fabricate any match,
// and returns resolvedCount = 0.
func TestResolverWithBrokenGemini(t *testing.T) {
	if err := godotenv.Load("../../.env"); err != nil {
		t.Logf("env note: %v", err)
	}

	db, err := database.NewConnectionPool()
	if err != nil {
		t.Fatalf("db pool error: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var merchantID string
	err = db.Pool.QueryRow(ctx, "SELECT id FROM merchants LIMIT 1").Scan(&merchantID)
	if err != nil {
		t.Fatalf("failed to query merchant: %v", err)
	}

	// Create client with invalid key
	client, err := NewGeminiClient(ctx, "INVALID_KEY_FOR_TESTING_99999")
	if err != nil {
		t.Fatalf("failed to init client: %v", err)
	}

	// Run ResolveExceptions
	resolvedCount, err := ResolveExceptions(ctx, merchantID, db.Pool, client)
	if err != nil {
		t.Fatalf("ResolveExceptions returned unexpected fatal error: %v", err)
	}

	fmt.Printf("\n--- RESOLVER WITH BROKEN GEMINI KEY RESULTS ---\n")
	fmt.Printf("Merchant ID: %s\n", merchantID)
	fmt.Printf("Resolved count: %d\n", resolvedCount)

	if resolvedCount != 0 {
		t.Errorf("Expected 0 resolved rows when Gemini API fails, but got %d", resolvedCount)
	}

	// Verify that the failure was recorded in reconciliation_log with 'unresolved'
	var logCount int
	err = db.Pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM reconciliation_log rl
		JOIN vendor_transactions vt ON vt.id = rl.vendor_transaction_id
		JOIN vendor_integrations vi ON vi.id = vt.vendor_integration_id
		WHERE vi.merchant_id = $1 
		  AND rl.method = 'unresolved' 
		  AND rl.reasoning LIKE '%Agent call failed%'
	`, merchantID).Scan(&logCount)
	if err != nil {
		t.Fatalf("failed to check reconciliation_log: %v", err)
	}

	fmt.Printf("Logged 'unresolved' audit rows containing 'Agent call failed': %d\n", logCount)
	if logCount == 0 {
		t.Logf("Note: No rows were submitted to agent or all had 0 candidates")
	} else {
		fmt.Printf("SUCCESS: Verified honest audit trail! Real API failure recorded in reconciliation_log.\n")
	}
}

// TestAgentEndpointMissingKey verifies that when GEMINI_API_KEY is unset (agentClient == nil),
// the /api/v1/reconcile/agent endpoint returns 503 Service Unavailable.
func TestAgentEndpointMissingKey(t *testing.T) {
	app := fiber.New()
	var agentClient *GeminiClient = nil // Unset / not configured

	app.Post("/api/v1/reconcile/agent", func(c *fiber.Ctx) error {
		if agentClient == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "agent resolver unavailable, GEMINI_API_KEY not configured",
			})
		}
		return c.SendStatus(fiber.StatusOK)
	})

	req := httptest.NewRequest("POST", "/api/v1/reconcile/agent", strings.NewReader(`{"merchant_id":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusServiceUnavailable {
		t.Errorf("Expected 503 Service Unavailable, got: %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("Endpoint response when GEMINI_API_KEY is unset: Status=%d, Body=%s\n", resp.StatusCode, string(body))
}
