# Changelog

All notable changes to `mcp-ynab` are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.2.0] — 2026-04-04

Major feature release. Adds write tools (opt-in), 5 task-shaped tools,
delta sync, and a new `list_payees` read tool. Doubles the codebase
test count from 49 to 160+. See [README.md](README.md) for the
complete tool surface.

### Added

- **4 write tools, gated behind `YNAB_ALLOW_WRITES=1`:**
  - `create_transaction` — posts a new transaction with optional `import_id` for idempotent retries. Asks the MCP client to confirm via elicitation before executing.
  - `update_category_budgeted` — the primitive for Rule 3 money moves during the Sunday ritual. Returns before/after snapshots of budgeted and balance for the skill to persist an audit entry from.
  - `update_transaction` — partial update of a transaction (category, payee, memo, approved, cleared, flag color). **Amount changes are structurally disallowed**: the input struct has no amount field, enforced by a reflection-based regression test.
  - `approve_transaction` — convenience wrapper over `update_transaction` with `approved: true`. Deliberately **skips per-call elicitation** to support batch daily pending-cleanup workflows.
- **5 task-shaped composition tools (read-only):**
  - `ynab_status` — one-call Sunday ritual dashboard (Ready-to-Assign, overspent categories with credit card payment filtering, debt accounts with optional APR enrichment, savings accounts, days-since-last-reconciled, unapproved count, next-7-days scheduled cash flow with recurrence expansion).
  - `ynab_spending_check` — "did I stay on plan?" with `excluded_payee_ids` for carve-outs.
  - `ynab_weekly_checkin` — week-over-week income/outflow comparison plus month-over-month newly-overspent categories. Explicit `period_grouping_note` field communicates the mixed week/month scope.
  - `ynab_debt_snapshot` — avalanche payoff simulation using integer basis-points arithmetic (no floats in the compounding loop). Structured negative-amortization error with shortfall amount when minimums can't cover interest.
  - `ynab_waterfall_assignment` — **advisory-only** pure math over caller-supplied priority tiers with per-category `need_milliunits`. Issues no writes.
- **`list_payees`** read tool with case-insensitive `name_contains` substring filter. Unblocks the `excluded_payee_ids` feature on `ynab_spending_check`.
- **`recurrence.go`** — pure-function occurrence iterator for all 13 YNAB scheduled-transaction frequency enum values. One test per frequency, fail-closed on unknown values.
- **Redacting `Token` type** already present from v0.1.0 extended to all new write paths via the shared `doJSONWithBody` helper in `client.go`.
- **`docs/ASSUMPTIONS.md`** — new documentation file listing non-obvious assumptions about the YNAB API, notably the English-only "Credit Card Payments" group name match.

### Changed

- **Delta sync** for unfiltered `list_accounts` and `list_transactions`: in-process cache keyed by `(plan_id, endpoint)`, passes `last_knowledge_of_server` on subsequent calls and merges deltas including deletions. Scope limited to unfiltered endpoints per v0.2 brief decision. Per-entry size cap of 20,000 items; on overflow the entry is flushed and the next call starts a fresh delta chain.
- **`Transaction` output type** gains `PayeeID` and `TransferAccountID` fields. `PayeeID` unblocks `ynab_spending_check.excluded_payee_ids`; `TransferAccountID` lets task tools exclude transfer-mirror rows from income/outflow aggregations.
- **`Account` output type** gains `LastReconciledAt *time.Time` for the `ynab_status` days-since-last-reconciled computation.
- **`Month.AgeOfMoney` and `MonthSummary.AgeOfMoney`** are `*int` (nullable) rather than `int` to preserve YNAB's null-vs-zero distinction — a new plan legitimately has `age_of_money=0` and should not be confused with "YNAB hasn't computed it yet".
- **`YnabSpendingCheckOutput.OnPlan`** is `*bool` and absent when the aggregation hit its 50,000-row safety ceiling; in that case `Truncated=true` and `VerdictUnavailableReason` explains why no verdict is given. Refusing to give an on_plan answer on incomplete data is the whole point — returning a wrong verdict is worse than returning none.
- `client.go:doJSON` is now a thin wrapper over `doJSONWithBody` so write and read paths share the same token injection, host lock, rate limiter, and error sanitization.
- `client.go` introduces an internal `fetchTransactions` / `fetchTransactionsForAggregation` split: the user-facing `list_transactions` keeps its 500-row LLM-context trim for response-size hygiene, while task-shaped tools get the full set (up to the 50,000-row safety ceiling) for correct sum/count math.
- `clock.go` introduces a package-level `nowUTC` function that all production time-sensitive paths call instead of `time.Now()`. Tests override it via `t.Cleanup` for deterministic dates.

