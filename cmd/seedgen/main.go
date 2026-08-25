// Command seedgen generates a synthetic, partially-broken reconciliation batch
// directly against the database schema. Run manually:
//
//	go run ./cmd/seedgen -merchant <merchant_id>
//
// It produces exactly 55 vendor_transactions and their corresponding
// bank_transactions across four categories:
//
//   - ~70% clean matches (settlement_id fully present in the NEFT narration)
//   - ~15% lumped settlements (multiple vendor settlements funded by ONE bank credit)
//   - ~10% truncated narrations (bank narration cuts the settlement_id mid-string)
//   - ~5%  T+1 timing bleed (late-night settlement landing in the next day's bank batch)
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand"
	"strings"
	"time"

	"passcheck/internal/database"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

// Category sizing. Clean is 39 (not 38) so all categories sum to exactly 55;
// 39/55 is still ~70%. Lumped is 8 vendor transactions flushed as groups of
// 3/3/2, each group funded by a single bank credit.
const (
	totalTxns        = 55
	cleanCount       = 39
	lumpedCount      = 8
	truncatedCount   = 5
	timingBleedCount = 3
	netFeeFactor     = 0.98 // platform fee: bank receives net = gross * 0.98
)

const alphanum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// settleIDOffset is the length of "NEFT CR:<16-char UTR>/RAZORPAY SETL " —
// i.e. the exact index at which the settlement ID begins in a narration.
const settleIDOffset = len("NEFT CR:UTR000000000000000/RAZORPAY SETL ")

func randStr(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alphanum[rand.Intn(len(alphanum))]
	}
	return string(b)
}

func randDigits(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('0' + rand.Intn(10))
	}
	return string(b)
}

func genSettlementID() string { return "setl_" + randStr(14) }

func genUTR() string { return "UTR" + randDigits(13) }

func round2(x float64) float64 { return math.Round(x*100) / 100 }

func genGross() float64 { return round2(500 + rand.Float64()*49500) }

func netOf(gross float64) float64 { return round2(gross * netFeeFactor) }

// dayFor spreads records over the trailing week so date-window filtering has variety.
func dayFor(seq int) time.Time {
	now := time.Now()
	return now.AddDate(0, 0, -(seq % 7))
}

func atDay(day time.Time, hour, min int) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), hour, min, 0, 0, day.Location())
}

