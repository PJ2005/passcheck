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
	"passcheck/internal/demo"
	"passcheck/internal/reconciliation"

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

	// 3. Initialize Fiber App
	app := fiber.New(fiber.Config{
		DisableStartupMessage: false,
		AppName:               "PassCheck FIU API Service",
	})

	// Add Standard Middlewares
	app.Use(recover.New())
	app.Use(logger.New())

	// Serve static files
	app.Static("/", "./public")
	app.Static("/demo", "./public/demo.html")

	// 4. Expose routes
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

	app.Post("/api/v1/reconcile", func(c *fiber.Ctx) error {
		type ReconcileRequest struct {
			MerchantID string `json:"merchant_id"`
		}

		var req ReconcileRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "cannot parse json"})
		}

		if req.MerchantID == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "merchant_id is required"})
		}

		matches, err := reconciliation.RunDailyReconciliation(req.MerchantID, db.Pool)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "reconciliation failed", "details": err.Error()})
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"message": "Reconciliation complete",
			"matches_found": matches,
		})
	})

	// Demo Routes
	demoGroup := app.Group("/api/v1/demo")
	demoGroup.Get("/config", func(c *fiber.Ctx) error {
		var merchantID, providerID string
		err := db.Pool.QueryRow(context.Background(), "SELECT id FROM merchants LIMIT 1").Scan(&merchantID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "no merchants found"})
		}
		err = db.Pool.QueryRow(context.Background(), "SELECT id FROM vendor_integrations LIMIT 1").Scan(&providerID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "no vendor integrations found"})
		}
		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"merchant_id": merchantID,
			"provider_id": providerID,
		})
	})
	demoGroup.Post("/seed", func(c *fiber.Ctx) error {
		type SeedRequest struct {
			MerchantID string `json:"merchant_id"`
			ProviderID string `json:"provider_id"`
		}
		var req SeedRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid json"})
		}
		if req.MerchantID == "" || req.ProviderID == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "merchant_id and provider_id are required"})
		}

		err := demo.SeedMockRazorpayData(context.Background(), db.Pool, req.MerchantID, req.ProviderID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "seeding failed", "details": err.Error()})
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{"message": "Mock data seeded successfully!"})
	})
	demoGroup.Get("/dashboard/:merchantId", demo.GetReconciliationDashboard(db.Pool))

	// 5. Support Graceful Shutdown
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
