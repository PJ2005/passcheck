package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"passcheck/internal/database"
	"passcheck/internal/setu"
	"passcheck/internal/webhooks"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/joho/godotenv"
)

func main() {
	// 1. Load environment variables
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, reading configurations from OS environment variables")
	}

	// 2. Initialize Database connection pool
	db, err := database.NewConnectionPool()
	if err != nil {
		log.Fatalf("Critical error initializing database: %v", err)
	}
	defer db.Close()

	// 3. Initialize Setu Clients
	setuClientID := os.Getenv("SETU_KYC_CLIENT_ID")
	setuSecret := os.Getenv("SETU_KYC_CLIENT_SECRET")
	setuProductInstanceID := os.Getenv("SETU_KYC_PAN_PRODUCT_INSTANCE_ID")
	setuClient := setu.NewSetuClient(setuClientID, setuSecret, setuProductInstanceID)
	log.Printf("Initialized Setu KYC client (Client ID: %s)", setuClientID)

	aaClientID := os.Getenv("SETU_AA_CLIENT_ID")
	aaSecret := os.Getenv("SETU_AA_CLIENT_SECRET")
	aaProductInstanceID := os.Getenv("SETU_AA_PRODUCT_INSTANCE_ID")
	aaClient := setu.NewSetuClient(aaClientID, aaSecret, aaProductInstanceID)
	log.Printf("Initialized Setu AA client (Client ID: %s)", aaClientID)

	// 4. Initialize Fiber App
	app := fiber.New(fiber.Config{
		DisableStartupMessage: false,
		AppName:               "PassCheck FIU API Service",
	})

	// Add Standard Middlewares
	app.Use(recover.New())
	app.Use(logger.New())

	// Serve the UI Frontend
	app.Static("/", "./public")

	// 5. Expose routes
	app.Get("/api/v1/health", func(c *fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		dbStatus := "healthy"
		if err := db.Ping(ctx); err != nil {
			dbStatus = fmt.Sprintf("unhealthy: %v", err)
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"status":   "online",
			"database": dbStatus,
			"timestamp": time.Now().Format(time.RFC3339),
		})
	})

	// Add a debug/test route to trigger Setu token generation
	app.Post("/api/v1/debug/setu-token", func(c *fiber.Ctx) error {
		token, err := setuClient.GetToken()
		if err != nil {
			return c.Status(fiber.StatusFailedDependency).JSON(fiber.Map{
				"error":   "failed to generate or retrieve setu token",
				"details": err.Error(),
			})
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"token":      token,
			"cached_until": setuClient.AuthCache.GetTokenExpiry(), // helper if exposed, wait let's just output success
		})
	})

	// Onboarding Routes
	onboardGroup := app.Group("/api/v1/onboard")

	onboardGroup.Post("/pan", func(c *fiber.Ctx) error {
		type PANRequest struct {
			PhoneNumber string `json:"phone_number"`
			PAN         string `json:"pan"`
		}
		var req PANRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid json payload"})
		}

		kycResp, err := setuClient.VerifyPAN(req.PAN)
		if err != nil {
			return c.Status(fiber.StatusFailedDependency).JSON(fiber.Map{"error": "pan verification failed", "details": err.Error()})
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		status := "SUCCESS"
		if kycResp.Status != "success" {
			status = "FAILED"
		}

		var merchantID string
		err = db.Pool.QueryRow(ctx, `
			INSERT INTO merchants (phone_number, pan, pan_status, pan_registered_name)
			VALUES ($1, $2, $3, $4)
			RETURNING id
		`, req.PhoneNumber, req.PAN, status, kycResp.Data.FullName).Scan(&merchantID)

		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to save merchant to database", "details": err.Error()})
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"message":     "PAN verified and merchant created",
			"merchant_id": merchantID,
			"setu_status": kycResp.Status,
		})
	})

	onboardGroup.Post("/gst", func(c *fiber.Ctx) error {
		type GSTRequest struct {
			MerchantID string `json:"merchant_id"`
			GSTIN      string `json:"gstin"`
		}
		var req GSTRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid json payload"})
		}

		kycResp, err := setuClient.VerifyGST(req.GSTIN)
		if err != nil {
			return c.Status(fiber.StatusFailedDependency).JSON(fiber.Map{"error": "gst verification failed", "details": err.Error()})
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		status := "SUCCESS"
		if kycResp.Status != "success" {
			status = "FAILED"
		}

		_, err = db.Pool.Exec(ctx, `
			UPDATE merchants 
			SET gstin = $1, gst_status = $2, gst_registered_name = $3, updated_at = NOW()
			WHERE id = $4
		`, req.GSTIN, status, kycResp.Data.FullName, req.MerchantID)

		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to update merchant gst in database", "details": err.Error()})
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"message":     "GST verified and merchant updated",
			"setu_status": kycResp.Status,
		})
	})

	onboardGroup.Post("/bank", func(c *fiber.Ctx) error {
		type BankRequest struct {
			MerchantID string `json:"merchant_id"`
		}
		var req BankRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid json payload"})
		}

		rpdResp, err := setuClient.InitiateRPD(req.MerchantID)
		if err != nil {
			return c.Status(fiber.StatusFailedDependency).JSON(fiber.Map{"error": "rpd initiation failed", "details": err.Error()})
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		var bankAccountID string
		err = db.Pool.QueryRow(ctx, `
			INSERT INTO merchant_bank_accounts (merchant_id, rpd_request_id, rpd_status)
			VALUES ($1, $2, 'PENDING')
			RETURNING id
		`, req.MerchantID, rpdResp.ID).Scan(&bankAccountID)

		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to save bank account state to database", "details": err.Error()})
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"message":         "RPD initiated successfully",
			"bank_account_id": bankAccountID,
			"payment_link":    rpdResp.PaymentLink.ShortURL,
		})
	})

	// AA Consent Routes
	consentGroup := app.Group("/api/v1/consent")
	consentGroup.Post("/initiate", func(c *fiber.Ctx) error {
		type ConsentRequest struct {
			MerchantID string `json:"merchant_id"`
			VUA        string `json:"vua"`
			FromDate   string `json:"from_date"`
			ToDate     string `json:"to_date"`
		}
		var req ConsentRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid json payload"})
		}

		resp, err := aaClient.InitiateConsent(req.MerchantID, req.VUA, req.FromDate, req.ToDate)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to initiate consent", "details": err.Error()})
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_, err = db.Pool.Exec(ctx, `
			INSERT INTO aa_consents (merchant_id, setu_request_id, vua, status, valid_from, valid_until)
			VALUES ($1, $2, $3, 'PENDING', $4, $5)
		`, req.MerchantID, resp.ID, req.VUA, req.FromDate, req.ToDate)

		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to save consent state to database", "details": err.Error()})
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"message":      "Consent initiated",
			"redirect_url": resp.URL,
			"request_id":   resp.ID,
		})
	})

	consentGroup.Get("/status/:merchantId", func(c *fiber.Ctx) error {
		merchantID := c.Params("merchantId")
		
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		var status string
		var consentID *string
		err := db.Pool.QueryRow(ctx, `
			SELECT status, setu_consent_id FROM aa_consents
			WHERE merchant_id = $1
			ORDER BY created_at DESC LIMIT 1
		`, merchantID).Scan(&status, &consentID)

		if err != nil {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "no consent found for merchant"})
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"status": status,
			"consent_id": consentID,
		})
	})

	// Webhooks
	webhookGroup := app.Group("/api/v1/webhooks")
	webhookGroup.Post("/setu", webhooks.HandleConsentUpdate(db.Pool, aaClient))

	// 6. Support Graceful Shutdown
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Listen in a goroutine so it doesn't block OS signal handling
	go func() {
		log.Printf("Starting Fiber API server on port %s...", port)
		if err := app.Listen(fmt.Sprintf(":%s", port)); err != nil {
			log.Printf("Server shut down with error: %v", err)
		}
	}()

	// Channel to capture termination signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Block until a signal is received
	sig := <-sigChan
	log.Printf("Received system shutdown signal: %v. Initiating graceful shutdown...", sig)

	// Create shutdown timeout context
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	// Shutdown the Fiber application
	if err := app.ShutdownWithContext(shutdownCtx); err != nil {
		log.Printf("Error shutting down Fiber app: %v", err)
	}

	log.Println("Server gracefully terminated.")
}
