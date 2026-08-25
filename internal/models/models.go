package models

import (
	"time"

	"github.com/google/uuid"
)

// Enums as custom string types for type safety in Go
type KYCStatus string

const (
	KYCStatusPending KYCStatus = "PENDING"
	KYCStatusSuccess KYCStatus = "SUCCESS"
	KYCStatusFailed  KYCStatus = "FAILED"
)

type ConsentStatus string

const (
	ConsentStatusPending  ConsentStatus = "PENDING"
	ConsentStatusActive   ConsentStatus = "ACTIVE"
	ConsentStatusRejected ConsentStatus = "REJECTED"
	ConsentStatusRevoked  ConsentStatus = "REVOKED"
)

type SessionStatus string

const (
	SessionStatusPending   SessionStatus = "PENDING"
	SessionStatusCompleted SessionStatus = "COMPLETED"
	SessionStatusFailed    SessionStatus = "FAILED"
)

type TxnType string

const (
	TxnTypeCredit TxnType = "CREDIT"
	TxnTypeDebit  TxnType = "DEBIT"
)

type ReconStatus string

const (
	ReconStatusUnmatched ReconStatus = "UNMATCHED"
	ReconStatusMatched   ReconStatus = "MATCHED"
	ReconStatusDisputed  ReconStatus = "DISPUTED"
)

// ReconciliationMethod is stored as VARCHAR in the database (not a Postgres
// ENUM) so new methods can be added later without a migration.
type ReconciliationMethod string

const (
	ReconMethodDeterministic ReconciliationMethod = "deterministic"
	ReconMethodAgent         ReconciliationMethod = "agent"
	ReconMethodUnresolved    ReconciliationMethod = "unresolved"
)

// Models corresponding to the database tables

type Merchant struct {
	ID                uuid.UUID `json:"id" db:"id"`
	PhoneNumber       string    `json:"phone_number" db:"phone_number"`
	PAN               *string   `json:"pan" db:"pan"`
	PANStatus         KYCStatus `json:"pan_status" db:"pan_status"`
	PANRegisteredName *string   `json:"pan_registered_name" db:"pan_registered_name"`
	GSTIN             *string   `json:"gstin" db:"gstin"`
	GSTStatus         KYCStatus `json:"gst_status" db:"gst_status"`
	GSTRegisteredName *string   `json:"gst_registered_name" db:"gst_registered_name"`
	CreatedAt         time.Time `json:"created_at" db:"created_at"`
	UpdatedAt         time.Time `json:"updated_at" db:"updated_at"`
}

type MerchantBankAccount struct {
	ID                  uuid.UUID `json:"id" db:"id"`
	MerchantID          uuid.UUID `json:"merchant_id" db:"merchant_id"`
	RPDRequestID        string    `json:"rpd_request_id" db:"rpd_request_id"`
	RPDStatus           KYCStatus `json:"rpd_status" db:"rpd_status"`
	AccountNumber       *string   `json:"account_number" db:"account_number"`
	IFSCCode            *string   `json:"ifsc_code" db:"ifsc_code"`
	VerifiedAccountName *string   `json:"verified_account_name" db:"verified_account_name"`
	CreatedAt           time.Time `json:"created_at" db:"created_at"`
	UpdatedAt           time.Time `json:"updated_at" db:"updated_at"`
}

type AAConsent struct {
	ID            uuid.UUID     `json:"id" db:"id"`
	MerchantID    uuid.UUID     `json:"merchant_id" db:"merchant_id"`
	BankAccountID *uuid.UUID    `json:"bank_account_id" db:"bank_account_id"`
	SetuRequestID string        `json:"setu_request_id" db:"setu_request_id"`
	SetuConsentID *string       `json:"setu_consent_id" db:"setu_consent_id"`
	VUA           *string       `json:"vua" db:"vua"`
	Status        ConsentStatus `json:"status" db:"status"`
	ValidFrom     *time.Time    `json:"valid_from" db:"valid_from"`
	ValidUntil    *time.Time    `json:"valid_until" db:"valid_until"`
	CreatedAt     time.Time     `json:"created_at" db:"created_at"`
	UpdatedAt     time.Time     `json:"updated_at" db:"updated_at"`
}

