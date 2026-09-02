package setu

import (
	"net/http"
	"time"
)

// SetuClient handles the base configuration and HTTP requests to Setu APIs
type SetuClient struct {
	ClientID          string
	Secret            string
	ProductInstanceID string
	HTTPClient        *http.Client
	AuthCache         *AuthCache
}

// NewSetuClient initializes a new SetuClient with credentials and standard HTTP configuration
func NewSetuClient(clientID, secret, productInstanceID string) *SetuClient {
	return &SetuClient{
		ClientID:          clientID,
		Secret:            secret,
		ProductInstanceID: productInstanceID,
		HTTPClient: &http.Client{
			Timeout: 15 * time.Second,
		},
		AuthCache: NewAuthCache(),
	}
}
