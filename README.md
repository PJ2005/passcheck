# PassCheck — Automated Settlement Reconciliation for Indian Payment Gateways

PassCheck is a reconciliation control plane for merchants who accept payments through Indian gateways (Razorpay, etc.) and need to prove, without manual spreadsheets, that every gateway settlement actually landed as a bank credit. A fast deterministic engine matches the majority of records; an LLM agent reasons over the genuinely ambiguous remainder — every decision from both layers is written to the same auditable log.

Built for **Razorpay AI Buildathon 2026, Track 04 — AI Finance Controller**.

## 2. The problem, briefly

On the gateway side a settlement is a batch with a `settlement_id` and a gross amount. On the bank side the same money arrives as a single NEFT credit whose narration embeds that `settlement_id` (not a payment-level UTR), and the credit amount is net of the gateway's ~2% platform fee. Three real-world wrinkles break naive UTR-or-amount matching: a bank credit can **lump** 2–3 settlements into one narration (`setl_A,setl_B,setl_C`), bank systems **truncate** long narrations mid-`settlement_id`, and late-night settlements **bleed** across the day boundary (`23:45` settled → `01:15` credited, T+1). This is why the project exists — a spreadsheet `VLOOKUP` on UTR is insufficient.

## 3. Architecture

```
                ┌────────────────────┐
                │   cmd/seedgen      │  synthetic FI & gateway batch
                │  5 categories:     │  56 vendor txns → 50 bank txns
                │  clean / lumped /  │  (1 deliberately orphaned)
                │  truncated / T+1 / │
                │  orphan             │
                └─────────┬──────────┘
                          │  vendor_transactions
                          │  bank_transactions
                          ▼
                ┌────────────────────┐
                │  Deterministic     │  Go, single tx, tiered
                │  Engine            │  1. exact settlement_id (1.0)
                │  engine.go         │  2. lumped narration (0.9)
                │                    │  3. truncated prefix (0.7)
                │                    │  4. legacy UTR+amount (0.85)
                └─────────┬──────────┘
                          │  UNMATCHED → reconciliation_log (unresolved)
                          │  MATCHED   → reconciled_matches + log (deterministic)
                          ▼
                ┌────────────────────┐
                │  Exceptions Queue  │  SELECT … WHERE recon_status='UNMATCHED'
                │  fetchCandidates() │  date window + 0.90–1.02 amount band, cap 5
                └─────────┬──────────┘
                          │  amount_ratio / date_gap_days / settlement_id_overlap
                          │  (pre-computed in Go, not by the model)
                          ▼
                ┌────────────────────┐
                │  Gemini Agent      │  Go HTTP client → Gemini API
                │  resolver.go       │  model: gemini-flash-lite-latest
                │  confidence ≥ 0.75 │  JSON-only, hallucination guard
                └─────────┬──────────┘
                          │  MATCHED → reconciled_matches + log (agent)
                          │  else    → log (unresolved, with reasoning)
                          ▼
                ┌────────────────────┐
                │ reconciliation_log │  single audit trail for both passes
                │ dashboard +        │  /api/v1/demo/dashboard/:merchantId
                │ records + CSV      │  /api/v1/demo/records/:merchantId?format=csv
                └────────────────────┘
```

Both the deterministic engine and the AI agent layer are implemented in Go — there is no second language or framework. The agent layer is a plain HTTP client calling the Gemini API, following the same pattern as the existing Razorpay vendor client. This was a deliberate choice: introducing Python or an agentic framework would have added complexity without adding capability that Go's standard library doesn't already provide.

## 4. How matching actually works

### Deterministic tiers (`internal/reconciliation/engine.go`)

`RunDailyReconciliation(merchantID, db)` opens one transaction, snapshots all `UNMATCHED` vendor rows for the merchant, and walks each row through four tiers in order — first hit wins. Every tier queries inside a date window of `[settlement_date 00:00, settlement_date + 3d 00:00)` (i.e. the settlement day through `day+2` inclusive, `dateWindowExtraDays = 2`) to absorb T+1 and weekend/holiday slack.