### Security

- **Write gate is bimodal**: without `YNAB_ALLOW_WRITES=1`, write tools are NOT REGISTERED at startup — they do not appear in `tools/list` and cannot be invoked at all. With the env var set, each handler also performs a per-call re-check as defense-in-depth.
- **Universal amount safety cap**: writes with `|amount| > 10_000_000` milliunits (=$10K USD) require an `amount_override_milliunits` argument equal to the main amount as an explicit echo-back acknowledgment.
- **MCP elicitation** per write call using the go-sdk's `ServerSession.Elicit` API (verified stable in v1.4.1). Graceful degradation: clients that don't support elicitation fall back to the env-var gate as the sole defense.
- **Extended log-leak regression** (`TestLogLeak_PathologicalRoundTripper`) now covers all 12 currently-registered tools (8 reads + 4 writes) and also echoes the request body in its adversarial error to exercise the write payload path.
- **New regression** (`TestLogLeak_WriteBodyNotEchoedInError`) documents the current sanitize() behavior for user-body content like transaction memos; flips visibly if future scrubbing rules change.
- **Subprocess test** now covers both the env-unset path (writes not registered) and the env-set path (writes registered with `readOnlyHint: false`).

### Known limitations

Documented in README and [docs/ASSUMPTIONS.md](docs/ASSUMPTIONS.md):

- Delta sync only applies to **unfiltered** read endpoints. Filtered `list_transactions` (with `since_date`, `type`, or scope args) always does a full fetch. YNAB's delta semantics on filtered endpoints are under-documented.
- Delta cache has **no TTL or size cap** in v0.2.0. Cache grows with session activity and dies with the process.
- `ynab_status` credit-card filter matches on the English string `"Credit Card Payments"`. YNAB has been English-only for 15+ years; a future localization would require updating `ynabStatusCreditCardPaymentGroupName` in `tools_tasks.go`.
- `twiceAMonth` scheduled frequency is approximated as 15-day advance in the recurrence iterator because YNAB's API doesn't expose the user's two anchor days.
- Amount safety cap is currency-agnostic at 10M milliunits. Correct for USD; tighter than intended for plans with very different subunit scales.

## [0.1.0] — 2026-04-04

Initial release. Read-only MCP server for the YNAB budgeting API.

### Added

- **7 read-only MCP tools**:
  - `list_plans` — all YNAB plans for the authenticated user.
  - `list_accounts` — accounts with current balances; closed accounts filtered by default.
  - `list_categories` — categories with current-month budgeted / activity / balance and goal details.
  - `list_transactions` — transactions with optional `since_date`, `type`, `limit`, and one of `account_id` / `category_id` / `payee_id` scope filters. Category and payee filters flatten split transactions, tagging subtransaction rows with `is_subtransaction: true`.
  - `list_months` — monthly rollup summaries for recent months, most recent first (default 6, max 60), for month-over-month trend questions.
  - `get_month` — full plan month with per-category breakdown. Accepts `"current"` or `YYYY-MM-01`.
  - `list_scheduled_transactions` — recurring and future-dated scheduled transactions in `date_next` order, with optional `upcoming_days` filter.
- **Redacting `Token` type** (`token.go`) — all `fmt.Stringer`, `fmt.GoStringer`, `fmt.Formatter`, `json.Marshaler`, and `encoding.TextMarshaler` paths return `[REDACTED]`. Raw value accessible only via package-private `reveal()`, called in exactly one place.
- **`Money` type** (`money.go`) — int64 milliunits plus pre-formatted decimal string with 3 fractional digits. No `float64` anywhere in the money path; formatting uses integer arithmetic only.
- **Host-locked HTTP transport** — refuses any request whose hostname is not `api.ynab.com` (case-insensitive via `url.URL.Hostname` + `strings.EqualFold`, port-tolerant). Strips `Authorization` header defensively on refusal. Refuses all HTTP redirects.
- **Token-bucket rate limiter** — 1 request per 20 seconds with a burst of 10. Max 190 requests per hour (burst + refill), steady-state 180/hour refill rate. Stays under YNAB's 200/hour ceiling.
- **OS keyring token storage** via `github.com/zalando/go-keyring`. `mcp-ynab store-token` reads from stdin and saves to the native credential store (macOS Keychain, Linux Secret Service, Windows Credential Manager). Token resolution order: `YNAB_API_TOKEN` → `YNAB_API_TOKEN_FILE` → keyring.
- **Error sanitization** (`errors.go`) — strips `Bearer <token>` and `Authorization:` patterns from any string forwarded to the MCP client. Applied at both the client layer and the tool-boundary layer as defense-in-depth.
- **stdio transport only** — no inbound network surface.
- **Distroless Docker image** — runs as non-root, no shell, no package manager, static binary. Base image pinned to content-addressable digest.
- **CI** — `go test -race`, `go vet`, `staticcheck`, `govulncheck`, CodeQL (security-extended + security-and-quality), OpenSSF Scorecard. All workflow actions pinned to commit SHAs for supply-chain integrity. Per-job minimal `permissions:` blocks.
- **Automated releases** via GoReleaser: cross-platform binaries (Linux/macOS/Windows × amd64/arm64) and multi-arch container images pushed to `ghcr.io/bold-minds/mcp-ynab`.

