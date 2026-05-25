package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"passcheck/internal/database"
	"passcheck/internal/vendors"
	"passcheck/internal/vendors/razorpay"

	"github.com/joho/godotenv"
)

func main() {
	// 1. Setup DB
	err := godotenv.Load(".env")
	if err != nil {
		log.Println("Warning: Error loading .env file, continuing with system vars")
	}

	db, err := database.NewConnectionPool()
	if err != nil {
		log.Fatalf("Failed to connect to DB: %v", err)
	}
	dbPool := db.Pool
	defer db.Close()

	// 2. Use the exact merchant ID the user is working with
	merchantID := "17e84418-58bf-4675-ba64-b37966e1307e"
	fmt.Printf("Using active Razorpay integration for Merchant: %s\n", merchantID)

	// 3. Initialize the real Razorpay provider
	provider := &razorpay.Provider{
		DB: dbPool,
		// Not setting BaseURL so it defaults to the live Razorpay API
	}

	// 4. Test the Sync process for the last 7 days to ensure we catch the test settlement
	ctx := context.Background()
	now := time.Now()

	fmt.Println("Starting Live Razorpay Sync...")
	
	totalFound := 0
	for i := 0; i < 7; i++ {
		testDate := now.AddDate(0, 0, -i)
		fmt.Printf("Fetching settlements for %s...\n", testDate.Format("2006-01-02"))
		
		err = vendors.SyncVendorData(ctx, dbPool, provider, merchantID, testDate)
		if err != nil {
			log.Printf("Sync failed for date %s: %v", testDate.Format("2006-01-02"), err)
			continue
		}
	}

	fmt.Println("Finished sync process. Querying database for inserted vendor_transactions...")
	rows, err := dbPool.Query(ctx, `
		SELECT vendor_txn_id, amount, utr_number, settlement_date 
		FROM vendor_transactions 
		WHERE vendor_integration_id = (
			SELECT id FROM vendor_integrations WHERE merchant_id = $1 AND vendor_name = 'Razorpay' LIMIT 1
		) AND vendor_txn_id NOT LIKE 'MOCK_%'
	`, merchantID)
	
	if err != nil {
		log.Fatalf("Query failed: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id, utr string
		var amount float64
		var date time.Time
		rows.Scan(&id, &amount, &utr, &date)
		fmt.Printf("Found DB Record -> ID: %s, Amount: %.2f INR, UTR: %s, Date: %s\n", id, amount, utr, date.Format("2006-01-02"))
		totalFound++
	}

	if totalFound > 0 {
		fmt.Printf("SUCCESS! Successfully pulled and ingested %d live Razorpay transactions!\n", totalFound)
	} else {
		fmt.Println("No live transactions found in the last 7 days. If you just created the test payment, Razorpay might not have generated a settlement for it yet. Razorpay only returns data via this API *after* they settle the batch to your bank account.")
	}
}
