package razorpay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"passcheck/internal/vendors"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Provider implements the PaymentProvider interface for Razorpay
type Provider struct {
	DB      *pgxpool.Pool
	BaseURL string // Allows overriding for tests
}

// Ensure Provider implements PaymentProvider at compile time
var _ vendors.PaymentProvider = (*Provider)(nil)

type razorpaySettlement struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	CreatedAt int64  `json:"created_at"`
}

type razorpaySettlementsResponse struct {
	Items []razorpaySettlement `json:"items"`
}

type razorpayReconItem struct {
	EntityID string `json:"entity_id"`
	Type     string `json:"type"`
	Credit   int64  `json:"credit"` // Razorpay amounts are in paise
	UTR      string `json:"utr"`
}

type razorpayReconResponse struct {
	Items []razorpayReconItem `json:"items"`
}

// FetchSettlements hits the Razorpay API to fetch and standardize settlements.
func (p *Provider) FetchSettlements(ctx context.Context, merchantID string, date time.Time) ([]vendors.StandardVendorTxn, error) {
	// 1. Fetch credentials
	creds, err := vendors.GetVendorCredentials(ctx, p.DB, merchantID, "Razorpay")
	if err != nil {
		return nil, fmt.Errorf("failed to get razorpay credentials: %w", err)
	}

	keyID, ok1 := creds.Keys["key_id"].(string)
	keySecret, ok2 := creds.Keys["key_secret"].(string)
	if !ok1 || !ok2 {
		return nil, fmt.Errorf("invalid razorpay credentials format")
	}

	// 2. Fetch Settlements for the date (approximated by created_at timestamps)
	// For simplicity, we fetch recent settlements. In production, we'd use from/to query params.
	startOfDay := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, date.Location()).Unix()
	endOfDay := startOfDay + 86400

	baseURL := p.BaseURL
	if baseURL == "" {
		baseURL = "https://api.razorpay.com"
	}

	url := fmt.Sprintf("%s/v1/settlements?from=%d&to=%d", baseURL, startOfDay, endOfDay)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(keyID, keySecret)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch razorpay settlements: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("razorpay settlements API error (%d): %s", resp.StatusCode, string(bodyBytes))
	}

	var settlementsResp razorpaySettlementsResponse
	if err := json.NewDecoder(resp.Body).Decode(&settlementsResp); err != nil {
		return nil, fmt.Errorf("failed to decode razorpay settlements: %w", err)
	}

	var standardTxns []vendors.StandardVendorTxn

	// 3. For each settlement, fetch recon details to get the individual line items
	for _, settlement := range settlementsResp.Items {
		reconURL := fmt.Sprintf("%s/v1/settlements/%s/recon", baseURL, settlement.ID)
		reconReq, err := http.NewRequestWithContext(ctx, "GET", reconURL, nil)
		if err != nil {
			log.Printf("Failed to create recon request for settlement %s: %v", settlement.ID, err)
			continue
		}
		reconReq.SetBasicAuth(keyID, keySecret)

		reconResp, err := client.Do(reconReq)
		if err != nil {
			log.Printf("Failed to fetch recon for settlement %s: %v", settlement.ID, err)
			continue
		}

		if reconResp.StatusCode != http.StatusOK {
			reconResp.Body.Close()
			log.Printf("Recon API error for settlement %s: status %d", settlement.ID, reconResp.StatusCode)
			continue
		}

		var reconData razorpayReconResponse
		if err := json.NewDecoder(reconResp.Body).Decode(&reconData); err != nil {
			reconResp.Body.Close()
			log.Printf("Failed to decode recon for settlement %s: %v", settlement.ID, err)
			continue
		}
		reconResp.Body.Close()

		// 4. Map to standard structs
		settlementDate := time.Unix(settlement.CreatedAt, 0)
		for _, item := range reconData.Items {
			// Skip debits or fees if we only want credits (actual payments)
			if item.Type != "payment" && item.Type != "refund" {
				continue // Simplified for now, mostly we care about payments
			}

			amountInRupees := float64(item.Credit) / 100.0

			standardTxns = append(standardTxns, vendors.StandardVendorTxn{
				VendorTxnID:    item.EntityID,
				Amount:         amountInRupees,
				SettlementID:   settlement.ID,
				UTRNumber:      item.UTR,
				SettlementDate: settlementDate,
				VendorName:     "Razorpay",
			})
		}
	}

	return standardTxns, nil
}
