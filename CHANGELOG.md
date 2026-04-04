# Changelog

All notable changes to `mcp-ynab` are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] — Unreleased

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
- **Token-bucket rate limiter** — 1 request per 20 seconds with a burst of 10 (180 req/hour max, under YNAB's 200/hour ceiling).
- **OS keyring token storage** via `github.com/zalando/go-keyring`. `mcp-ynab store-token` reads from stdin and saves to the native credential store (macOS Keychain, Linux Secret Service, Windows Credential Manager). Token resolution order: `YNAB_API_TOKEN` → `YNAB_API_TOKEN_FILE` → keyring.
- **Error sanitization** (`errors.go`) — strips `Bearer <token>` and `Authorization:` patterns from any string forwarded to the MCP client. Applied at both the client layer and the tool-boundary layer as defense-in-depth.
- **stdio transport only** — no inbound network surface.
- **Distroless Docker image** — runs as non-root, no shell, no package manager, static binary.
- **CI** — `go test -race`, `go vet`, `staticcheck`, `govulncheck`, CodeQL, OpenSSF Scorecard.
- **Automated releases** via GoReleaser: cross-platform binaries (Linux/macOS/Windows × amd64/arm64) and multi-arch container images pushed to `ghcr.io/bold-minds/mcp-ynab`.

### Security

- Regression test `TestDoJSON_401DoesNotLeakBearerToken` verifies that a pathological YNAB 401 response with a token embedded in its `detail` field is scrubbed before reaching the MCP client.
- Regression test `TestLogLeak_PathologicalRoundTripper` verifies that a misbehaving inner HTTP transport that embeds the bearer token literally in its error string produces no token leakage through any of the 7 tool handlers.
- Subprocess test `TestSubprocess_SDKValidatesMissingRequiredArg` verifies that the MCP SDK rejects tool calls with missing required arguments at the protocol layer (JSON-RPC `-32602`), before any handler code runs.

[Unreleased]: https://github.com/bold-minds/mcp-ynab/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/bold-minds/mcp-ynab/releases/tag/v0.1.0
