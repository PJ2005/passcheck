package database

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DB represents the database connection pool wrapper
type DB struct {
	Pool *pgxpool.Pool
}

// NewConnectionPool creates a new connection pool based on environment variables
func NewConnectionPool() (*DB, error) {
	dbHost := os.Getenv("DB_HOST")
	dbPort := os.Getenv("DB_PORT")
	dbUser := os.Getenv("DB_USER")
	dbPass := os.Getenv("DB_PASSWORD")
	dbName := os.Getenv("DB_NAME")
	dbSSLMode := os.Getenv("DB_SSLMODE")

	if dbSSLMode == "" {
		dbSSLMode = "disable"
	}

	if dbHost == "" || dbPort == "" || dbUser == "" || dbPass == "" || dbName == "" {
		return nil, fmt.Errorf("missing required database environment variables")
	}

	dsn := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s",
		dbUser, dbPass, dbHost, dbPort, dbName, dbSSLMode)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to parse connection config: %w", err)
	}

	// Configure pool parameters
	config.MaxConns = 10
	config.MinConns = 2
	config.MaxConnLifetime = 30 * time.Minute
	config.MaxConnIdleTime = 15 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("failed to create database pool: %w", err)
	}

	// Ping database to verify connection
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	log.Printf("Successfully established database connection pool on %s:%s/%s", dbHost, dbPort, dbName)

	return &DB{Pool: pool}, nil
}

// Close gracefully closes all database connections in the pool
func (db *DB) Close() {
	if db.Pool != nil {
		db.Pool.Close()
		log.Println("Database connection pool closed successfully")
	}
}

// Ping pings the database with a context timeout
func (db *DB) Ping(ctx context.Context) error {
	if db.Pool == nil {
		return fmt.Errorf("database pool is not initialized")
	}
	return db.Pool.Ping(ctx)
}