| Tier | What it looks for | When it applies | Confidence written to `reconciliation_log.confidence` |
|---|---|---|---|
| **1 — exact `settlement_id`** | `bt.narration LIKE '%settlement_id%'` in a *single-settlement* narration (`narration !~ lumpedNarrationPattern`) | `settlement_id` present and date window usable | `1.0` |
| **2 — lumped** | same `LIKE` but in a *lumped* narration (`narration ~ '(setl_[A-Za-z0-9]+[^A-Za-z0-9]+)+setl_[A-Za-z0-9]+'`) | same as T1, but the bank credit lists 2–3 IDs | `0.9` — amount sum is intentionally *not* verified here; the bank row is the sum of several vendors |
| **3 — truncated prefix** | first `minPartialIDLen = 8` chars of `settlement_id` appear in exactly one bank narration (`COUNT(*) == 1`) | same as T1/T2, for bank-side truncation | `0.7` — heuristic, so lower trust; ambiguous (`count != 1`) is refused |
| **4 — legacy UTR + amount** | `bt.utr_number = vendor.utr_number AND ABS(bt.amount - vendor.amount) <= vendor.amount * legacyFeeTolerance` | fallback when `settlement_id` is missing (older data) | `0.85` with `legacyFeeTolerance = 0.03` |

A match inserts one row in `reconciled_matches`, flips `vendor_transactions.recon_status` to `MATCHED`, and writes one `reconciliation_log` row with `method = 'deterministic'`. If no tier hits, the row stays `UNMATCHED` and one `method = 'unresolved'` log row records what was attempted. The pairing guard is `NOT EXISTS (rm.vendor_transaction_id = $vendor AND rm.bank_transaction_id = $bank)` — the same bank credit may legitimately match many vendors after migration `003` removed the uniqueness on `bank_transaction_id`.

`ReconciliationResult` returned to the caller: `total_processed`, `matched_count`, `unresolved_count`, `match_rate` (`matched/total`, 0 if none), `elapsed_ms`, `throughput_per_sec` (`total / (elapsed_ms/1000)`, 0 if elapsed is 0).

### Agent layer (`internal/agent/resolver.go` + `client.go`)

`ResolveExceptions(merchantID, db, agentClient)` runs in its own transaction over every `UNMATCHED` row that has a prior `unresolved` log entry.

1. **Candidate gathering** (`fetchCandidates`) — two deterministic guards before the model is ever called: a wider date window `[settlement_date 00:00, +4d 00:00)` (day through `day+3` inclusive) and an amount band `bank.amount BETWEEN vendor.amount*0.90 AND vendor.amount*1.02` that admits the ~2% fee relationship with headroom. Candidates are ordered by `ABS(bank.amount - vendor.amount)` and capped at 5. No candidates → immediate `unresolved` audit, no LLM call.

