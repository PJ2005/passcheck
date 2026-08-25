# PassCheck FIU API Service

PassCheck is a financial reconciliation backend service built in Go. It operates as a Financial Information User (FIU) to automate the matching of payment gateway settlements against actual bank account credits. 

The primary problem this system solves is the manual reconciliation effort required by merchants to verify if the money promised by their payment gateways (like Razorpay or PhonePe) has actually settled into their bank accounts.

## Core Architecture

The system operates by fetching data from two independent financial streams and running a matching engine to reconcile them:

1. **Bank Statement Stream (Setu Account Aggregator):** Using the Setu AA network, the system requests user consent and pulls verified, read-only bank statement data (specifically CREDIT transactions) for the merchant's linked accounts.
2. **Payment Gateway Stream (Vendor APIs):** The system integrates with payment gateway APIs (e.g., Razorpay, PhonePe) to fetch daily settlement records and extract the Unique Transaction Reference (UTR) numbers and amounts for successful payments.

The reconciliation engine then compares these two data sets based on UTR numbers and amounts to find exact matches, mark transactions as settled, or flag discrepancies.

## Key Modules

### 1. Merchant Onboarding & KYC (`internal/setu` & `cmd/api`)
Before a merchant can use the system, they must be verified. The system uses Setu's Data Gateway (DG) APIs to perform:
- **PAN Verification:** Validates the merchant's PAN number and name.
- **GST Verification:** Validates the merchant's GSTIN.
- **Penny Drop (RPD):** Validates the merchant's bank account by depositing a small amount and verifying the account holder's name (Reverse Penny Drop).

### 2. Account Aggregator Sync (`internal/setu` & `internal/webhooks`)
- **Consent Initiation:** Generates a Setu AA consent request for the merchant.
- **Data Sessions:** Once consent is approved, the system creates a data session to fetch historical bank transactions.
- **Webhooks:** Listens to async FI_STATUS_UPDATE webhooks from Setu to know when bank data is ready to be fetched and parsed into the `bank_transactions` database table.

