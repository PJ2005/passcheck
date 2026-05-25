package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"time"

	"passcheck/internal/database"
	"passcheck/internal/vendors"
	"passcheck/internal/vendors/razorpay"

	"github.com/joho/godotenv"
)

func main() {
	// 1. Setup Mock Razorpay Server
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/settlements" {
			w.Write([]byte(`{
				"items": [
					{
						"id": "setl_mock123",
						"status": "processed",
						"created_at": 1672531200
					}
				]
			}`))
			return
		}
		if r.URL.Path == "/v1/settlements/setl_mock123/recon" {
			w.Write([]byte(`{
				"items": [
					{
						"entity_id": "pay_mockABC",
						"type": "payment",
						"credit": 150000,
						"utr": "UTR_MOCK_RAZORPAY_1"
					},
					{
						"entity_id": "pay_mockDEF",
						"type": "payment",
						"credit": 250000,
						"utr": "UTR_MOCK_RAZORPAY_2"
					}
				]
			}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockServer.Close()

	// 2. Setup DB
	err := godotenv.Load(".env")
	if err != nil {
		log.Fatal("Error loading .env file")
	}

	db, err := database.NewConnectionPool()
	if err != nil {
		log.Fatalf("Failed to connect to DB: %v", err)
	}
	dbPool := db.Pool
	defer db.Close()

	// 3. Get a valid merchant ID from DB
	var merchantID string
	err = dbPool.QueryRow(context.Background(), "SELECT id FROM merchants LIMIT 1").Scan(&merchantID)
	if err != nil {
		log.Fatalf("No merchants found in DB")
	}

	// 4. Ensure Razorpay credentials exist for this merchant
	var integrationID string
	err = dbPool.QueryRow(context.Background(), `
		INSERT INTO vendor_integrations (merchant_id, vendor_name, encrypted_credentials, is_active)
		VALUES ($1, 'Razorpay', '{"key_id": "mock_id", "key_secret": "mock_secret"}', true)
		ON CONFLICT (merchant_id, vendor_name) DO UPDATE 
		SET encrypted_credentials = '{"key_id": "mock_id", "key_secret": "mock_secret"}'
		RETURNING id
	`, merchantID).Scan(&integrationID)
	if err != nil {
		log.Fatalf("Failed to ensure vendor credentials exist: %v", err)
	}

	// 5. Initialize the Razorpay provider pointing to the mock server
	provider := &razorpay.Provider{
		DB:      dbPool,
		BaseURL: mockServer.URL,
	}

	// 6. Test the Sync process
	ctx := context.Background()
	testDate := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	
	fmt.Println("Starting sync...")
	err = vendors.SyncVendorData(ctx, dbPool, provider, merchantID, testDate)
	if err != nil {
		log.Fatalf("Sync failed: %v", err)
	}
	
	fmt.Println("Checking database for inserted transactions...")
	rows, err := dbPool.Query(ctx, "SELECT vendor_txn_id, amount, utr_number FROM vendor_transactions WHERE vendor_integration_id = $1 AND vendor_name = 'Razorpay'", integrationID)
	// wait, vendor_name doesn't exist in vendor_transactions. It's inferred via vendor_integration_id.
	rows, err = dbPool.Query(ctx, "SELECT vendor_txn_id, amount, utr_number FROM vendor_transactions WHERE vendor_integration_id = $1", integrationID)
	
	if err != nil {
		log.Fatalf("Query failed: %v", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var id, utr string
		var amount float64
		rows.Scan(&id, &amount, &utr)
		fmt.Printf("Found DB Record -> ID: %s, Amount: %.2f, UTR: %s\n", id, amount, utr)
		count++
	}

	if count == 2 {
		fmt.Println("SUCCESS! The Razorpay adapter correctly pulled, parsed, and ingested the data.")
	} else {
		fmt.Printf("FAILED. Expected 2 records, found %d\n", count)
	}
}