type AADataSession struct {
	ID            uuid.UUID     `json:"id" db:"id"`
	ConsentID     uuid.UUID     `json:"consent_id" db:"consent_id"`
	SetuSessionID string        `json:"setu_session_id" db:"setu_session_id"`
	Status        SessionStatus `json:"status" db:"status"`
	DataRangeFrom time.Time     `json:"data_range_from" db:"data_range_from"`
	DataRangeTo   time.Time     `json:"data_range_to" db:"data_range_to"`
	CreatedAt     time.Time     `json:"created_at" db:"created_at"`
	CompletedAt   *time.Time    `json:"completed_at" db:"completed_at"`
}

type VendorIntegration struct {
	ID                   uuid.UUID `json:"id" db:"id"`
	MerchantID           uuid.UUID `json:"merchant_id" db:"merchant_id"`
	VendorName           string    `json:"vendor_name" db:"vendor_name"`
	EncryptedCredentials string    `json:"encrypted_credentials" db:"encrypted_credentials"`
	IsActive             bool      `json:"is_active" db:"is_active"`
	CreatedAt            time.Time `json:"created_at" db:"created_at"`
}

type VendorTransaction struct {
	ID                  uuid.UUID   `json:"id" db:"id"`
	VendorIntegrationID uuid.UUID   `json:"vendor_integration_id" db:"vendor_integration_id"`
	VendorTxnID         string      `json:"vendor_txn_id" db:"vendor_txn_id"`
	Amount              float64     `json:"amount" db:"amount"`
	SettlementID        *string     `json:"settlement_id" db:"settlement_id"`
	UTRNumber           *string     `json:"utr_number" db:"utr_number"`
	SettlementDate      *time.Time  `json:"settlement_date" db:"settlement_date"`
	ReconStatus         ReconStatus `json:"recon_status" db:"recon_status"`
	CreatedAt           time.Time   `json:"created_at" db:"created_at"`
}

type BankTransaction struct {
	ID             uuid.UUID  `json:"id" db:"id"`
	BankAccountID  uuid.UUID  `json:"bank_account_id" db:"bank_account_id"`
	Amount         float64    `json:"amount" db:"amount"`
	TxnType        TxnType    `json:"txn_type" db:"txn_type"`
	Narration      *string    `json:"narration" db:"narration"`
	UTRNumber      *string    `json:"utr_number" db:"utr_number"`
	TxnDate        time.Time  `json:"txn_date" db:"txn_date"`
	ClosingBalance *float64   `json:"closing_balance" db:"closing_balance"`
	CreatedAt      time.Time  `json:"created_at" db:"created_at"`
}

type ReconciledMatch struct {
	ID                  uuid.UUID  `json:"id" db:"id"`
	VendorTransactionID uuid.UUID  `json:"vendor_transaction_id" db:"vendor_transaction_id"`
	BankTransactionID   uuid.UUID  `json:"bank_transaction_id" db:"bank_transaction_id"`
	MatchedAt           time.Time  `json:"matched_at" db:"matched_at"`
}

type ReconciliationLog struct {
	ID                  uuid.UUID            `json:"id" db:"id"`
	VendorTransactionID uuid.UUID            `json:"vendor_transaction_id" db:"vendor_transaction_id"`
	BankTransactionID   *uuid.UUID           `json:"bank_transaction_id" db:"bank_transaction_id"`
	Method              ReconciliationMethod `json:"method" db:"method"`
	Confidence          *float64             `json:"confidence" db:"confidence"`
	Reasoning           *string              `json:"reasoning" db:"reasoning"`
	CreatedAt           time.Time            `json:"created_at" db:"created_at"`
}
