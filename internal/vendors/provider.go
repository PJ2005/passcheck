package vendors

import (
	"context"
	"time"
)

// StandardVendorTxn represents the unified structure for incoming payment gateway transactions.
// Regardless of whether the data comes from Razorpay, PayU, or Stripe, it must be mapped to this struct.
type StandardVendorTxn struct {
	VendorTxnID    string
	Amount         float64
	UTRNumber      string
	SettlementDate time.Time
	VendorName     string
}

// PaymentProvider is the adapter interface that all vendor clients must implement.
type PaymentProvider interface {
	// FetchSettlements fetches all settlements for a given date and returns them as standardized structs.
	FetchSettlements(ctx context.Context, merchantID string, date time.Time) ([]StandardVendorTxn, error)
}
