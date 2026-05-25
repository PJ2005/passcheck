package setu

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AASessionResponse represents the response when creating a new data session
type AASessionResponse struct {
	ID string `json:"id"`
}

// ReBITFI represents the nested structure of Setu's FI data dump
type ReBITFI struct {
	FIPs []struct {
		Accounts []struct {
			Data struct {
				Account struct {
					Transactions struct {
						Transaction []struct {
							Type                 string `json:"type"`
							Amount               string `json:"amount"`
							Narration            string `json:"narration"`
							Reference            string `json:"reference"`
							TransactionTimestamp string `json:"transactionTimestamp"`
						} `json:"transaction"`
					} `json:"transactions"`
				} `json:"account"`
			} `json:"data"`
		} `json:"accounts"`
	} `json:"fips"`
}

// CreateDataSession creates a new FI data fetch session using an active consent
func (c *SetuClient) CreateDataSession(consentID string, fromDate string, toDate string) (*AASessionResponse, error) {
	url := "https://fiu-sandbox.setu.co/v2/sessions"

	payload := map[string]interface{}{
		"consentId": consentID,
		"dataRange": map[string]interface{}{
			"from": fromDate,
			"to":   toDate,
		},
		"format": "json",
	}

	prodID := os.Getenv("SETU_AA_PRODUCT_INSTANCE_ID")
	bodyBytes, err := c.makeVerificationRequest("POST", url, payload, prodID)
	if err != nil {
		return nil, err
	}

	var resp AASessionResponse
	if err := json.Unmarshal(bodyBytes, &resp); err != nil {
		return nil, fmt.Errorf("failed to decode json response: %w", err)
	}

	return &resp, nil
}

// FetchSessionData retrieves the finalized data payload for a specific session and parses it into the DB
func (c *SetuClient) FetchSessionData(sessionID string, db *pgxpool.Pool) error {
	url := fmt.Sprintf("https://fiu-sandbox.setu.co/v2/sessions/%s", sessionID)
	
	prodID := os.Getenv("SETU_AA_PRODUCT_INSTANCE_ID")
	// Using standard GET request for the session data
	bodyBytes, err := c.makeVerificationRequest("GET", url, nil, prodID)
	if err != nil {
		return fmt.Errorf("failed to fetch session data: %w", err)
	}

	// Dump raw for debugging
	log.Printf("==== RAW FI DATA DUMP FOR SESSION %s ====\n", sessionID)
	log.Println(string(bodyBytes))
	log.Println("==================================================")
	
	var rebitData ReBITFI
	if err := json.Unmarshal(bodyBytes, &rebitData); err != nil {
		return fmt.Errorf("failed to parse ReBIT FI JSON: %w", err)
	}

	// Very basic query to get the first merchant_bank_account ID since the mock data doesn't map directly
	var accountID string
	err = db.QueryRow(context.Background(), `SELECT id FROM merchant_bank_accounts LIMIT 1`).Scan(&accountID)
	if err != nil {
		// Create a dummy bank account if none exists
		var merchantID string
		db.QueryRow(context.Background(), `SELECT id FROM merchants LIMIT 1`).Scan(&merchantID)
		if merchantID != "" {
			err = db.QueryRow(context.Background(), `
				INSERT INTO merchant_bank_accounts (merchant_id, bank_name, account_number, ifsc_code) 
				VALUES ($1, 'Mock Bank', 'XXXXXXXX9774', 'MOCK0000123') RETURNING id`, merchantID).Scan(&accountID)
			if err != nil {
				return fmt.Errorf("failed to get or create merchant bank account: %w", err)
			}
		} else {
			return fmt.Errorf("no merchant found to link bank account to")
		}
	}

	insertedCount := 0
	for _, fip := range rebitData.FIPs {
		for _, account := range fip.Accounts {
			for _, txn := range account.Data.Account.Transactions.Transaction {
				_, err := db.Exec(context.Background(), `
					INSERT INTO bank_transactions (bank_account_id, amount, txn_type, narration, utr_number, txn_date)
					VALUES ($1, $2, $3, $4, $5, $6)
				`, accountID, txn.Amount, txn.Type, txn.Narration, txn.Reference, txn.TransactionTimestamp)
				
				if err != nil {
					log.Printf("Failed to insert bank transaction (UTR: %s): %v", txn.Reference, err)
					continue
				}
				insertedCount++
			}
		}
	}

	log.Printf("Successfully ingested %d bank transactions into the ledger for session %s!", insertedCount, sessionID)
	return nil
}
