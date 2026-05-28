package cron

import (
	"context"
	"log"
	"time"

	"passcheck/internal/setu"
	"passcheck/internal/vendors"
	"passcheck/internal/vendors/phonepe"
	"passcheck/internal/vendors/razorpay"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RunDailyPipeline orchestrates the entire daily data synchronization and reconciliation pipeline.
func RunDailyPipeline(ctx context.Context, db *pgxpool.Pool, aaClient *setu.SetuClient, targetDate time.Time) {
	log.Printf("Starting Daily Reconciliation Pipeline for date: %s", targetDate.Format("2006-01-02"))

	// 1. Fetch all merchants
	rows, err := db.Query(ctx, "SELECT id FROM merchants")
	if err != nil {
		log.Printf("Pipeline Error: Failed to fetch active merchants: %v", err)
		return
	}
	defer rows.Close()

	var merchants []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			merchants = append(merchants, id)
		}
	}

	// 2. Process each merchant
	for _, merchantID := range merchants {
		go processMerchant(db, aaClient, merchantID, targetDate)
	}
}

func processMerchant(db *pgxpool.Pool, aaClient *setu.SetuClient, merchantID string, targetDate time.Time) {
	ctx := context.Background() // Use background ctx for async operations

	log.Printf("[Merchant %s] Starting sync pipeline", merchantID)

	// --- VENDOR SYNC ---
	// Run for Razorpay
	rzpProvider := &razorpay.Provider{DB: db}
	err := vendors.SyncVendorData(ctx, db, rzpProvider, merchantID, targetDate)
	if err != nil {
		log.Printf("[Merchant %s] Razorpay sync failed: %v", merchantID, err)
		// We continue to bank sync even if vendor sync fails
	} else {
		log.Printf("[Merchant %s] Razorpay sync completed successfully", merchantID)
	}

	// Run for PhonePe
	ppProvider := &phonepe.Provider{DB: db}
	err = vendors.SyncVendorData(ctx, db, ppProvider, merchantID, targetDate)
	if err != nil {
		log.Printf("[Merchant %s] PhonePe sync failed: %v", merchantID, err)
	} else {
		log.Printf("[Merchant %s] PhonePe sync completed successfully", merchantID)
	}

	// --- BANK SYNC (Setu AA) ---
	var consentID string
	var validUntil time.Time
	err = db.QueryRow(ctx, `
		SELECT setu_consent_id, valid_until FROM aa_consents 
		WHERE merchant_id = $1 AND status = 'ACTIVE' 
		LIMIT 1
	`, merchantID).Scan(&consentID, &validUntil)

	if err != nil {
		log.Printf("[Merchant %s] No active bank consent found. Skipping bank sync.", merchantID)
		return
	}

	// Calculate date range (start to end of the target day)
	startOfDay := time.Date(targetDate.Year(), targetDate.Month(), targetDate.Day(), 0, 0, 0, 0, targetDate.Location())
	endOfDay := startOfDay.Add(24 * time.Hour).Add(-time.Second)

	// Setu rejects requests if the data range exceeds the consent's approved FIDataRange. Cap it.
	if endOfDay.After(validUntil) {
		endOfDay = validUntil
	}
	if startOfDay.After(endOfDay) {
		startOfDay = endOfDay.Add(-24 * time.Hour)
	}

	fromDate := startOfDay.Format(time.RFC3339)
	toDate := endOfDay.Format(time.RFC3339)

	log.Printf("[Merchant %s] Requesting FI data from %s to %s", merchantID, fromDate, toDate)
	
	sessionResp, err := aaClient.CreateDataSession(consentID, fromDate, toDate)
	if err != nil {
		log.Printf("[Merchant %s] Failed to create data session: %v", merchantID, err)
		return
	}

	// Record the new data session
	_, err = db.Exec(ctx, `
		INSERT INTO aa_data_sessions (merchant_id, setu_session_id, status)
		VALUES ($1, $2, 'INITIATED')
	`, merchantID, sessionResp.ID)

	if err != nil {
		log.Printf("[Merchant %s] Failed to record data session in DB: %v", merchantID, err)
		return
	}

	log.Printf("[Merchant %s] Bank data session %s successfully initiated. Waiting for async webhook...", merchantID, sessionResp.ID)
	// The pipeline halts here. The webhook will pick up the completion and trigger the Reconciliation Engine.
}
