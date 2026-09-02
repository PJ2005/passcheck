package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"passcheck/internal/agent"
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

	// 3. Initialize Gemini agent client (optional). The deterministic
	// reconciliation endpoint must keep working without it: a missing or
	// invalid key only disables the agent pass, it never crashes the server.
	var agentClient *agent.GeminiClient
	geminiAPIKey := os.Getenv("GEMINI_API_KEY")
	if geminiAPIKey == "" {
		log.Println("GEMINI_API_KEY not set — agent-based exception resolution will be unavailable")
	} else if ac, initErr := agent.NewGeminiClient(context.Background(), geminiAPIKey); initErr != nil {
		log.Printf("Failed to initialize Gemini agent client (agent endpoint will return 503): %v", initErr)
	} else {
		agentClient = ac
		log.Printf("Gemini agent client initialized (model: %s)", agent.ModelName)
	}

	// 4. Initialize Fiber App
	app := fiber.New(fiber.Config{
		DisableStartupMessage: false,
		AppName:               "PassCheck FIU API Service",
	})

	// Add Standard Middlewares
	app.Use(recover.New())
	app.Use(logger.New())

	// Demo is the primary UI — serve it at both "/" and "/demo" so the
	// root URL opens the reconciliation dashboard directly (no separate
	// “API Tester” landing page). Static serving of the whole ./public
	// directory is intentionally removed to avoid exposing index.html.
	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendFile("./public/demo.html")
	})
	app.Get("/demo", func(c *fiber.Ctx) error {
		return c.SendFile("./public/demo.html")
	})

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

		result, err := reconciliation.RunDailyReconciliation(req.MerchantID, db.Pool)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "reconciliation failed", "details": err.Error()})
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"message": "Reconciliation complete",
			"result":  result,
		})
	})

	// Agent pass: sends whatever the deterministic engine left unresolved to
	// Gemini for judgment. Requires GEMINI_API_KEY at startup; otherwise this
	// route degrades to a clear 503 while the rest of the API keeps working.
	app.Post("/api/v1/reconcile/agent", func(c *fiber.Ctx) error {
		if agentClient == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "agent resolver unavailable, GEMINI_API_KEY not configured",
			})
		}

		type AgentRequest struct {
			MerchantID string `json:"merchant_id"`
		}
		var req AgentRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "cannot parse json"})
		}
		if req.MerchantID == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "merchant_id is required"})
		}

		resolved, err := agent.ResolveExceptions(context.Background(), req.MerchantID, db.Pool, agentClient)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "agent resolution failed", "details": err.Error()})
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"message":        "Agent resolution complete",
			"resolved_count": resolved,
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
	demoGroup.Get("/dashboard/:merchantId", demo.GetReconciliationDashboard(db.Pool))
	demoGroup.Get("/records/:merchantId", demo.GetReconciliationRecords(db.Pool))

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
