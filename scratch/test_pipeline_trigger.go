package main

import (
	"context"
	"log"
	"time"

	"passcheck/internal/database"
	"passcheck/internal/vendors"
	"passcheck/internal/vendors/phonepe"

	"github.com/joho/godotenv"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found")
	}

	db, err := database.NewConnectionPool()
	if err != nil {
		log.Fatalf("Critical error initializing database: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	var merchantID string
	err = db.Pool.QueryRow(ctx, "SELECT id FROM merchants LIMIT 1").Scan(&merchantID)
	if err != nil {
		log.Fatalf("No merchants found in db: %v", err)
	}

	ppProvider := &phonepe.Provider{DB: db.Pool}
	err = vendors.SyncVendorData(ctx, db.Pool, ppProvider, merchantID, time.Now())
	if err != nil {
		log.Fatalf("[Merchant %s] PhonePe sync failed: %v", merchantID, err)
	} else {
		log.Printf("[Merchant %s] PhonePe sync completed successfully", merchantID)
	}
}