### Fixed

Correctness issues discovered during internal code review:

- **Silent aggregation truncation**: task tools (`ynab_spending_check`, `ynab_weekly_checkin`, `ynab_status`) previously inherited `ListTransactions`'s 500-row LLM-context trim and could produce wrong totals, wrong on-plan verdicts, and wrong unapproved counts on plans with more than 500 matching transactions in a scope. Now route through a dedicated aggregation helper with a 50,000-row safety ceiling.
- **Transfer double-counting in `ynab_weekly_checkin`**: transfers between on-budget accounts appear as two mirrored transactions (one positive, one negative). Both sides were being summed into both `income_received` and `total_outflows`, so a $5K checking→savings transfer would inflate both totals by $5K. Transfer rows are now filtered at the aggregation boundary.
- **Transfer double-counting in `ynab_status.unapproved_transaction_count`**: unapproved transfers counted as 2 pending items instead of 1. Fixed via `countUnapprovedExcludingTransferMirrors`.
- **Debt credit-balance rendering**: `ynab_status` showed a user with an overpaid credit card as owing negative dollars. Now clamps to zero, matching `ynab_debt_snapshot`'s existing behavior.
- **Avalanche payoff tiebreak**: `simulateAvalanche`'s same-month tiebreak comment claimed "higher APR first" but the code compared `BalanceAtStart`. Now threads APR through the simulation and uses it as the actual tiebreak.
- **`age_of_money_delta_days` suppressed legitimate zeros**: presence check used `!= 0` rather than `!= nil`, so a brand-new plan with `age_of_money=0` would never produce a delta. Fixed via the `*int` nullability change above.
- **`elicitConfirmation` failed open on cancelled contexts**: any `Elicit` error routed to the "client doesn't support elicitation" graceful-degrade path, including `context.Canceled` and `context.DeadlineExceeded`. Now distinguishes cancellation (abort the write) from unsupported-client (log + proceed).
- **`update_transaction` elicitation message showed UUIDs as names**: rendered "category: Groceries → a1b2c3d4..." making the new side look like a name. Now uses `(new id=...)` labelling to make the asymmetry explicit.
- **`loadToken` unbounded file read**: `YNAB_API_TOKEN_FILE` was read via `os.ReadFile` with no size limit. A misconfigured path (e.g. `/dev/urandom`) would exhaust memory. Now bounded at 4 KB via `io.LimitReader`.
- **`YnabDebtSnapshot.extra_per_month_milliunits` had no sanity bound**: a pathological caller passing `math.MaxInt64/2` would not be rejected at the entry point. Now capped at 1 billion milliunits ($1M USD/month) with an actionable error message.

### Security

- Regression test `TestDoJSON_401DoesNotLeakBearerToken` verifies that a pathological YNAB 401 response with a token embedded in its `detail` field is scrubbed before reaching the MCP client.
- Regression test `TestLogLeak_PathologicalRoundTripper` covers all 12 currently-registered tools (8 reads + 4 writes) with an adversarial transport that echoes both the Bearer token and the request body in its error. No tool's error path leaks the token.
- Regression test `TestLogLeak_WriteBodyNotEchoedInError` documents the current sanitize() behavior for user-body content like transaction memos.
- Subprocess test `TestSubprocess_SDKValidatesMissingRequiredArg` verifies that the MCP SDK rejects tool calls with missing required arguments at the protocol layer (JSON-RPC `-32602`), before any handler code runs.
- Refused redirects log the attempted target URL to stderr so operators can diagnose "why is YNAB returning 3xx" without the error leaking via MCP-visible surfaces.
- New regression tests for aggregation truncation bounds, transfer exclusion in both weekly_checkin and status, debt credit-balance clamping, age_of_money null-vs-zero semantics, delta cache size cap, task tool context cancellation short-circuit, and deterministic clock override.

[Unreleased]: https://github.com/bold-minds/mcp-ynab/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/bold-minds/mcp-ynab/releases/tag/v0.2.0
[0.1.0]: https://github.com/bold-minds/mcp-ynab/releases/tag/v0.1.0