2. **Evidence pre-computation** (`buildResolverPrompt`) — for each candidate Go computes three labeled signals and hands them to the model as text:
   - `amount_ratio` = `bank.amount / vendor.amount` (string: `"0.9812"`, with note that `~0.98` is expected)
   - `date_gap_days` = `round(|settlement_date - txn_date| / 24h)` (`longestPrefixOverlap` helper's complement; `-1` if either date is nil)
   - `settlement_id_overlap` = `longestPrefixOverlap(settlement_id, narration)` reported as `"X of N characters matched"` — longest run of consecutive `settlement_id` characters anchored at `id[0]` appearing in the narration, which bank truncation preserves

The prompt also includes `settlement_id`, `utr_number`, `amount`, `settlement_date`, and the prior `unresolved` reasoning.

3. **Judgment** (`GeminiClient.GenerateJSON` in `client.go`, model `gemini-flash-lite-latest`, `ResponseMIMEType: "application/json"`) — `resolverSystemInstruction` pins the model to judge those three signals and return `{"is_match": bool, "bank_transaction_id": string, "confidence": 0.0-1.0, "reasoning": "<200 chars"}`. The model is explicitly told not to re-derive evidence.

4. **Gates** — `AgentConfidenceThreshold = 0.75`. If `!is_match` or `confidence < 0.75`, the row stays `unresolved` with that reasoning. If `is_match` but `bank_transaction_id` is not among the offered candidates, it is rejected as hallucination. Otherwise the row is written as `MATCHED` + `reconciliation_log` with `method = 'agent'`. Every other outcome (API error, unparsable JSON, guarded rejection) writes an `unresolved` audit row and continues — one bad row never sinks the batch.

The AI agent never performs arithmetic or string matching itself — every numeric signal it reasons over (fee ratio, date proximity, text overlap) is pre-computed in Go and handed to it as labeled evidence. The model's only job is judging weight of evidence across pre-computed signals, not deriving them.

## 5. What's deliberately out of scope, and why

- **Section 194O TDS logic**: whether a payment-gateway intermediary (as opposed to a marketplace operator) is a 194O deductor is a live, disputed question in Indian tax law. Research showed the interpretation is unsettled; building a headline feature on disputed tax reasoning was judged too risky for a core architectural pillar.

- **Multi-source vendor breadth (e.g. Pine Labs adapter)**: the brief lists a second gateway as one of several *example* directions, not an additive requirement. Depth on one 3-way match (vendor ledger → gateway settlement → bank credit) was prioritized over shallow breadth. The `PaymentProvider` interface in `internal/vendors/provider.go` (`FetchSettlements(ctx, merchantID, date) ([]StandardVendorTxn, error)`) is intentionally kept so a second adapter slots in without architectural change — see the existing `razorpay/client.go` and `phonepe/client.go` stubs.

- **Setu Account Aggregator / KYC / onboarding flows**: real production code (PAN/GST verification, RPD penny-drop, AA consent + FI webhooks, cron) that was scoped out of this submission because it belongs to a different product (merchant onboarding/compliance) than the one being demonstrated — a 56-record synthetic batch loop. It was removed to keep the reconciliation story legible; the history is preserved on prior commits/branches. The current `internal/` no longer contains `internal/setu`, `internal/webhooks`, or `internal/cron`.

## 6. Tech stack

- **Language**: Go `1.25.0` (`go.mod`), single-language architecture — deliberately no Python, no agentic framework
- **HTTP**: Fiber `v2.52.13` (`github.com/gofiber/fiber/v2`)
- **DB**: PostgreSQL, driver `pgx/v5` `v5.9.2` (`pgxpool`, `pgx.Tx`, no ORM)
- **AI**: Gemini API via `google.golang.org/genai` `v1.69.0`, model string `gemini-flash-lite-latest` (`internal/agent/client.go:18`), thin wrapper `GeminiClient.GenerateJSON` (system instruction + user prompt, JSON MIME)
- **Env**: `joho/godotenv` for local `.env` loading

## 7. Project structure

```
.
├── cmd/
│   ├── api/main.go              — Fiber app, DB + Gemini init, route registration, graceful shutdown
│   └── seedgen/main.go          — synthetic batch generator (5 categories, single-tx, pre-commit verify)
├── internal/
│   ├── agent/
│   │   ├── client.go            — GeminiClient wrapper (NewGeminiClient, GenerateJSON)
│   │   └── resolver.go          — Phase-2: fetchCandidates, evidence pre-compute, confidence gate
│   ├── database/db.go           — pgxpool connection pool (env-driven DSN)
│   ├── demo/
│   │   └── dashboard.go         — GetReconciliationDashboard + GetReconciliationRecords (paginated + CSV)
│   ├── models/models.go         — Go structs mirroring core tables + ReconMethod constants
│   ├── reconciliation/engine.go — tiered deterministic matcher + audit logging
│   └── vendors/
│       ├── provider.go          — PaymentProvider interface + StandardVendorTxn
│       ├── credentials.go       — credential helpers
│       ├── razorpay/client.go   — Razorpay settlement adapter
│       ├── phonepe/client.go    — PhonePe adapter (stub)
│       └── sync.go              — vendor sync helpers
├── migrations/
│   ├── 001_initial_schema.sql
│   ├── 002_settlement_audit.sql
│   └── 003_allow_lumped_matches.sql
├── public/demo.html             — single-page dashboard (metrics, pending, exceptions, audit, full records)
├── go.mod / go.sum
└── .env                         — local env (not committed)
```

## 8. Database schema (brief)

All migrations are numbered, additive, and idempotent guards where appropriate (001 → 003).

| Table | Purpose |
|---|---|
| `merchants` | merchant identity (phone, PAN/GST + KYC statuses) |
| `merchant_bank_accounts` | settlement bank account (RPD fields, account/ifsc) — bank side owner |
| `aa_consents` / `aa_data_sessions` | retained AA schema from onboarding scope (not exercised in current demo path) |
| `vendor_integrations` | one row per merchant+vendor (e.g. `vendor_name='Razorpay'`) |
| `vendor_transactions` | source: gateway settlements — `vendor_txn_id`, `amount` (gross), `utr_number`, `settlement_id`, `settlement_date`, `recon_status` (`UNMATCHED`/`MATCHED`) |
| `bank_transactions` | destination: AA-decrypted bank credits — `amount` (net), `txn_type`, `narration`, `utr_number`, `txn_date` |
| `reconciled_matches` | ledger link `vendor_transaction_id → bank_transaction_id` (vendor unique, bank may repeat after 003) |
| `reconciliation_log` | **audit trail for every decision** — `vendor_transaction_id`, `bank_transaction_id` (nullable), `method` (`deterministic`/`agent`/`unresolved`), `confidence` (nullable), `reasoning`, `created_at` |

Key indexes: `vendor_transactions(settlement_id)`, `reconciliation_log(vendor_transaction_id)`, `reconciled_matches(bank_transaction_id)` (post-003), and the original UTR/date/status indexes from 001.

## 9. Local setup

### Prerequisites

- Go `1.25.0` (see `go.mod`)
- PostgreSQL 14+ (16 recommended), database `passcheck_db`

### Environment

`cmd/api/main.go` and `internal/database/db.go` read these from the environment (`.env` via `godotenv` is optional):

| Var | Required | Notes |
|---|---|---|
| `DB_HOST` | yes | e.g. `localhost` |
| `DB_PORT` | yes | e.g. `5432` |
| `DB_USER` | yes | |
| `DB_PASSWORD` | yes | |
| `DB_NAME` | yes | e.g. `passcheck_db` |
| `DB_SSLMODE` | no | defaults to `disable` |
| `GEMINI_API_KEY` | no | if unset, `/api/v1/reconcile` still works; `/api/v1/reconcile/agent` returns `503` |
| `PORT` | no | defaults to `8080` |

Example `.env`:

```
DB_HOST=localhost
DB_PORT=5432
DB_USER=postgres
DB_PASSWORD=postgres
DB_NAME=passcheck_db
DB_SSLMODE=disable
GEMINI_API_KEY=AIza...  # optional; deterministic pass works without it
PORT=8080
```

### Migrations

```bash
# from repo root, with psql
psql -h localhost -U postgres -d passcheck_db -f migrations/001_initial_schema.sql
psql -h localhost -U postgres -d passcheck_db -f migrations/002_settlement_audit.sql
psql -h localhost -U postgres -d passcheck_db -f migrations/003_allow_lumped_matches.sql
```

### Run the server

```bash
go run ./cmd/api
# → http://localhost:8080/  (dashboard at both / and /demo)
# → GET  /api/v1/health
# → POST /api/v1/reconcile
# → POST /api/v1/reconcile/agent
# → GET  /api/v1/demo/config
# → GET  /api/v1/demo/dashboard/:merchantId
# → GET  /api/v1/demo/records/:merchantId
```

## 10. Running the demo

A judge can follow this in order — no prior data needed.

### 1. Generate synthetic data

```bash
go run ./cmd/seedgen -merchant <merchant_id>
# if -merchant is omitted, the first merchant in the DB is used
```

What it generates (and why each exists):

| Category | Count | Simulates |
|---|---|---|
| **Clean** | 39 vendor / 39 bank | narration contains full `settlement_id` — the happy path |
| **Lumped** | 8 vendor / 3 bank (groups of 3/3/2) | bank merged several settlements into one NEFT credit (`MULTI SETL id1,id2,id3`) |
| **Truncated** | 5 / 5 | bank truncated narration mid-`settlement_id` — tests Tier 3 prefix logic |
| **T+1 bleed** | 3 / 3 | settlement at `23:45` landed as `01:15` next day — tests date window |
| **Orphaned** | 1 vendor / 0 bank | settlement that genuinely never hit the bank — must remain `UNMATCHED` through *both* passes |

All inserts for the 56 vendor + 50 bank rows happen in one transaction with a pre-commit `COUNT(*) WHERE id = ANY($vendorIDs)` verify; a failed run rolls back cleanly.

### 2. Open the dashboard

```
http://localhost:8080/
```
(also at `/demo`)

### 3. Deterministic pass

- **UI**: click **Run Engine**
- **API**: `curl -X POST http://localhost:8080/api/v1/reconcile -H 'Content-Type: application/json' -d '{"merchant_id":"<id>"}'`

Response `result` contains `total_processed`, `matched_count`, `unresolved_count`, `match_rate`, `elapsed_ms`, `throughput_per_sec`.

### 4. Agent pass (optional, requires `GEMINI_API_KEY`)

- **UI**: click **Run Agent**
- **API**: `curl -X POST http://localhost:8080/api/v1/reconcile/agent -H 'Content-Type: application/json' -d '{"merchant_id":"<id>"}'` → `{"resolved_count": N}`

If the key is unset, the endpoint returns `503` — the deterministic dashboard still works.

### 5. What to look at

- **Reconciliation Health** card — `method_breakdown` (`deterministic`/`agent`/`unresolved`) bar, `match_rate`, `throughput_per_sec`, `exception_count` (all from `reconciliation_log`, not invented).
- **Pending / Exceptions / Audit Trail** workspace tabs — `Pending` is the `UNMATCHED` table; `Exceptions` shows the 20 most recent `unresolved` log rows with reasoning; `Audit Trail` shows up to 10 recent `deterministic` matches, diversified by confidence tier.
- **Reasoning** text on every row — deterministic tiers emit e.g. `Tier 1: exact settlement_id match (setl_abc…) within date window`; agent rows show the model's `reasoning` filtered through the 0.75 gate.
- **Full Records** (if visible) — `GET /api/v1/demo/records/:merchantId?page=1&page_size=25&status=&method=&format=csv`:
  - JSON paginated view: `?status=MATCHED|UNMATCHED`, `?method=deterministic|agent|unresolved`, `?page` (default 1), `?page_size` (default 25, max 100), `total_count`/`total_pages` in the envelope
  - CSV export: `?format=csv` ignores pagination, streams the whole filtered set with `Content-Type: text/csv` and `Content-Disposition: attachment; filename="reconciliation_records_<merchantId>_<YYYY-MM-DD>.csv"` via `encoding/csv` (proper quoting for `reasoning` commas)
  - Scope: `LEFT JOIN LATERAL` to each vendor row's most recent `reconciliation_log` entry *regardless of method* — the tab's `Full Records` is the complete ledger, not the old `recent_matches` cap.

## 11. What's measured and reported

Per the buildathon's evaluation bar (throughput, measured accuracy, honest exception list), this project reports: **`throughput_per_sec` (records/sec) from the deterministic pass** (computed as `total_processed / (elapsed_ms/1000)`, surfaced in `ReconciliationResult.throughput_per_sec`), a **computed `match_rate`** (`matched_count / total_processed`) rather than a claimed one, and a **complete, non-cherry-picked exception list with reasoning for every unresolved record**, all persisted in the `reconciliation_log` audit table and exposed via both `/api/v1/demo/dashboard/:merchantId` (20 most recent) and `/api/v1/demo/records/:merchantId` (full paginated + CSV).

## 12. Known limitations / what's next

- The Phase-2 amount pre-filter (`0.90–1.02 × vendor.amount`) may exclude the true match in some lumped-settlement edge cases; tuning it without widening false positives is future work.
- A second gateway adapter would be a new `PaymentProvider` implementation (`internal/vendors/`), not a new architecture — the ledger and audit model are gateway-agnostic.
- `GEMINI_API_KEY` is optional; without it the system is fully functional as a deterministic reconciler (agent route degrades to `503`).
- The model alias `gemini-flash-lite-latest` tracks Google's moving alias; per Google's versioning it will auto-update to newer flash-lite point releases (pin to a dated version like `gemini-2.5-flash-lite-…` if you need reproducibility).
- `T+1` window (`+2d` deterministic, `+3d` agent) covers overnight + weekend/holiday slack; a true calendar-aware window (bank holidays) would be tighter but is not implemented.

