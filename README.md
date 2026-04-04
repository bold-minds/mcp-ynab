# mcp-ynab

[![CI](https://github.com/bold-minds/mcp-ynab/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/bold-minds/mcp-ynab/actions/workflows/ci.yml)
[![CodeQL](https://github.com/bold-minds/mcp-ynab/actions/workflows/codeql.yml/badge.svg?branch=main)](https://github.com/bold-minds/mcp-ynab/actions/workflows/codeql.yml)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/bold-minds/mcp-ynab/badge)](https://securityscorecards.dev/viewer/?uri=github.com/bold-minds/mcp-ynab)
[![Go Reference](https://pkg.go.dev/badge/github.com/bold-minds/mcp-ynab.svg)](https://pkg.go.dev/github.com/bold-minds/mcp-ynab)
[![Go Report Card](https://goreportcard.com/badge/github.com/bold-minds/mcp-ynab)](https://goreportcard.com/report/github.com/bold-minds/mcp-ynab)
[![Release](https://img.shields.io/github/v/release/bold-minds/mcp-ynab?include_prereleases&sort=semver)](https://github.com/bold-minds/mcp-ynab/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

A **read-only** Model Context Protocol server for the [YNAB](https://www.ynab.com) budgeting API. Lets an LLM (Claude Desktop, Cursor, Claude Code, or any MCP-compatible client) inspect your plans, accounts, categories, transactions, and monthly summaries — without the ability to modify anything.

Built in Go against the [official MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk). One binary, stdio transport, no inbound network surface, OS-keyring token storage.

## Why read-only

YNAB personal access tokens are long-lived and full-scope — they grant read *and* write on every plan on the account, and YNAB has no bulk-undo for mutations. An LLM that can call `deleteTransaction` is one prompt-injected transaction memo away from scrambling a budget with no recourse. This server deliberately exposes **only** the read endpoints, and the design has no code path that mutates YNAB data.

## Tools

| Tool                          | Description                                                                                                   |
| ----------------------------- | ------------------------------------------------------------------------------------------------------------- |
| `list_plans`                  | List all YNAB plans (called "budgets" in the YNAB UI) owned by the authenticated user.                        |
| `list_accounts`               | List accounts in a plan with current balances. Closed accounts are excluded by default.                       |
| `list_categories`             | List all categories in a plan with this month's assigned / activity / balance amounts and goal details.      |
| `list_transactions`           | List transactions for a plan, most recent first. Filter by `since_date`, approval state (`type`), or scope — **one of** `account_id`, `category_id`, or `payee_id`. Category / payee scoping flattens split transactions so each subtransaction line appears separately with `is_subtransaction: true`. Default limit 100, max 500. |
| `list_months`                 | Monthly rollup summaries (income, budgeted, activity, to_be_budgeted, age_of_money) for recent months, most recent first. Use for month-over-month trends. Default limit 6, max 60. |
| `get_month`                   | Full plan month with per-category breakdown. Accepts `"current"` or `YYYY-MM-01`. Use for the Sunday ritual / True Expenses check.|
| `list_scheduled_transactions` | Recurring and future-dated scheduled transactions in date_next order (soonest first). Optional `upcoming_days` filter.|

All tools advertise `readOnlyHint: true` in their MCP annotations.

### What each tool enables

| Use case                                                  | Tool(s)                                                       |
| --------------------------------------------------------- | ------------------------------------------------------------- |
| "What's my grocery spend this week?"                      | `list_categories` → find grocery id → `list_transactions` with `category_id` |
| Debt snapshot (all debt accounts + balances)              | `list_accounts` (filter to creditCard / loan / debt types)    |
| True Expenses check / overspending detection              | `list_categories` or `get_month`                              |
| Month-over-month trend ("am I spending more than last month?") | `list_months`                                                 |
| "What's coming up this month?" (rent, subscriptions)      | `list_scheduled_transactions` with `upcoming_days: 30`        |
| Ready-to-Assign for waterfall conversation                | `get_month` → `to_be_budgeted` field                          |
| Pattern detection across a payee                          | `list_transactions` with `payee_id`                           |
| Eating plan audit                                         | `list_categories` → grocery/restaurant ids → `list_transactions` with `category_id` and `since_date` |

### Money representation

Every monetary value in a tool response is a `Money` object with two fields:

```json
"balance": { "milliunits": 123456, "decimal": "123.456" }
```

- `milliunits` is the authoritative int64 value — YNAB's native format, exact across every currency.
- `decimal` is a pre-formatted string with 3 fractional digits (milliunit precision).

**No code path in this server uses `float64` for currency.** Formatting is performed via integer arithmetic only. See `money.go`.

## Install

### Homebrew / prebuilt binaries

Download a release from [GitHub Releases](https://github.com/bold-minds/mcp-ynab/releases) — static binaries for Linux, macOS, and Windows (amd64 and arm64).

### Docker

```bash
docker pull ghcr.io/bold-minds/mcp-ynab:latest
```

Distroless-based image (no shell, no package manager), runs as non-root, static binary, stdio-only — no exposed ports.

### From source

```bash
go install github.com/bold-minds/mcp-ynab@latest
```

Requires Go 1.25 or newer.

## Configure

### Get a YNAB personal access token

Log in to YNAB, go to **Account Settings → Developer Settings**, and create a **Personal Access Token**. Copy it once — you cannot retrieve it later.

### Provide the token to the server

Three options, in order of preference. The server checks them in the order below and uses the first source that provides a non-empty value.

#### 1. OS keyring (recommended)

Store the token once in your operating system's native credential store (macOS Keychain, Linux Secret Service, Windows Credential Manager):

```bash
echo -n "your-ynab-personal-access-token" | mcp-ynab store-token
```

The token is read from stdin — it never appears on the command line, so it never lands in shell history or `/proc/PID/cmdline`. Subsequent runs of `mcp-ynab` pick it up automatically with no environment variables needed.

To rotate, re-run `store-token` with the new value.

#### 2. File-based secret

Useful for Docker secrets, systemd `LoadCredential`, or Kubernetes secret volumes:

```bash
export YNAB_API_TOKEN_FILE=/run/secrets/ynab_token
```

Keep the file at `chmod 600`.

#### 3. Environment variable (plaintext)

The simplest option, but the token lives in plaintext in process environment:

```bash
export YNAB_API_TOKEN=your_personal_access_token
```

### Wire it into your MCP client

#### Claude Desktop

Edit `~/Library/Application Support/Claude/claude_desktop_config.json` (macOS) or the equivalent on your OS. If you have stored the token in the OS keyring, you don't need an `env` block at all:

```json
{
  "mcpServers": {
    "ynab": {
      "command": "/path/to/mcp-ynab"
    }
  }
}
```

If you prefer file-based:

```json
{
  "mcpServers": {
    "ynab": {
      "command": "/path/to/mcp-ynab",
      "env": {
        "YNAB_API_TOKEN_FILE": "/Users/you/.config/ynab/token"
      }
    }
  }
}
```

Do **not** paste the raw token into `claude_desktop_config.json` — that file is typically world-readable and may be backed up by iCloud / Time Machine. Use the keyring instead.

#### Cursor

Edit `~/.cursor/mcp.json`:

```json
{
  "mcpServers": {
    "ynab": {
      "command": "mcp-ynab"
    }
  }
}
```

(Token comes from the keyring.)

#### Other MCP clients

Any client that speaks the MCP stdio transport can launch `mcp-ynab` as a subprocess. Configure the command to run the binary and provide the token via `YNAB_API_TOKEN`, `YNAB_API_TOKEN_FILE`, or the OS keyring (recommended).

## Security model

### What this server protects against

| Threat                                                                      | Mitigation                                                                                                                                                |
| --------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Prompt-injected instructions telling the LLM to delete/modify financial data | **No write tools exist.** There is no code path that issues a `POST`, `PATCH`, `PUT`, or `DELETE` to YNAB.                                                |
| Token leakage via log statements / `%+v` on a config struct                 | Token is wrapped in a redacting `Token` type. `String`, `GoString`, `Format`, `MarshalJSON`, `MarshalText` all return `[REDACTED]`. Raw value only accessible via a package-private `reveal()` called in exactly ONE place. |
| Token leakage via HTTP-client errors (axios-style config-in-error pattern)  | Adversarial regression test: pathological RoundTripper returns errors containing the literal token; every tool's error path is asserted not to echo it.  |
| Exfiltration of the YNAB token via a rogue URL (SSRF / spec injection)       | Custom `http.RoundTripper` refuses any request whose hostname is not `api.ynab.com` (case-insensitive, port-tolerant). Strips `Authorization` defensively. Refuses all redirects. |
| Token leakage via YNAB error responses surfaced to the LLM                  | All YNAB errors go through `sanitize()`, which strips `Bearer <token>` and `Authorization:` patterns. No code path formats the raw token into an error.  |
| Runaway LLM exhausting YNAB's per-token rate limit (200 req/hour)            | Token-bucket rate limiter at **180 req/hour** (1 per 20 seconds, burst of 10). Enforced in the RoundTripper.                                              |
| Hung upstream                                                                | 30-second per-request timeout, 8 MB response body cap.                                                                                                    |
| Plaintext token storage                                                      | OS keyring is the recommended storage path; file-based and env-var are fallbacks documented with tradeoffs.                                                |
| Inbound network attack on the server                                         | stdio transport only — no listening socket, no HTTP endpoints.                                                                                             |
| Floating-point drift in money arithmetic                                     | All currency stored as int64 milliunits. Formatting is integer arithmetic only. No `float64` anywhere in the money path.                                   |

### Defense-in-depth

- **Token type**: `Token` struct blocks every standard format/serialize path. `UnmarshalJSON` and `UnmarshalText` refuse, so an attacker-controlled payload cannot inject a valid Token.
- **Schema validation**: tool arguments are validated against SDK-derived JSON Schemas before any handler runs. Missing required fields produce JSON-RPC `-32602 invalid params` protocol errors at the SDK layer — our handlers are never reached.
- **Error sanitization at the tool boundary**: every handler wraps its error through `sanitizedErr` before returning, as a final defense even if an inner error escapes without going through the client's own sanitization.
- **No shell execution**: no `os/exec` call anywhere in the credential or runtime path.
- **No hardcoded credentials**: tests use fake sentinel values.

### What it does NOT protect against

- A fully compromised upstream `api.ynab.com` (TLS verification is on, but no certificate pinning).
- A fully compromised MCP client (the client sees every tool response in plaintext).
- An adversarial user on your own machine with read access to your keyring / token file.
- The bare token value appearing in a YNAB error response body as a non-`Bearer`-prefixed substring (unlikely in practice).

## Roadmap

Deferred from v0.1 with explicit rationale:

- **Delta sync (`last_knowledge_of_server`)** — YNAB supports it on `/plans` and related endpoints. A full implementation would need stateful in-memory caching keyed by `(plan_id, endpoint)` and delta-merge logic. Not a security concern: every call is still 1 HTTP request whether delta sync is used or not — the benefit is smaller responses and less bandwidth, not fewer requests. Punted until the rate limiter is actually a bottleneck in practice.
- **Task-shaped tools** (`ynab_status`, `ynab_spending_check`, `ynab_weekly_checkin`) — a more LLM-friendly surface composed on top of the current API-shaped primitives. v0.1 ships with 7 API-shaped read tools; task-shaped tools can be added in v0.2 without breaking changes.
- **APR storage for debt projections** — YNAB does not track APRs. A config-file sidecar mapping `account_id → apr` would enable payoff projections. Not in v0.1; belongs with task-shaped tools.
- **Write tools** (`create_transaction`, `update_category_budgeted`, `approve_transaction`) — will ship in v0.2+ behind an explicit opt-in (`YNAB_ALLOW_WRITES=1`) with per-call confirmation via MCP elicitation and bounded amounts. Covers "log this coffee", Rule 3 money moves, and daily pending-transaction cleanup.

## Development

```bash
go test -race ./...        # run tests with race detector
go vet ./...
go build ./...
staticcheck ./...
govulncheck ./...
```

Docker build:

```bash
docker build -t mcp-ynab:dev --build-arg VERSION=dev .
```

### Running locally against the real YNAB API

```bash
echo -n "your-token" | mcp-ynab store-token   # one-time, uses OS keyring
./mcp-ynab                                     # run the server
```

The server reads JSON-RPC messages from stdin and writes responses to stdout. All logs go to stderr — the binary never touches stdout from its own code, or it would corrupt the transport framing.

### Adding a tool

1. Define input/output structs in `tools.go` with `json` and `jsonschema` tags.
2. Add a handler method on `*Client`.
3. Register it in `registerTools` with an appropriate `ToolAnnotations` hint.
4. Add a test in `tools_test.go` using the `testClient` helper.
5. Add a subprocess test in `subprocess_test.go` if the tool has required arguments, to confirm the SDK validation layer rejects missing fields.

The MCP SDK automatically derives JSON Schemas from struct types and validates incoming arguments before the handler runs.

## License

MIT — see [LICENSE](LICENSE).
