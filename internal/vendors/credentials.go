package vendors

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// VendorCredentials holds the raw parsed JSON credentials and the database ID of the integration.
type VendorCredentials struct {
	IntegrationID string
	Keys          map[string]interface{}
}

// GetVendorCredentials fetches the API keys for a specific merchant and vendor.
func GetVendorCredentials(ctx context.Context, db *pgxpool.Pool, merchantID string, vendorName string) (*VendorCredentials, error) {
	var integrationID string
	var encryptedCreds string // Currently stored as plain JSON string, will be encrypted in future phases

	err := db.QueryRow(ctx, `
		SELECT id, encrypted_credentials 
		FROM vendor_integrations 
		WHERE merchant_id = $1 AND vendor_name = $2 AND is_active = TRUE
	`, merchantID, vendorName).Scan(&integrationID, &encryptedCreds)

	if err != nil {
		return nil, fmt.Errorf("failed to retrieve vendor credentials: %w", err)
	}

	var keys map[string]interface{}
	if err := json.Unmarshal([]byte(encryptedCreds), &keys); err != nil {
		return nil, fmt.Errorf("failed to parse vendor credentials JSON: %w", err)
	}

	return &VendorCredentials{
		IntegrationID: integrationID,
		Keys:          keys,
	}, nil
}
