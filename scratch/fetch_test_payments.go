package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	"github.com/joho/godotenv"
)

type PaymentResponse struct {
	Items []struct {
		ID     string `json:"id"`
		Amount int    `json:"amount"` // in paise
		Status string `json:"status"`
		Email  string `json:"email"`
	} `json:"items"`
}

func main() {
	godotenv.Load(".env")

	keyID := os.Getenv("RAZORPAY_KEY_ID")
	keySecret := os.Getenv("RAZORPAY_SECRET")

	if keyID == "" || keySecret == "" {
		log.Fatalf("Missing Razorpay API keys in .env")
	}

	req, err := http.NewRequest("GET", "https://api.razorpay.com/v1/payments", nil)
	if err != nil {
		log.Fatalf("Error creating request: %v", err)
	}
	req.SetBasicAuth(keyID, keySecret)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		log.Fatalf("Error executing request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		log.Fatalf("Razorpay API error: %s", string(bodyBytes))
	}

	var data PaymentResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		log.Fatalf("Error decoding JSON: %v", err)
	}

	fmt.Println("Successfully connected to Razorpay Test Account!")
	fmt.Printf("Found %d recent payments:\n", len(data.Items))
	
	for _, payment := range data.Items {
		amountInRupees := payment.Amount / 100
		fmt.Printf("- Payment ID: %s | Amount: ₹%d | Status: %s | Email: %s\n", payment.ID, amountInRupees, payment.Status, payment.Email)
	}
}
