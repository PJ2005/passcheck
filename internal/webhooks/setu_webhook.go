package webhooks

import (
	"context"
	"log"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"passcheck/internal/setu"
)

// SetuWebhookPayload represents the incoming JSON for Setu webhooks
type SetuWebhookPayload struct {
	Type      string `json:"type"`
	ConsentID string `json:"consentId"`
	Timestamp string `json:"timestamp"`
	Success   bool   `json:"success"`
	Data      *struct {
		Status string `json:"status"`
	} `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// HandleConsentUpdate processes the async webhook sent by Setu when an AA consent changes status
func HandleConsentUpdate(db *pgxpool.Pool, aaClient *setu.SetuClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var payload SetuWebhookPayload
		if err := c.BodyParser(&payload); err != nil {
			log.Printf("Failed to parse webhook payload: %v", err)
			return c.SendStatus(fiber.StatusBadRequest)
		}

		if payload.Type != "CONSENT_STATUS_UPDATE" {
			log.Printf("Ignoring webhook type: %s", payload.Type)
			return c.SendStatus(fiber.StatusOK)
		}

		if !payload.Success {
			log.Printf("Webhook reported failure for consent %s: %v", payload.ConsentID, payload.Error)
			// Optionally update DB to REJECTED or FAILED here
			return c.SendStatus(fiber.StatusOK)
		}

		status := ""
		if payload.Data != nil {
			status = payload.Data.Status
		}

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
}