### 3. Vendor Provider Adapters (`internal/vendors`)
To support multiple payment gateways, the system uses a standard `PaymentProvider` interface.
- Adapters (e.g., `phonepe/client.go`, `razorpay/client.go`) implement this interface.
- They fetch proprietary gateway data, handle necessary API authentication (like PhonePe's X-VERIFY SHA256 checksums), and map the responses to a unified `StandardVendorTxn` struct.

### 4. Reconciliation Engine (`internal/reconciliation`)
The core matching logic. It queries unmatched vendor transactions and attempts to find a matching unmatched bank credit transaction using the `utr_number` and `amount` as the composite key. Matches are recorded securely in a `reconciled_matches` table, establishing an immutable link between the vendor record and the bank record.

### 5. Daily Cron Orchestrator (`internal/cron`)
A background scheduler that triggers the daily synchronization pipeline automatically (e.g., at 2:00 AM). It iterates through all active merchants, triggers their respective vendor adapters to pull the previous day's settlements, and initiates a Setu AA data session to pull the previous day's bank statement.

## Application Flow

The system operates in three distinct phases: Onboarding, Account Setup, and Daily Reconciliation.

### Phase 1: Merchant Onboarding
This phase establishes the merchant's identity and banking details.
1. **PAN Verification (`POST /api/v1/onboard/pan`):**
   - **Action:** Receives the merchant's PAN and phone number.
   - **External API:** Calls Setu's PAN Verification API.
   - **Storage:** Creates a new record in the `merchants` PostgreSQL table containing the PAN, status, and verified name.
2. **GST Verification (`POST /api/v1/onboard/gst`):**
   - **Action:** Receives the GSTIN.
   - **External API:** Calls Setu's GST Verification API.
   - **Storage:** Updates the existing record in the `merchants` table with GST details.
3. **Bank Verification (`POST /api/v1/onboard/bank`):**
   - **Action:** Initiates a Penny Drop to verify the merchant's settlement bank account.
   - **External API:** Calls Setu's Reverse Penny Drop (RPD) API.
   - **Storage:** Stores the pending verification state in the `merchant_bank_accounts` table.

### Phase 2: Account Aggregator (AA) Setup
This phase establishes continuous, read-only access to the merchant's bank statements.
1. **Consent Initiation (`POST /api/v1/consent/initiate`):**
   - **Action:** Requests permission to read the merchant's bank data for a specific duration.
   - **External API:** Calls Setu AA `Initiate Consent` API.
   - **Storage:** Records the request in the `aa_consents` table with a `PENDING` status.
2. **Consent Approval Webhook (`POST /api/v1/webhooks/setu`):**
   - **Action:** Setu asynchronously notifies the system when the merchant approves the consent on their mobile device.
   - **Storage:** Updates the `aa_consents` table status to `ACTIVE`.

### Phase 3: Daily Synchronization & Reconciliation
This is the core loop that runs daily via the Cron orchestrator or manually via `POST /api/v1/admin/trigger-pipeline`.
1. **Vendor Settlement Sync:**
   - **Action:** The orchestrator iterates over all active merchants and their linked payment gateways.
   - **External API:** Calls the Razorpay/PhonePe Settlement APIs (using the `PaymentProvider` adapter) to fetch all payouts for the previous day.
   - **Storage:** Maps responses to a standard structure and inserts them into the `vendor_transactions` table with `recon_status = 'UNMATCHED'`.
2. **Bank Statement Request:**
   - **Action:** The orchestrator requests the previous day's bank statement using the active AA consent.
   - **External API:** Calls Setu AA `Create Data Session` API.
   - **Storage:** Stores the session ID in `aa_data_sessions` with status `INITIATED`.
3. **Bank Data Retrieval (Webhook):**
   - **Action:** Setu sends a `FI_STATUS_UPDATE` webhook when the bank data is ready to download.
   - **External API:** The system calls Setu AA to download and decrypt the Financial Information (FI) JSON.
   - **Storage:** Parses the JSON and inserts only `CREDIT` transactions into the `bank_transactions` table.
4. **Reconciliation Engine Execution:**
   - **Action:** The system queries the database for all `UNMATCHED` records in `vendor_transactions` and `bank_transactions` for the specific merchant.
   - **Matching Logic:** It attempts an exact match using `utr_number` and `amount`.
   - **Storage:** Successful matches are inserted into the `reconciled_matches` table, and the respective source rows are updated to reflect their matched status.

## Technology Stack

- **Language:** Go (Golang)
- **Framework:** Fiber (v2) for high-performance HTTP routing and middleware.
- **Database:** PostgreSQL (v16) for persistent, relational storage.
- **Database Driver:** `pgxpool` (jackc/pgx/v5) for efficient connection pooling.
- **Environment Management:** `godotenv` for loading `.env` configurations.

## Project Structure

- `/cmd/api`: The entry point of the application (`main.go`), containing server initialization, route definitions, and graceful shutdown logic.
- `/internal/cron`: Background jobs and orchestrators.
- `/internal/database`: PostgreSQL connection management and migrations.
- `/internal/demo`: Seeders and dashboard mock generation for testing and UI demonstration.
- `/internal/reconciliation`: The core UTR and amount matching algorithms.
- `/internal/setu`: API clients for Setu Account Aggregator and Setu KYC/DG services.
- `/internal/vendors`: The common Payment Gateway adapter interface and specific implementations (Razorpay, PhonePe).
- `/internal/webhooks`: HTTP handlers for receiving async updates from external partners (like Setu).
- `/public`: Static assets and simple HTML templates for the demo dashboard.
- `/scratch`: Temporary scripts used for local testing and debugging.

## Local Setup Instructions

### Prerequisites
- Go 1.21+
- PostgreSQL 16+

### 1. Database Setup
Ensure PostgreSQL is running on your local machine.
Create a database named `passcheck_db`.
```sql
CREATE DATABASE passcheck_db;
```
*(Run the corresponding schema migrations to set up the tables).*

### 2. Environment Variables
Create a `.env` file in the root directory based on the configuration required. The file should include:
- Database settings (`DB_HOST`, `DB_PORT`, `DB_USER`, `DB_PASSWORD`, `DB_NAME`)
- Setu credentials (`SETU_AA_CLIENT_ID`, `SETU_KYC_CLIENT_ID`, etc.)
- Vendor testing credentials (`RAZORPAY_KEY_ID`, `PHONEPE_CLIENT_ID`, etc.)

### 3. Running the Server
Use the standard Go toolchain to build and run the API server.
```bash
go run ./cmd/api
```
The server will start on `http://localhost:8080` (or the port defined in your `.env`).

## Testing & Demo

### Generating Synthetic Test Data
Use the seedgen CLI to insert a batch of 55 synthetic reconciliation records directly into the database — clean matches, lumped settlements, truncated narrations, and T+1 timing bleeds — for realistic testing of the reconciliation engine:
```bash
go run ./cmd/seedgen -merchant <merchant_id>
```
If `-merchant` is omitted, the first merchant in the database is used.

### Demo Endpoints
The application provides demo endpoints to simulate the reconciliation flow without live API triggers:
- `/api/v1/reconcile`: Triggers the reconciliation engine immediately.
- `/api/v1/demo/dashboard/:merchantId`: Provides a visual web dashboard of the reconciliation results.

### AI Agent Exception Resolution
Set `GEMINI_API_KEY` in your `.env` (get a free key at [Google AI Studio](https://aistudio.google.com/apikey)) to enable Gemini-powered resolution of transactions the deterministic engine cannot match. The demo flow is two steps:
```bash
curl -X POST http://localhost:8080/api/v1/reconcile       -H 'Content-Type: application/json' -d '{"merchant_id": "<merchant_id>"}'  # deterministic pass
curl -X POST http://localhost:8080/api/v1/reconcile/agent -H 'Content-Type: application/json' -d '{"merchant_id": "<merchant_id>"}'  # agent pass on leftovers
```
Every decision from both passes is written to the `reconciliation_log` audit table and surfaced on the dashboard. Without the key, step 1 still works and step 2 returns a clear 503.
