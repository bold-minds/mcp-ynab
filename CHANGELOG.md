# Changelog

All notable changes to `mcp-ynab` are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed (breaking, v0.2.0-rc.2 vs v0.2.0-rc.1)

- `ynab_status.savings_accounts` renamed to `ynab_status.liquid_accounts`, and the field now includes `checking` and `cash` account types alongside `savings`. The previous field only surfaced savings-type accounts, which missed users whose Ready-to-Assign cushion lives on a checking balance. Review finding L5.

### Fixed (v0.2.0-rc.1 code review)

High-severity (silent correctness bugs):
- **H1**: Task tools no longer inherit `ListTransactions`'s 500-row LLM-context trim. New `fetchTransactionsForAggregation` with a 50,000-row safety ceiling is used by `ynab_spending_check`, `ynab_weekly_checkin`, and `ynab_status`. `YnabSpendingCheckOutput.OnPlan` is now `*bool` and absent when `Truncated=true`, replaced by `VerdictUnavailableReason`.
- **H2**: `ynab_weekly_checkin` no longer double-counts transfers between on-budget accounts in its `income_received` and `total_outflows` sums. `Transaction.TransferAccountID` is now plumbed through from YNAB's wire field; transfer rows are filtered out at the aggregation boundary.
- **H3**: `ynab_status.unapproved_transaction_count` deduplicates transfer mirror rows via `countUnapprovedExcludingTransferMirrors`. An unapproved transfer now counts as 1 pending item, not 2.

Medium-severity:
- **M1**: `elicitConfirmation` distinguishes `context.Canceled` / `context.DeadlineExceeded` (abort) from other elicit errors (graceful-degrade as unsupported-client).
- **M2**: `update_transaction` elicitation message renders new category/payee UUIDs as `(new id=...)` instead of next to a name, making the name-vs-id asymmetry explicit.
- **M5**: `loadToken` `YNAB_API_TOKEN_FILE` reads are bounded at 4 KB via `io.LimitReader`.

Low-severity:
- **B2**: `simulateAvalanche` payoff-order tiebreak now compares APR (the code's intent) instead of `BalanceAtStart` (what it actually did). `DebtPayoffMilestone.APRPercent` surfaced in output.
- **B3/M3**: `Month.AgeOfMoney` and `MonthSummary.AgeOfMoney` changed from `int` to `*int` to preserve the null-vs-zero distinction. `ynab_weekly_checkin.age_of_money_delta_days` now correctly reports a delta of 0 for new plans where `age_of_money=0` legitimately.
- **B4**: `ynab_status` clamps debt account balances to zero when the user has a credit balance, matching `ynab_debt_snapshot`'s existing behavior.
- **L2**: Refused redirects log their target URL to stderr for operator debugging (the error returned to MCP clients is still status-only).
- **L3**: `deltaCache` enforces a per-plan-entry cap of 20,000 items. When hit, the entry is flushed and the next call resets to a fresh delta chain.
- **L4**: `progressedThisMonth` → `anyProgressFromInitial` (misleading name).
- **L5**: `ynab_status.savings_accounts` → `liquid_accounts` (see Changed above).
- **L6**: `debtAccountTypes` lifted from inline literal to package-level `var`.
- **L9**: `YnabDebtSnapshot.extra_per_month_milliunits` sanity-capped at 1 billion milliunits ($1M USD/month).
- **L14**: Introduced `clock.go` with an overridable package-level `nowUTC` function. All production time-dependent paths route through it; tests override for determinism via `t.Cleanup`.

Plus a handful of documentation clarifications: M4 (floor-division error magnitude), M6 (cross-category de-dup rationale), M7 (spec-verified `type + scope` combination), L1 (per-Client vs per-token rate limiter wording), L7 (`bearerRe` allowlist rationale), L8 (`list_categories` slice pre-sizing), L10 (month format string style), L11 (`IsSubtransaction` omitempty behavior), L12 (`CreateTransactionOutput.Before *struct{}` rationale), L13 (explicit `ctx.Err()` short-circuit in long task tools).

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

- **Delta sync** for unfiltered `list_accounts` and `list_transactions`: in-process cache keyed by `(plan_id, endpoint)`, passes `last_knowledge_of_server` on subsequent calls and merges deltas including deletions. Scope limited to unfiltered endpoints per v0.2 brief decision.
- **`Transaction` output type** gains `PayeeID` field so `ynab_spending_check`'s `excluded_payee_ids` has the data it needs to match.
- **`Account` output type** gains `LastReconciledAt *time.Time` for the `ynab_status` days-since-last-reconciled computation.
- `client.go:doJSON` is now a thin wrapper over `doJSONWithBody` so write and read paths share the same token injection, host lock, rate limiter, and error sanitization.

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

### Security

- Regression test `TestDoJSON_401DoesNotLeakBearerToken` verifies that a pathological YNAB 401 response with a token embedded in its `detail` field is scrubbed before reaching the MCP client.
- Regression test `TestLogLeak_PathologicalRoundTripper` verifies that a misbehaving inner HTTP transport that embeds the bearer token literally in its error string produces no token leakage through any of the 7 tool handlers.
- Subprocess test `TestSubprocess_SDKValidatesMissingRequiredArg` verifies that the MCP SDK rejects tool calls with missing required arguments at the protocol layer (JSON-RPC `-32602`), before any handler code runs.

[Unreleased]: https://github.com/bold-minds/mcp-ynab/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/bold-minds/mcp-ynab/releases/tag/v0.2.0
[0.1.0]: https://github.com/bold-minds/mcp-ynab/releases/tag/v0.1.0
