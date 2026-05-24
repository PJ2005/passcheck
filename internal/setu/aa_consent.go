package setu

import (
	"encoding/json"
	"fmt"
	"os"
)

// AAConsentResponse represents the response from creating a consent
type AAConsentResponse struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	URL         string `json:"url"`          // The Setu hosted portal URL
	RedirectURL string `json:"redirectUrl"`  // Our echoed redirectUrl
	CreatedAt   string `json:"createdAt"`
}

// AAConsentArtefactResponse represents the finalized consent artefact
type AAConsentArtefactResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	// Additional fields like Detail can be mapped if needed,
	// but we primarily care about fetching it to trigger the data session.
	Detail map[string]interface{} `json:"detail"`
}

// InitiateConsent creates a new AA consent request for a given merchant and user VUA
func (c *SetuClient) InitiateConsent(merchantID string, vua string, fromDate string, toDate string) (*AAConsentResponse, error) {
	url := "https://fiu-sandbox.setu.co/v2/consents"

	// Using the standard payload for Setu AA Consent
	payload := map[string]interface{}{
		"vua":          vua,
		"consentMode":  "STORE",
		"fetchType":    "PERIODIC",
		"consentTypes": []string{"TRANSACTIONS", "PROFILE"},
		"fiTypes":      []string{"DEPOSIT"},
		"dataLife": map[string]interface{}{
			"unit":  "MONTH",
			"value": 1,
		},
		"consentDuration": map[string]interface{}{
			"unit":  "MONTH",
			"value": 6,
		},
		"dataRange": map[string]interface{}{
			"from": fromDate,
			"to":   toDate,
		},
		"purpose": map[string]interface{}{
			"code":   "101",
			"refUri": "https://api.rebit.org.in/aa/purpose/101.xml",
			"text":   "Wealth management service",
			"category": map[string]string{
				"type": "string",
			},
		},
		"redirectUrl": "http://localhost:8080/onboarding/consent-success",
	}

	prodID := os.Getenv("SETU_AA_PRODUCT_INSTANCE_ID")
	bodyBytes, err := c.makeVerificationRequest("POST", url, payload, prodID)
	if err != nil {
		return nil, err
	}

	var resp AAConsentResponse
	if err := json.Unmarshal(bodyBytes, &resp); err != nil {
		return nil, fmt.Errorf("failed to decode json response: %w", err)
	}

	return &resp, nil
}

// GetConsentArtefact fetches a finalized consent artefact using the consent ID
func (c *SetuClient) GetConsentArtefact(consentID string) (*AAConsentArtefactResponse, error) {
	url := fmt.Sprintf("https://fiu-sandbox.setu.co/v2/consents/%s", consentID)
	
	prodID := os.Getenv("SETU_AA_PRODUCT_INSTANCE_ID")
	// HTTP GET does not need a payload
	bodyBytes, err := c.makeVerificationRequest("GET", url, nil, prodID)
	if err != nil {
		return nil, err
	}

	var resp AAConsentArtefactResponse
	if err := json.Unmarshal(bodyBytes, &resp); err != nil {
		return nil, fmt.Errorf("failed to decode json response: %w", err)
	}

	return &resp, nil
}
