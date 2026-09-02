package setu

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

// AuthCache is a thread-safe in-memory cache for the authentication token
type AuthCache struct {
	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// NewAuthCache creates and initializes a new AuthCache
func NewAuthCache() *AuthCache {
	return &AuthCache{}
}

// loginRequest is the payload sent to Setu's login API.
// We support both clientId and clientID cases to ensure compatibility.
type loginRequest struct {
	ClientID  string `json:"clientId,omitempty"`
	ClientID2 string `json:"clientID,omitempty"`
	Secret    string `json:"secret"`
	GrantType string `json:"grant_type,omitempty"`
}

// loginResponseData holds token details inside structured responses
type loginResponseData struct {
	Token     string `json:"token"`
	ExpiresIn int64  `json:"expiresIn"`
}

// loginResponseWrapper represents the nested JSON response format from Setu APIs
type loginResponseWrapper struct {
	Success bool              `json:"success"`
	Data    loginResponseData `json:"data"`
}

// loginResponseDirect represents the flat JSON response format
type loginResponseDirect struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expiresIn"`
	ExpiresIn2  int64  `json:"expires_in"`
}

// GenerateAuthToken directly hits the Setu login endpoint to generate a new authentication JWT.
func (c *SetuClient) GenerateAuthToken() (string, int64, error) {
	loginURL := "https://orgservice-prod.setu.co/v1/users/login"
	log.Printf("Initiating authentication request to Setu at %s", loginURL)

	payload := loginRequest{
		ClientID:  c.ClientID,
		ClientID2: c.ClientID,
		Secret:    c.Secret,
		GrantType: "client_credentials",
	}

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return "", 0, fmt.Errorf("failed to marshal login payload: %w", err)
	}

	req, err := http.NewRequest("POST", loginURL, bytes.NewBuffer(jsonPayload))
	if err != nil {
		return "", 0, fmt.Errorf("failed to create login request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	// Setu occasionally requires a client header indicating bridge/fiu context
	req.Header.Set("client", "bridge")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("http login request failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, fmt.Errorf("failed to read login response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", 0, fmt.Errorf("login failed with status code %d, body: %s", resp.StatusCode, string(bodyBytes))
	}

	// Try parsing nested response first
	var wrapper loginResponseWrapper
	if err := json.Unmarshal(bodyBytes, &wrapper); err == nil && wrapper.Data.Token != "" {
		expiresIn := wrapper.Data.ExpiresIn
		if expiresIn <= 0 {
			expiresIn = 300 // default to 5 minutes if not specified
		}
		return wrapper.Data.Token, expiresIn, nil
	}

	// Fallback to flat response
	var direct loginResponseDirect
	if err := json.Unmarshal(bodyBytes, &direct); err == nil {
		token := direct.Token
		if token == "" {
			token = direct.AccessToken
		}

		if token != "" {
			expiresIn := direct.ExpiresIn
			if expiresIn <= 0 {
				expiresIn = direct.ExpiresIn2
			}
			if expiresIn <= 0 {
				// We can parse the JWT to get the actual expiry, but defaulting to 5 mins is safer
				expiresIn = 300 
			}
			return token, expiresIn, nil
		}
	}

	return "", 0, fmt.Errorf("failed to parse login response token from body: %s", string(bodyBytes))
}

// GetToken returns a valid cached authentication token, or fetches a new one if expired/missing.
// This implementation is thread-safe using a sync.Mutex.
func (c *SetuClient) GetToken() (string, error) {
	c.AuthCache.mu.Lock()
	defer c.AuthCache.mu.Unlock()

	now := time.Now()
	// Check if token exists and is valid (with a 15-second grace window to prevent boundary failures)
	if c.AuthCache.token != "" && now.Before(c.AuthCache.expiresAt) {
		return c.AuthCache.token, nil
	}

	log.Println("Cached token expired or missing. Fetching a new token...")
	token, expiresIn, err := c.GenerateAuthToken()
	if err != nil {
		return "", err
	}

	c.AuthCache.token = token
	// Cache expiration is adjusted with a 15-second safety buffer
	c.AuthCache.expiresAt = now.Add(time.Duration(expiresIn) * time.Second).Add(-15 * time.Second)
	log.Printf("Token cached successfully. Expires at: %s", c.AuthCache.expiresAt.Format(time.RFC3339))

	return token, nil
}

// GetTokenExpiry returns the expiry time of the cached token in a thread-safe manner.
func (ac *AuthCache) GetTokenExpiry() time.Time {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	return ac.expiresAt
}

