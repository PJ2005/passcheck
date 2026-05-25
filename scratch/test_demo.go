package main

import (
	"context"
	"fmt"
	"log"
	"bytes"
	"io"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

func main() {
	godotenv.Load(".env")
	dbUrl := "postgres://postgres:postgres@localhost:5436/passcheck_db"
	pool, err := pgxpool.New(context.Background(), dbUrl)
	if err != nil {
		log.Fatalf("db error: %v", err)
	}
	defer pool.Close()

	var merchantID, providerID string
	err = pool.QueryRow(context.Background(), "SELECT id FROM merchants LIMIT 1").Scan(&merchantID)
	if err != nil {
		log.Fatalf("no merchants: %v", err)
	}

	err = pool.QueryRow(context.Background(), "SELECT id FROM vendor_integrations LIMIT 1").Scan(&providerID)
	if err != nil {
		// insert a dummy provider if none exist
		err = pool.QueryRow(context.Background(), "INSERT INTO vendor_integrations (merchant_id, vendor_name, encrypted_credentials) VALUES ($1, 'Razorpay', '{}') RETURNING id", merchantID).Scan(&providerID)
		if err != nil {
			log.Fatalf("no providers: %v", err)
		}
	}

	// Hit Seeder
	seedPayload := fmt.Sprintf(`{"merchant_id": "%s", "provider_id": "%s"}`, merchantID, providerID)
	resp, err := http.Post("http://localhost:8080/api/v1/demo/seed", "application/json", bytes.NewBuffer([]byte(seedPayload)))
	if err != nil {
		log.Fatalf("seed request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("Seed Response: %s\n", string(body))

	// Hit Dashboard
	resp2, err := http.Get(fmt.Sprintf("http://localhost:8080/api/v1/demo/dashboard/%s", merchantID))
	if err != nil {
		log.Fatalf("dashboard request failed: %v", err)
	}
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)
	fmt.Printf("Dashboard Before Recon: %s\n", string(body2))
	
	// Trigger Recon
	reconPayload := fmt.Sprintf(`{"merchant_id": "%s"}`, merchantID)
	resp3, err := http.Post("http://localhost:8080/api/v1/reconcile", "application/json", bytes.NewBuffer([]byte(reconPayload)))
	if err != nil {
		log.Fatalf("recon request failed: %v", err)
	}
	defer resp3.Body.Close()
	body3, _ := io.ReadAll(resp3.Body)
	fmt.Printf("Recon Response: %s\n", string(body3))

	// Hit Dashboard again
	resp4, err := http.Get(fmt.Sprintf("http://localhost:8080/api/v1/demo/dashboard/%s", merchantID))
	if err != nil {
		log.Fatalf("dashboard request failed: %v", err)
	}
	defer resp4.Body.Close()
	body4, _ := io.ReadAll(resp4.Body)
	fmt.Printf("Dashboard After Recon: %s\n", string(body4))
}
