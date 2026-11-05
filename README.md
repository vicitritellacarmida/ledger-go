# ledger-go

ledger-go is a double-entry ledger service for a payment marketplace. The marketplace in this project is fictional. Customers keep money in wallets, pay merchants, and get refunds. Merchants receive payouts. The platform keeps a fee from each payment.

The service records every movement of money as a journal entry. It keeps balances, reserves money with holds, rejects duplicate requests, and can prove that nobody changed its history.

It is written in Go with the standard library, MySQL 8, and two small dependencies.

## Contents

- [Accounting concepts for backend engineers](#accounting-concepts-for-backend-engineers)
- [Setup](#setup)
- [Running](#running)
- [Tests](#tests)
- [API](#api)
- [Error codes](#error-codes)
- [Project layout](#project-layout)
- [Design notes](#design-notes)
- [Current limitations](#current-limitations)

## Accounting concepts for backend engineers

This section explains the ideas behind the code. It assumes that you know databases and APIs, but not accounting.

### Why two sides for every movement

Money never appears from nothing and never disappears. If a customer pays 100,000 IDR, the wallet goes down by 100,000 and something else goes up by 100,000. Double-entry bookkeeping writes both sides every time. One side is a debit. The other side is a credit.

A journal entry is one business event. It has two or more postings. A posting is one line that changes one account. The rule of the ledger is simple: in each currency, the debits of an entry must equal its credits. If an entry breaks this rule, the service rejects it. Because every entry balances, the whole ledger always balances. A bug that creates or deletes money becomes visible at once.

### The five account types

Every account has one type. The types come from the accounting equation:

```
Assets + Expenses = Liabilities + Equity + Revenue
```

| Type | Meaning in this marketplace | Grows with |
|---|---|---|
| asset | Money the platform has, for example the bank account | debit |
| liability | Money the platform owes, for example customer wallets and merchant payables | credit |
| equity | Money the owners put in | credit |
| revenue | Money the platform earned, for example fees | credit |
| expense | Money the platform spent, for example processor fees | debit |

A customer wallet is a liability. This surprises many engineers. The money in the wallet belongs to the customer, so the platform owes it to them. When a customer tops up 150,000 IDR, two things happen. The bank account (an asset) goes up, and the debt to the customer (a liability) goes up by the same amount. Both sides of the equation grow, so it stays equal.

### Normal balance

The normal balance of an account is the side on which it grows. Assets and expenses have a debit normal balance. The other three types have a credit normal balance. The service derives the normal balance from the type. You do not set it.

### The signed convention

Inside the database, the ledger stores one signed number per posting:

- A debit is a positive number.
- A credit is a negative number.

With this convention, "debits equal credits" becomes "the amounts of an entry add up to zero". The column `accounts.posted_balance` holds the sum of all postings of the account, so it is debits minus credits.

That number is hard for people to read. A wallet with 150,000 IDR has a posted balance of -150,000. For this reason, the API never shows signed numbers. Postings carry a `side` (`debit` or `credit`) and a positive `amount`. Balances use the natural sign of the account. The natural sign makes a positive number mean "the account holds more". So the wallet shows `posted: 150000`.

### Posted, held and available

An account has three balance numbers:

- Posted: the sum of all journal entries. This is money that moved.
- Held: the total of open holds. A hold is a reservation of money for a payment that is not final yet. A card authorization is the classic example.
- Available: posted minus held. This is the money that the account can spend now.

A hold does not create a journal entry, because no money moved yet. It only lowers the available balance. At capture, the service creates a journal entry for the captured amount and releases the hold.

Most accounts do not allow an overdraft. For these accounts, the available balance can never go below zero. If a posting lowers the balance below zero, it fails with `insufficient_funds`. A posting that raises the balance is always accepted, so an account that is short can still get money. System accounts such as suspense and fee revenue allow an overdraft. The flag is per account (`allow_overdraft`).

### Corrections are new entries

Entries are immutable. The service never updates or deletes them, and database triggers refuse it too. To fix a mistake, you post a reversal. A reversal is a new entry with the same lines and opposite sides. It points back to the original entry. Both entries stay in the history, so an auditor can see what happened and the date of the fix.

### Effective date and closed periods

Each entry has an effective date. The effective date is the business date of the event. It can be earlier than the date the entry was written, for example for a late bank statement. Reports such as the trial balance and as-of balances use the effective date.

At the end of a month, finance closes the period. After that, no entry can have an effective date inside the closed month. Late corrections get a date in an open month. This keeps reports for closed months stable.

### Trial balance

A trial balance lists each account with its net debit or net credit as of a date. The total of the debit column must equal the total of the credit column in each currency. In this service the totals always match, because every entry balances. If they do not match, the data is corrupt.

### The marketplace flows

| Operation | Debit | Credit |
|---|---|---|
| Top-up of a wallet | bank (asset grows) | customer wallet (debt to customer grows) |
| Payment with fee | customer wallet (full amount) | merchant payable (amount minus fee), fee revenue (fee) |
| Payout to merchant | merchant payable | bank |
| Refund | merchant payable, and fee revenue if the fee is refunded | customer wallet, or refunds payable |
| Hold capture | held account | destination of the hold |

## Setup

You need:

1. Go 1.22 or later.
2. MySQL 8.0 or later.
3. A MySQL user that can create tables and triggers.

Create a database:

```sql
CREATE DATABASE ledger CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
```

Copy the example configuration and set the real DSN:

```sh
cp .env.example .env
# Edit .env and replace YOUR_PASSWORD_HERE in DATABASE_DSN.
```

If `DATABASE_DSN` still contains `YOUR_PASSWORD_HERE`, the server does not start. Real environment variables win over values in `.env`.

| Variable | Default | Meaning |
|---|---|---|
| `DATABASE_DSN` | placeholder DSN | MySQL DSN in go-sql-driver format |
| `HTTP_ADDR` | `:8080` | Listen address |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `LOG_FORMAT` | `json` | `json` or `text` |
| `BUSINESS_TIMEZONE` | `Asia/Jakarta` | Time zone that decides which day is "today" |
| `CURRENCIES` | `IDR,USD` | Currencies whose system accounts are seeded |
| `MIGRATE_ON_START` | `true` | Apply migrations when the server starts |
| `SEED_CHART` | `true` | Create missing system accounts when the server starts |
| `HOLD_EXPIRY_INTERVAL` | `30s` | How often due holds expire. `0s` disables the job |
| `SHUTDOWN_TIMEOUT` | `10s` | Time limit for graceful shutdown |
| `MAX_BODY_BYTES` | `1048576` | Largest accepted request body |

The service always sets `parseTime=true`, `loc=UTC`, `multiStatements=true` and the session time zone `+00:00`, whatever the DSN says.

## Running

```sh
make migrate   # apply migrations and seed the chart of accounts
make run       # start the HTTP server
```

The binary has three commands:

```sh
go run ./cmd/ledger serve          # default: HTTP server, also migrates when MIGRATE_ON_START=true
go run ./cmd/ledger migrate        # apply migrations, seed the chart, exit
go run ./cmd/ledger expire-holds   # expire due holds once, exit (for cron)
go run ./cmd/ledger -env other.env serve
```

### Chart of accounts

On startup the service creates these system accounts for each currency in `CURRENCIES`. The codes end with the currency in lower case.

| Code (IDR) | Type | Overdraft |
|---|---|---|
| `assets:bank:idr` | asset | no |
| `assets:psp-receivable:idr` | asset | yes |
| `assets:suspense:idr` | asset | yes |
| `liabilities:refunds-payable:idr` | liability | no |
| `equity:owner-capital:idr` | equity | yes |
| `revenue:fees:idr` | revenue | yes |
| `expenses:psp-fees:idr` | expense | yes |
| `expenses:rounding:idr` | expense | yes |

You create customer wallets and merchant payables through the API. The suggested codes are `liabilities:wallets:<customer>:<currency>` and `liabilities:merchants:<merchant>:<currency>`.

## Tests

```sh
make test               # unit tests, no database needed
make test-integration   # unit and integration tests against MySQL
make test-race          # the same with the race detector
make lint               # gofmt -l and go vet
```

`go test ./...` passes without a database. If `TEST_DATABASE_DSN` is not set, the integration tests skip with a message.

To run the integration tests, set `TEST_DATABASE_DSN`. The database name must end with `_test`:

```sh
TEST_DATABASE_DSN='root@tcp(127.0.0.1:3306)/ledger_test' go test ./...
```

The tests drop and create their own databases. For this reason, the helper refuses other names. Each test package uses its own database (`ledger_store_test`, `ledger_service_test`, `ledger_httpapi_test`), because `go test` runs packages in parallel.

The integration tests cover these areas:

- Each marketplace flow, each error path, and each hold state change.
- Concurrent payments against one wallet. Exactly the affordable number succeeds, and the wallet never goes negative.
- Concurrent transfers in opposite directions between two accounts, with no deadlock.
- Concurrent holds, captures, refunds and reversals.
- Concurrent HTTP requests with one idempotency key. The ledger posts exactly one entry.
- Tampering with a posting in the database. The audit endpoint finds the changed entry.

After most tests, a helper makes sure that the ledger is consistent. Each stored balance must equal the sum of its postings. The held amounts must match the open holds. The trial balance must balance, and the hash chain must be intact.

## API

All amounts are integers in minor units of the currency. IDR has no minor unit, so `150000` is 150,000 rupiah. USD has two decimals, so `150000` is 1,500.00 dollars. `GET /v1/currencies` lists the exponent of each currency.

Dates use the format `YYYY-MM-DD`. Timestamps use RFC 3339 in UTC.

Every `POST` request must have an `Idempotency-Key` header. See [Idempotency](#idempotency).

### Endpoints

| Method and path | Purpose |
|---|---|
| `GET /healthz` | Process is alive |
| `GET /readyz` | Database is reachable |
| `GET /v1/currencies` | Supported currencies and exponents |
| `POST /v1/accounts` | Create an account |
| `GET /v1/accounts` | List accounts. Filters: `type`, `currency`, `status`, `owner_ref`, `code_prefix`, `cursor`, `limit` |
| `GET /v1/accounts/{code}` | One account with its balances |
| `POST /v1/accounts/{code}/status` | Freeze, unfreeze or close an account |
| `GET /v1/accounts/{code}/balance` | Current balance, or the posted balance at the end of `as_of` |
| `GET /v1/accounts/{code}/statement` | Postings with running balance. Filters: `from`, `to`, `cursor`, `limit` |
| `GET /v1/accounts/{code}/holds` | Holds on the account. `state=open` shows only open holds |
| `POST /v1/entries` | Post a manual or adjustment entry |
| `GET /v1/entries` | List entries, newest first. Filters: `type`, `external_ref`, `cursor`, `limit` |
| `GET /v1/entries/{id}` | One entry, with `reversed_by` when it was reversed |
| `POST /v1/entries/{id}/reverse` | Reverse an entry |
| `POST /v1/holds` | Place a hold |
| `GET /v1/holds/{id}` | One hold |
| `POST /v1/holds/{id}/capture` | Capture all or part of a hold |
| `POST /v1/holds/{id}/void` | Cancel a hold |
| `POST /v1/holds/expire` | Expire all due holds now |
| `POST /v1/transfers/topup` | Top up a customer wallet |
| `POST /v1/transfers/payment` | Pay a merchant from a wallet, with an optional fee |
| `POST /v1/transfers/payout` | Pay a merchant balance out to the bank |
| `POST /v1/transfers/refund` | Refund all or part of a payment |
| `GET /v1/reports/trial-balance` | Trial balance. Filters: `as_of`, `currency` |
| `GET /v1/periods` | Closed periods |
| `POST /v1/periods/{period}/close` | Close a month, for example `2026-10` |
| `GET /v1/audit/verify` | Recompute the audit hash chain and report the first break |

List endpoints return `{"data": [...], "next_cursor": "..."}`. If `next_cursor` is present, pass it as `cursor` to get the next page. Treat the cursor as an opaque string.

### Examples

The responses below come from a real run. Some long fields are shortened with `...`.

Create a customer wallet:

```sh
curl -s -X POST localhost:8080/v1/accounts \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: acct-cust-1001' \
  -d '{"code":"liabilities:wallets:cust-1001:idr","name":"Wallet of customer 1001",
       "type":"liability","currency":"IDR","owner_ref":"cust-1001"}'
```

```json
{"id":17,"code":"liabilities:wallets:cust-1001:idr","name":"Wallet of customer 1001",
 "type":"liability","normal_balance":"credit","currency":"IDR","status":"active",
 "owner_ref":"cust-1001","allow_overdraft":false,
 "balance":{"posted":0,"held":0,"available":0,"posted_formatted":"0"},
 "created_at":"2026-11-05T06:12:01.566453Z","updated_at":"2026-11-05T06:12:01.566453Z"}
```

Create a merchant payable in the same way, with `"code":"liabilities:merchants:m-2001:idr"`.

Top up the wallet:

```sh
curl -s -X POST localhost:8080/v1/transfers/topup \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: topup-va-77001' \
  -d '{"wallet_account_code":"liabilities:wallets:cust-1001:idr","amount":150000,
       "currency":"IDR","external_ref":"va-77001"}'
```

```json
{"id":1,"seq":1,"type":"topup","description":"Wallet top-up","external_ref":"va-77001",
 "effective_date":"2026-11-05",
 "postings":[
   {"line":1,"account_code":"assets:bank:idr","side":"debit","amount":150000,"currency":"IDR","role":"funding"},
   {"line":2,"account_code":"liabilities:wallets:cust-1001:idr","side":"credit","amount":150000,"currency":"IDR","role":"payee"}],
 "prev_hash":"0000000000000000000000000000000000000000000000000000000000000000",
 "hash":"fff29a9e8341dbee...","created_at":"2026-11-05T06:12:01.583493Z"}
```

Pay a merchant with a 2.5% fee. `fee_bps` is in basis points (1 bp = 0.01%). You can send `fee_amount` instead. The fee is rounded half up.

```sh
curl -s -X POST localhost:8080/v1/transfers/payment \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: pay-order-5001' \
  -d '{"wallet_account_code":"liabilities:wallets:cust-1001:idr",
       "merchant_account_code":"liabilities:merchants:m-2001:idr",
       "amount":100000,"currency":"IDR","fee_bps":250,"external_ref":"order-5001"}'
```

```json
{"id":2,"seq":2,"type":"payment","description":"Payment to merchant","external_ref":"order-5001",
 "effective_date":"2026-11-05",
 "postings":[
   {"line":1,"account_code":"liabilities:wallets:cust-1001:idr","side":"debit","amount":100000,"currency":"IDR","role":"payer"},
   {"line":2,"account_code":"liabilities:merchants:m-2001:idr","side":"credit","amount":97500,"currency":"IDR","role":"payee"},
   {"line":3,"account_code":"revenue:fees:idr","side":"credit","amount":2500,"currency":"IDR","role":"fee"}],
 "prev_hash":"fff29a9e8341dbee...","hash":"70fab08ede6a7dfe...","created_at":"2026-11-05T06:12:01.596562Z"}
```

Place a hold for 15 minutes, then capture part of it. With `"final": true`, the service releases the rest of the hold.

```sh
curl -s -X POST localhost:8080/v1/holds \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: hold-order-5002' \
  -d '{"account_code":"liabilities:wallets:cust-1001:idr",
       "destination_account_code":"liabilities:merchants:m-2001:idr",
       "amount":20000,"ttl_seconds":900,"external_ref":"order-5002"}'

curl -s -X POST localhost:8080/v1/holds/1/capture \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: capture-order-5002' \
  -d '{"amount":15000,"final":true}'
```

```json
{"hold":{"id":1,"account_code":"liabilities:wallets:cust-1001:idr",
  "destination_account_code":"liabilities:merchants:m-2001:idr","currency":"IDR",
  "amount":20000,"captured_amount":15000,"remaining_amount":0,"state":"captured", "...": "..."},
 "entry":{"id":3,"seq":3,"type":"hold_capture","hold_id":1,
  "postings":[
    {"line":1,"account_code":"liabilities:wallets:cust-1001:idr","side":"debit","amount":15000,"currency":"IDR","role":"payer"},
    {"line":2,"account_code":"liabilities:merchants:m-2001:idr","side":"credit","amount":15000,"currency":"IDR","role":"payee"}],
  "...": "..."}}
```

Hold fields: send `expires_at` (RFC 3339) or `ttl_seconds`, not both. The default is 7 days and the maximum is 30 days. If a capture has no `amount`, the service captures all of the remaining amount.

The hold states and their changes:

```
active ──capture (part, final=false)──> partially_captured
active / partially_captured ──capture (final, or the full rest)──> captured
active / partially_captured ──void──> voided
active / partially_captured ──expire (after expires_at)──> expired
```

The states `captured`, `voided` and `expired` are final.

Refund 40,000 of the payment and give back the same share of the fee:

```sh
curl -s -X POST localhost:8080/v1/transfers/refund \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: refund-order-5001-1' \
  -d '{"payment_entry_id":2,"amount":40000,"refund_fee":true}'
```

```json
{"id":4,"seq":4,"type":"refund","related_entry_id":2,
 "postings":[
   {"line":1,"account_code":"liabilities:merchants:m-2001:idr","side":"debit","amount":39000,"currency":"IDR","role":"payee"},
   {"line":2,"account_code":"revenue:fees:idr","side":"debit","amount":1000,"currency":"IDR","role":"fee"},
   {"line":3,"account_code":"liabilities:wallets:cust-1001:idr","side":"credit","amount":40000,"currency":"IDR","role":"payer"}],
 "...": "..."}
```

Refund fields: if you leave out `amount`, the service refunds everything that is not refunded yet. `destination` is `wallet` (the default) or `original_method`. The value `original_method` credits `liabilities:refunds-payable:<cur>` instead of the wallet. The total of all refunds of a payment cannot be more than the payment. A reversed refund does not count.

Pay the merchant out:

```sh
curl -s -X POST localhost:8080/v1/transfers/payout \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: payout-m-2001-2026-11-05' \
  -d '{"merchant_account_code":"liabilities:merchants:m-2001:idr","amount":50000,"currency":"IDR"}'
```

Post a manual entry and reverse it:

```sh
curl -s -X POST localhost:8080/v1/entries \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: capital-2026-11' \
  -d '{"type":"manual","description":"Owner capital injection",
       "postings":[
         {"account_code":"assets:bank:idr","side":"debit","amount":1000000},
         {"account_code":"equity:owner-capital:idr","side":"credit","amount":1000000}]}'

curl -s -X POST localhost:8080/v1/entries/6/reverse \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: reverse-6' \
  -d '{"description":"Booked twice by mistake"}'
```

```json
{"id":7,"seq":7,"type":"reversal","description":"Booked twice by mistake","effective_date":"2026-11-05",
 "reversal_of":6,
 "postings":[
   {"line":1,"account_code":"assets:bank:idr","side":"credit","amount":1000000,"currency":"IDR"},
   {"line":2,"account_code":"equity:owner-capital:idr","side":"debit","amount":1000000,"currency":"IDR"}],
 "...": "..."}
```

Manual entry fields: `type` is `manual` (the default) or `adjustment`. The other types come only from the transfer and hold endpoints, so they always follow their rules. `currency` on a posting defaults to the currency of the account. `effective_date` defaults to today in `BUSINESS_TIMEZONE` and cannot be in the future.

Reversal rules: an entry can be reversed once. A reversal cannot be reversed. If a payment has refunds, reverse the refunds first. If you send no `effective_date`, the reversal is dated today.

Read a balance and a statement:

```sh
curl -s localhost:8080/v1/accounts/liabilities:wallets:cust-1001:idr/balance
curl -s 'localhost:8080/v1/accounts/liabilities:wallets:cust-1001:idr/balance?as_of=2026-10-31'
curl -s 'localhost:8080/v1/accounts/liabilities:wallets:cust-1001:idr/statement?limit=2'
```

```json
{"account_code":"liabilities:wallets:cust-1001:idr","currency":"IDR",
 "posted":75000,"held":0,"available":75000,"posted_formatted":"75000"}
```

```json
{"account_code":"liabilities:wallets:cust-1001:idr","currency":"IDR","normal_balance":"credit",
 "opening_balance":0,"closing_balance":50000,
 "lines":[
   {"posting_id":2,"entry_id":1,"entry_type":"topup","description":"Wallet top-up","external_ref":"va-77001",
    "effective_date":"2026-11-05","side":"credit","amount":150000,"running_balance":150000,"created_at":"..."},
   {"posting_id":3,"entry_id":2,"entry_type":"payment","description":"Payment to merchant","external_ref":"order-5001",
    "effective_date":"2026-11-05","side":"debit","amount":100000,"running_balance":50000,"created_at":"..."}],
 "next_cursor":"MjAyNi0xMS0wNXwz"}
```

A balance with `as_of` has only `posted`, because holds have no effective date. A statement is sorted by effective date, then by posting id. The running balance continues across pages.

Trial balance, period close and audit report:

```sh
curl -s 'localhost:8080/v1/reports/trial-balance?currency=IDR'
curl -s -X POST localhost:8080/v1/periods/2026-10/close -H 'Idempotency-Key: close-2026-10'
curl -s localhost:8080/v1/audit/verify
```

```json
{"as_of":"2026-11-05","balanced":true,"currencies":[{"currency":"IDR",
  "total_debit":100000,"total_credit":100000,"balanced":true,
  "lines":[
    {"account_code":"assets:bank:idr","name":"Platform bank account (IDR)","type":"asset","debit":100000,"credit":0},
    {"account_code":"liabilities:merchants:m-2001:idr","name":"Payable to merchant 2001","type":"liability","debit":0,"credit":23500},
    {"account_code":"liabilities:wallets:cust-1001:idr","name":"Wallet of customer 1001","type":"liability","debit":0,"credit":75000},
    {"account_code":"revenue:fees:idr","name":"Platform fee revenue (IDR)","type":"revenue","debit":0,"credit":1500}]}]}
```

```json
{"period":"2026-10","status":"closed","closed_at":"2026-11-05T06:12:01.680852Z"}
```

```json
{"ok":true,"checked_entries":7,"head_seq":7}
```

After the last day of a month, you can close it. If the audit endpoint finds a problem, `ok` is `false` and `first_broken` names the entry and the reason.

Freeze an account:

```sh
curl -s -X POST localhost:8080/v1/accounts/liabilities:wallets:cust-1001:idr/status \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: freeze-cust-1001' \
  -d '{"status":"frozen"}'
```

A frozen account rejects postings, new holds and captures. You can still void its holds, and its holds still expire. Send `"status":"active"` to unfreeze. If the balance is zero and no holds are open, you can close the account. A closed account cannot open again.

### Idempotency

If the network fails, a client retries the request. Without protection, a retried payment is booked twice. The `Idempotency-Key` header prevents this. Use a new unique key for each operation, for example a UUID or `payment-<order id>`.

The service handles a key as follows:

| Case | Result |
|---|---|
| New key | The service runs the request and stores the response with the key |
| Same key, same request | The service returns the stored response with the header `Idempotent-Replayed: true` |
| Same key, different request | `422 idempotency_key_reused` |
| Same key, sent again while the first request still runs | The second request waits for the first, then gets the stored response |
| No key on a `POST` | `400 idempotency_key_required` |

A request is "the same" when the method, the path and the JSON body are equal. Key order and spaces in the JSON do not matter. Keys are case-sensitive and can have up to 255 printable ASCII characters.

The service stores 4xx responses too. A retry of a rejected payment gets the same rejection and does not run again. The service does not store 5xx responses, so you can retry them with the same key.

The key row is written in the same database transaction as the business change. This has two results. If the process stops in the middle, there is no booked entry without a stored response. If two requests with one key arrive at the same time, the unique index makes the second one wait for the first.

### Errors

Every error has the same body:

```json
{"error":{"code":"insufficient_funds",
          "message":"account liabilities:merchants:m-2001:idr has insufficient available balance",
          "details":{"account_code":"liabilities:merchants:m-2001:idr","available":23500,"required":999999}},
 "request_id":"a8277f89-aed6-47da-a333-061d6cd1f00a"}
```

Clients use `code` for decisions. The `message` text can change. Every response has an `X-Request-Id` header. If the request has a safe `X-Request-Id`, the service uses it again.

## Error codes

| HTTP | Code | Meaning |
|---|---|---|
| 400 | `validation_failed` | A field is missing or wrong. `details.field` names it |
| 400 | `invalid_json` | The body is not valid JSON, or it has an unknown field |
| 400 | `unbalanced_entry` | Debits and credits differ. `details.difference_by_currency` shows how much |
| 400 | `currency_mismatch` | The currency does not match the account |
| 400 | `unknown_currency` | The currency is not supported |
| 400 | `idempotency_key_required` | The `Idempotency-Key` header is missing or invalid |
| 404 | `account_not_found`, `entry_not_found`, `hold_not_found` | The resource does not exist |
| 404 | `route_not_found` | No such endpoint |
| 409 | `already_exists` | The account code exists, or the period is already closed |
| 409 | `invalid_state_transition` | The hold or account cannot change to that state |
| 409 | `entry_already_reversed` | The entry has a reversal already |
| 409 | `idempotency_key_in_progress` | Rare: the stored response is not complete yet |
| 413 | `body_too_large` | The body is larger than `MAX_BODY_BYTES` |
| 422 | `insufficient_funds` | The available balance is too low |
| 422 | `account_not_active` | The account is frozen or closed |
| 422 | `account_not_empty` | An account with a balance or holds cannot close |
| 422 | `period_closed` | The effective date is in a closed month |
| 422 | `period_not_ended` | The month has not ended yet |
| 422 | `effective_date_in_future` | The effective date is after today |
| 422 | `entry_not_reversible` | The entry is a reversal, or a payment with refunds |
| 422 | `hold_expired` | The hold expired before the capture |
| 422 | `capture_exceeds_hold` | The capture amount is more than the rest of the hold |
| 422 | `refund_exceeds_payment` | The refund is more than the refundable amount |
| 422 | `entry_not_a_payment` | The refund points to an entry that is not a payment |
| 422 | `amount_overflow` | A balance goes past 2^53-1 |
| 422 | `idempotency_key_reused` | The key was used with a different request |
| 500 | `internal_error` | An unexpected error. The server log has the details |
| 503 | `service_busy` | Lock contention did not clear after retries. Retry with the same key |

## Project layout

```
cmd/ledger/          main: serve, migrate and expire-holds commands
internal/config/     configuration from environment variables and .env
internal/domain/     business rules with no database: accounts, entries, holds,
                     periods, money arithmetic, hash chain, transfer postings
internal/store/      MySQL repository with database/sql, transactions in context
internal/service/    use cases: locking, posting, holds, transfers, reports
internal/httpapi/    HTTP handlers, JSON types, error mapping, idempotency
internal/migrate/    migration runner with a schema_migrations table
internal/testutil/   MySQL helper for integration tests
migrations/          SQL migrations, embedded into the binary
```

A request goes through the layers in one direction: `httpapi`, then `service`, then `domain` and `store`. The `domain` package imports no other package of the project. You can test its rules without a database.

## Design notes

### Locks and deadlocks

Two transfers can run at the same time in opposite directions, A to B and B to A. If each one locks its source account first, each waits for the other forever. This is a deadlock.

To prevent this, every operation takes its locks in one global order:

1. The idempotency key row (in the HTTP layer).
2. The hold row (hold operations only).
3. The account rows, in ascending id order, with `SELECT ... FOR UPDATE`.
4. The `chain_head` row (only for operations that write an entry or close a period).

Transactions use READ COMMITTED isolation. After a transaction waits for a lock, its next read sees what the other transaction committed. For example, a refund then sees the earlier refunds of the same payment. If MySQL still reports a deadlock or a lock wait timeout, the service runs the transaction again, up to four times.

### Audit hash chain

Each entry stores a SHA-256 hash of the previous hash plus the canonical content of the entry. The content covers the sequence number, type, description, references, dates and every posting. If someone changes one stored entry, its hash no longer matches. Each later entry also stops matching, because it covers the changed hash. `GET /v1/audit/verify` computes the chain again and reports the first broken entry. It also makes sure that the `chain_head` row points to the last entry, so it finds deleted entries at the end.

The chain needs a strict order. For this reason, each transaction that appends an entry locks the single `chain_head` row. All postings therefore run one at a time at the final step. See the limitations below.

### Immutability

The application never runs `UPDATE` or `DELETE` on `journal_entries` or `postings`. Migration `0005` adds triggers that refuse both statements. The hash chain finds changes made by someone who removes the triggers.

## Current limitations

- One lock on `chain_head` makes all postings run in sequence. This keeps the hash chain simple. It also limits throughput to one writer at a time. Several chains (for example one per currency) or hashing in batches are possible next steps.
- Amounts are limited to 2^53-1 minor units, so JSON clients in JavaScript read them without loss.
- An entry can use an account only once. Two lines on the same account must be merged first.
- The service has no foreign exchange logic. One entry can hold several currencies, but each currency must balance alone.
- A closed period cannot open again through the API.
- A historical balance (`as_of`) shows the posted amount only, because holds have no effective date.
- The statement for an empty page with a `to` filter reports the balance at the cursor or `from` position, not at `to`.
- On a partial refund with `refund_fee`, the fee share is rounded down each time. Several partial refunds can therefore return slightly less fee than one full refund.
- The service has no authentication or authorization. Put it behind a gateway that does this.
- The service keeps idempotency keys forever. `store.DeleteIdempotencyKeysBefore` exists, but no job calls it yet.
- On MySQL servers with binary logging, migration `0005` needs the `SUPER` privilege or `log_bin_trust_function_creators = 1`. Managed databases often need the second option.
- MySQL commits DDL at once, so a migration that fails in the middle is not rolled back. Keep each migration file small.