func main() {
	merchantFlag := flag.String("merchant", "", "Merchant ID to generate data for (defaults to the first merchant in the database)")
	flag.Parse()

	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, reading configurations from OS environment variables")
	}

	db, err := database.NewConnectionPool()
	if err != nil {
		log.Fatalf("Critical error initializing database: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	merchantID := *merchantFlag
	if merchantID == "" {
		if err := db.Pool.QueryRow(ctx, "SELECT id FROM merchants LIMIT 1").Scan(&merchantID); err != nil {
			log.Fatalf("no merchants found in database; create one or pass -merchant")
		}
		log.Printf("No -merchant flag provided, using first merchant: %s", merchantID)
	}

	vendorIntegrationID, err := ensureVendorIntegration(ctx, db.Pool, merchantID)
	if err != nil {
		log.Fatalf("failed to resolve vendor integration: %v", err)
	}
	log.Printf("Using vendor integration: %s", vendorIntegrationID)

	bankAccountID, err := ensureBankAccount(ctx, db.Pool, merchantID)
	if err != nil {
		log.Fatalf("failed to resolve bank account: %v", err)
	}
	log.Printf("Using bank account: %s", bankAccountID)

	stats, err := generate(ctx, db.Pool, vendorIntegrationID, bankAccountID)
	if err != nil {
		log.Fatalf("generation failed: %v", err)
	}

	fmt.Println("\n=== seedgen summary ===")
	fmt.Printf("Clean matches:          %d vendor / %d bank\n", stats.cleanVendors, stats.cleanBanks)
	fmt.Printf("Lumped settlements:     %d vendor / %d bank (groups of 3/3/2)\n", stats.lumpedVendors, stats.lumpedBanks)
	fmt.Printf("Truncated narrations:   %d vendor / %d bank\n", stats.truncatedVendors, stats.truncatedBanks)
	fmt.Printf("T+1 timing bleeds:      %d vendor / %d bank\n", stats.timingVendors, stats.timingBanks)
	fmt.Printf("TOTAL:                  %d vendor_transactions + %d bank_transactions = %d records\n",
		stats.vendorRows, stats.bankRows, stats.vendorRows+stats.bankRows)
}

type genStats struct {
	cleanVendors, cleanBanks         int
	lumpedVendors, lumpedBanks       int
	truncatedVendors, truncatedBanks int
	timingVendors, timingBanks       int
	vendorRows, bankRows             int
}

func generate(ctx context.Context, pool *pgxpool.Pool, vendorIntegrationID, bankAccountID string) (*genStats, error) {
	stats := &genStats{}
	seq := 0
	ref := 100000 + rand.Intn(800000)

	insertVendor := func(gross float64, utr, settlementID string, date time.Time) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO vendor_transactions
				(vendor_integration_id, vendor_txn_id, amount, utr_number, settlement_id, settlement_date, recon_status)
			VALUES ($1, $2, $3, $4, $5, $6, 'UNMATCHED')
		`, vendorIntegrationID, fmt.Sprintf("pay_%s%04d", randStr(12), seq), gross, utr, settlementID, date)
		return err
	}
	insertBank := func(amount float64, narration, utr string, date time.Time) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO bank_transactions
				(bank_account_id, amount, txn_type, narration, utr_number, txn_date)
			VALUES ($1, $2, 'CREDIT', $3, $4, $5)
		`, bankAccountID, amount, narration, utr, date)
		return err
	}

	// --- Category A: clean matches ---
	for i := 0; i < cleanCount; i++ {
		seq++
		gross := genGross()
		setl := genSettlementID()
		utr := genUTR()
		settledAt := atDay(dayFor(seq), 15+rand.Intn(5), rand.Intn(60))

		if err := insertVendor(gross, utr, setl, settledAt); err != nil {
			return nil, err
		}
		// Batch UTR on the bank side is intentionally distinct from the
		// payment-level UTR: settlement_id is the primary match key.
		bankUtr := genUTR()
		narration := fmt.Sprintf("NEFT CR:%s/RAZORPAY SETL %s/REF %09d", bankUtr, setl, ref)
		ref++

		if err := insertBank(netOf(gross), narration, bankUtr, settledAt.Add(time.Duration(90+rand.Intn(120))*time.Minute)); err != nil {
			return nil, err
		}
		stats.cleanVendors++
		stats.cleanBanks++
	}

	// --- Category B: lumped settlements (groups of 3/3/2 sharing one credit) ---
	for _, groupSize := range []int{3, 3, 2} {
		seq += groupSize
		groupDay := dayFor(seq)
		batchUtr := genUTR()

		var setlIDs []string
		var netSum float64
		for j := 0; j < groupSize; j++ {
			gross := genGross()
			setl := genSettlementID()
			setlIDs = append(setlIDs, setl)
			netSum += netOf(gross)

			if err := insertVendor(gross, genUTR(), setl, atDay(groupDay, 16+rand.Intn(4), rand.Intn(60))); err != nil {
				return nil, err
			}
			stats.lumpedVendors++
		}

		narration := fmt.Sprintf("NEFT CR:%s/MULTI SETL %s/REF %09d", batchUtr, strings.Join(setlIDs, ","), ref)
		ref++
		if err := insertBank(round2(netSum), narration, batchUtr, atDay(groupDay, 21, rand.Intn(50))); err != nil {
			return nil, err
		}
		stats.lumpedBanks++
	}

	// --- Category C: truncated narrations ---
	for i := 0; i < truncatedCount; i++ {
		seq++
		gross := genGross()
		setl := genSettlementID()
		settledAt := atDay(dayFor(seq), 15+rand.Intn(5), rand.Intn(60))

		if err := insertVendor(gross, genUTR(), setl, settledAt); err != nil {
			return nil, err
		}
		bankUtr := genUTR()
		full := fmt.Sprintf("NEFT CR:%s/RAZORPAY SETL %s/REF %09d", bankUtr, setl, ref)
		ref++
		// Cut inside the settlement ID so only a fragment survives (well under
		// the 100-char narration cap), e.g. "...SETL setl_a83Kf0" with no REF tail.
		cut := settleIDOffset + 4 + rand.Intn(10)
		if cut > len(full) {
			cut = len(full)
		}

		if err := insertBank(netOf(gross), full[:cut], bankUtr, settledAt.Add(time.Duration(90+rand.Intn(120))*time.Minute)); err != nil {
			return nil, err
		}
		stats.truncatedVendors++
		stats.truncatedBanks++
	}

	// --- Category D: T+1 timing bleeds ---
	for i := 0; i < timingBleedCount; i++ {
		seq++
		gross := genGross()
		setl := genSettlementID()
		settledAt := atDay(dayFor(seq), 23, 45)           // 11:45 PM
		bankCreditedAt := settledAt.Add(90 * time.Minute) // 01:15 AM next day

		if err := insertVendor(gross, genUTR(), setl, settledAt); err != nil {
			return nil, err
		}
		bankUtr := genUTR()
		narration := fmt.Sprintf("NEFT CR:%s/RAZORPAY SETL %s/REF %09d", bankUtr, setl, ref)
		ref++

		if err := insertBank(netOf(gross), narration, bankUtr, bankCreditedAt); err != nil {
			return nil, err
		}
		stats.timingVendors++
		stats.timingBanks++
	}

	stats.vendorRows = cleanCount + lumpedCount + truncatedCount + timingBleedCount
	stats.bankRows = cleanCount + 3 + truncatedCount + timingBleedCount

	if stats.vendorRows != totalTxns {
		return nil, fmt.Errorf("internal error: generated %d vendor rows, expected %d", stats.vendorRows, totalTxns)
	}
	return stats, nil
}

func ensureVendorIntegration(ctx context.Context, pool *pgxpool.Pool, merchantID string) (string, error) {
	var id string
	err := pool.QueryRow(ctx,
		"SELECT id FROM vendor_integrations WHERE merchant_id = $1 LIMIT 1", merchantID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != pgx.ErrNoRows {
		return "", err
	}
	// Fresh database: create a placeholder integration so generation can proceed.
	err = pool.QueryRow(ctx, `
		INSERT INTO vendor_integrations (merchant_id, vendor_name, encrypted_credentials)
		VALUES ($1, 'Razorpay', '{"mock": "keys"}')
		RETURNING id
	`, merchantID).Scan(&id)
	return id, err
}

func ensureBankAccount(ctx context.Context, pool *pgxpool.Pool, merchantID string) (string, error) {
	var id string
	err := pool.QueryRow(ctx,
		"SELECT id FROM merchant_bank_accounts WHERE merchant_id = $1 LIMIT 1", merchantID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != pgx.ErrNoRows {
		return "", err
	}
	err = pool.QueryRow(ctx, `
		INSERT INTO merchant_bank_accounts (merchant_id, rpd_request_id, account_number, ifsc_code, verified_account_name)
		VALUES ($1, $2, '50100234567890', 'HDFC0001234', 'SEEDGEN PLACEHOLDER')
		RETURNING id
	`, merchantID, "rpd_seedgen_"+randStr(10)).Scan(&id)
	return id, err
}
