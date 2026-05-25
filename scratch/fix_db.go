package main

import (
	"context"
	"log"

	"passcheck/internal/database"

	"github.com/joho/godotenv"
)

func main() {
	godotenv.Load(".env")
	db, err := database.NewConnectionPool()
	if err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}
	defer db.Close()

	_, err = db.Pool.Exec(context.Background(), `
		UPDATE vendor_integrations 
		SET encrypted_credentials = $1
		WHERE merchant_id = '17e84418-58bf-4675-ba64-b37966e1307e'
	`, `{"key_id": "rzp_test_StVk5QcAM4YLbW", "key_secret": "mERHvYNny2Fc1KffgW0iK1hO"}`)
	
	if err != nil {
		log.Fatalf("Failed to update: %v", err)
	}
	log.Println("Successfully updated DB with proper JSON")
}
