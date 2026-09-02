// Package pinelabs is an explicitly mocked PaymentProvider adapter.
// No real Pine Labs API integration exists in this codebase. This file
// exists to demonstrate that the PaymentProvider interface (see
// internal/vendors/provider.go) is genuinely provider-agnostic: adding
// a new gateway means implementing one interface, not restructuring
// the reconciliation engine. FetchSettlements here returns synthetic
// data shaped like a real Pine Labs settlement response would look,
// based on Pine Labs' publicly documented settlement report fields,
// NOT a live API call.
package pinelabs

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"time"

	"passcheck/internal/vendors"
)

// Provider implements vendors.PaymentProvider for Pine Labs (mocked).
type Provider struct{}

// Ensure Provider implements PaymentProvider at compile time.
var _ vendors.PaymentProvider = (*Provider)(nil)

// FetchSettlements generates synthetic Pine Labs settlement records shaped like
// a real Pine Labs settlement report would look. It does not make any network call.
func (p *Provider) FetchSettlements(ctx context.Context, merchantID string, date time.Time) ([]vendors.StandardVendorTxn, error) {
	// Generate 5-10 synthetic records per call.
	count := 5 + rand.Intn(6)
	txns := make([]vendors.StandardVendorTxn, 0, count)

	for i := 0; i < count; i++ {
		amount := math.Round((200+rand.Float64()*4800)*100) / 100
		settlementID := fmt.Sprintf("pl_setl_%s", randString(12))
		vendorTxnID := fmt.Sprintf("PL_TXN_%s%04d", date.Format("20060102"), rand.Intn(10000))
		utr := fmt.Sprintf("PL_UTR%010d", rand.Intn(9999999999))
		// SettlementDate: same day as requested date, random hour
		settlementDate := time.Date(date.Year(), date.Month(), date.Day(), 10+rand.Intn(10), rand.Intn(60), 0, 0, date.Location())

		txns = append(txns, vendors.StandardVendorTxn{
			VendorTxnID:    vendorTxnID,
			Amount:         amount,
			SettlementID:   settlementID,
			UTRNumber:      utr,
			SettlementDate: settlementDate,
			VendorName:     "PineLabs",
		})

		// Use i to avoid unused variable in case of future logic
		_ = i
	}

	return txns, nil
}

func randString(n int) string {
	const alphanum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = alphanum[rand.Intn(len(alphanum))]
	}
	return string(b)
}
