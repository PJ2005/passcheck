package setu

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

// KYCResponse represents the standard response structure for PAN/GST verification.
type KYCResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"` // e.g., "success", "failed"
	Data   struct {
		FullName string `json:"fullName"`
	} `json:"data"`
}

// RPDResponse represents the standard response structure for Reverse Penny Drop
type RPDResponse struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	PaymentLink struct {
		ShortURL string `json:"shortUrl"`
	} `json:"paymentLink"`
}

func (c *SetuClient) makeVerificationRequest(method, url string, payload interface{}, productInstanceID string) ([]byte, error) {
	var bodyReader io.Reader
	if payload != nil {
		jsonPayload, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal payload: %w", err)
		}
		bodyReader = bytes.NewBuffer(jsonPayload)
	}

	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Fetch token from our thread-safe token manager using the KYC credentials
	token, err := c.GetToken()
	if err != nil {
		return nil, fmt.Errorf("failed to generate auth token: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-client-id", c.ClientID)
	req.Header.Set("x-client-secret", c.Secret)
	req.Header.Set("x-product-instance-id", productInstanceID)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		return nil, fmt.Errorf("verification failed with status code %d, body: %s", resp.StatusCode, string(bodyBytes))
	}

	return bodyBytes, nil
}

// VerifyPAN calls the Setu DG Sandbox API to verify a PAN number.
func (c *SetuClient) VerifyPAN(pan string) (*KYCResponse, error) {
	url := "https://dg-sandbox.setu.co/api/verify/pan"
	payload := map[string]string{
		"pan":     pan,
		"consent": "Y",
		"reason":  "Merchant Onboarding Verification",
	}

	prodID := os.Getenv("SETU_KYC_PAN_PRODUCT_INSTANCE_ID")
	bodyBytes, err := c.makeVerificationRequest("POST", url, payload, prodID)
	if err != nil {
		return nil, err
	}

	var kycResp KYCResponse
	if err := json.Unmarshal(bodyBytes, &kycResp); err != nil {
		return nil, fmt.Errorf("failed to decode json response: %w", err)
	}

	return &kycResp, nil
}

// VerifyGST calls the Setu DG Sandbox API to verify a GSTIN.
func (c *SetuClient) VerifyGST(gstin string) (*KYCResponse, error) {
	url := "https://dg-sandbox.setu.co/api/verify/gst"
	payload := map[string]string{
		"gstin":   gstin,
		"consent": "Y",
		"reason":  "Merchant Onboarding Verification",
	}

	prodID := os.Getenv("SETU_KYC_GSTIN_PRODUCT_INSTANCE_ID")
	bodyBytes, err := c.makeVerificationRequest("POST", url, payload, prodID)
	if err != nil {
		return nil, err
	}

	var kycResp KYCResponse
	if err := json.Unmarshal(bodyBytes, &kycResp); err != nil {
		return nil, fmt.Errorf("failed to decode json response: %w", err)
	}

	return &kycResp, nil
}

// InitiateRPD initiates a Reverse Penny Drop to verify a bank account.
func (c *SetuClient) InitiateRPD(merchantID string) (*RPDResponse, error) {
	url := "https://dg-sandbox.setu.co/api/verify/ban/reverse"
	
	// Production-grade payload: includes redirection for the user and additionalData for webhook tracking
	payload := map[string]interface{}{
		"redirectionConfig": map[string]interface{}{
			"redirectUrl": "https://passcheck.fiu/onboarding/bank-success",
			"timeout":     30,
		},
		"additionalData": map[string]string{
			"merchant_id": merchantID,
		},
	}

	// Use the dedicated RPD Product Instance ID
	prodID := os.Getenv("SETU_KYC_RPD_PRODUCT_INSTANCE_ID")
	bodyBytes, err := c.makeVerificationRequest("POST", url, payload, prodID)
	if err != nil {
		return nil, err
	}

	var rpdResp RPDResponse
	if err := json.Unmarshal(bodyBytes, &rpdResp); err != nil {
		return nil, fmt.Errorf("failed to decode json response: %w", err)
	}

	return &rpdResp, nil
}
