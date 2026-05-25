package main

import (
	"context"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	dbUrl := "postgres://postgres:postgres@localhost:5436/passcheck_db"
	pool, err := pgxpool.New(context.Background(), dbUrl)
	if err != nil {
		log.Fatalf("db error: %v", err)
	}
	defer pool.Close()

	_, err = pool.Exec(context.Background(), "DELETE FROM reconciled_matches;")
	fmt.Printf("Deleted matches: %v\n", err)

	_, err = pool.Exec(context.Background(), "DELETE FROM vendor_transactions;")
	fmt.Printf("Deleted vendors: %v\n", err)

	var count int
	err = pool.QueryRow(context.Background(), "SELECT count(*) FROM bank_transactions WHERE txn_type = 'CREDIT';").Scan(&count)
	fmt.Printf("Credit bank txns: %d (error: %v)\n", count, err)
}
