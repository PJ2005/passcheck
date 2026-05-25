package webhooks

import (
	"context"
	"log"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"passcheck/internal/reconciliation"
	"passcheck/internal/setu"
)

// SetuWebhookPayload represents the incoming JSON for Setu webhooks
type SetuWebhookPayload struct {
	Type          string `json:"type"`
	ConsentID     string `json:"consentId"`
	DataSessionID string `json:"dataSessionId"`
	Timestamp     string `json:"timestamp"`
	Success       bool   `json:"success"`
	Data      *struct {
		Status               string `json:"status"`
		SessionID            string `json:"sessionId"`
		SessionStatusDetails struct {
			SessionID string `json:"sessionId"`
		} `json:"sessionStatusDetails"`
	} `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// HandleConsentUpdate processes the async webhook sent by Setu when an AA consent changes status
func HandleConsentUpdate(db *pgxpool.Pool, aaClient *setu.SetuClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		// Log raw payload for debugging
		log.Printf("Raw webhook payload: %s", string(c.Body()))

		var payload SetuWebhookPayload
		if err := c.BodyParser(&payload); err != nil {
			log.Printf("Failed to parse webhook payload: %v", err)
			return c.SendStatus(fiber.StatusBadRequest)
		}

		if !payload.Success {
			log.Printf("Webhook reported failure: %v", payload.Error)
			return c.SendStatus(fiber.StatusOK)
		}

		status := ""
		sessionID := payload.DataSessionID // Start with the root field as requested

		if payload.Data != nil {
			status = payload.Data.Status
			
			// Try both common locations for the session ID in Setu payloads if root field is empty
			if sessionID == "" {
				if payload.Data.SessionID != "" {
					sessionID = payload.Data.SessionID
				} else if payload.Data.SessionStatusDetails.SessionID != "" {
					sessionID = payload.Data.SessionStatusDetails.SessionID
				}
			}
		}

		if payload.Type == "FI_STATUS_UPDATE" || payload.Type == "SESSION_STATUS_UPDATE" {
			if sessionID == "" {
				log.Printf("Failed to parse session ID from payload")
				return c.SendStatus(fiber.StatusBadRequest)
			}

			log.Printf("Received FI data update for session %s: %s (Type: %s)", sessionID, status, payload.Type)
			if status == "READY" || status == "COMPLETED" || status == "PARTIAL" {
				_, err := db.Exec(context.Background(), `
					UPDATE aa_data_sessions 
					SET status = 'COMPLETED', completed_at = NOW() 
					WHERE setu_session_id = $1
				`, sessionID)

				if err != nil {
					log.Printf("Failed to update session status in DB: %v", err)
					return c.SendStatus(fiber.StatusInternalServerError)
				}

				go func(sid string) {
					log.Printf("Fetching FI data for session %s", sid)
					if err := aaClient.FetchSessionData(sid, db); err != nil {
						log.Printf("Failed to fetch session data: %v", err)
					} else {
						// Trigger Reconciliation
						var merchantID string
						err := db.QueryRow(context.Background(), "SELECT merchant_id FROM aa_data_sessions WHERE setu_session_id = $1", sid).Scan(&merchantID)
						if err != nil {
							log.Printf("Reconciliation Trigger Failed: Could not find merchant for session %s: %v", sid, err)
							return
						}

						log.Printf("Bank data successfully ingested! Firing Reconciliation Engine for merchant %s...", merchantID)
						matches, err := reconciliation.RunDailyReconciliation(merchantID, db)
						if err != nil {
							log.Printf("Reconciliation Engine error: %v", err)
						} else {
							log.Printf("Reconciliation Engine completed automatically! Found %d new matches.", matches)
						}
					}
				}(sessionID)
			}
			return c.SendStatus(fiber.StatusOK)
		}

		if payload.Type == "CONSENT_STATUS_UPDATE" {
			log.Printf("Received consent update for %s: %s", payload.ConsentID, status)

			if status == "ACTIVE" {
				// Update the database to map the final consent ID and mark as ACTIVE
				_, err := db.Exec(context.Background(), `
					UPDATE aa_consents 
					SET status = 'ACTIVE', setu_consent_id = $1 
					WHERE setu_request_id = $1 OR setu_consent_id = $1
				`, payload.ConsentID)

				if err != nil {
					log.Printf("Failed to update consent status in DB: %v", err)
					return c.SendStatus(fiber.StatusInternalServerError)
				}

				// Fire goroutine to fetch the artefact asynchronously
				go func(consentID string) {
					log.Printf("Fetching consent artefact for active consent %s", consentID)
					artefact, err := aaClient.GetConsentArtefact(consentID)
					if err != nil {
						log.Printf("Failed to fetch consent artefact for %s: %v", consentID, err)
						return
					}
					log.Printf("Successfully fetched consent artefact for %s. Ready for data sessions.", artefact.ID)
				}(payload.ConsentID)
			}
			return c.SendStatus(fiber.StatusOK)
		}

		log.Printf("Ignoring webhook type: %s", payload.Type)
		return c.SendStatus(fiber.StatusOK)
	}
}
