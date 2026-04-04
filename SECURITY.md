# Security Policy

`mcp-ynab` mediates access to a financial API. Security issues are taken seriously.

## Supported versions

Only the **latest released minor version** receives security patches. Older versions should be upgraded.

| Version     | Supported |
| ----------- | --------- |
| `0.1.x`     | ✅        |
| older       | ❌        |

## Reporting a vulnerability

**Do not open a public GitHub issue for security problems.**

Report privately via **[GitHub Security Advisories](https://github.com/bold-minds/mcp-ynab/security/advisories/new)**. This creates a confidential channel between you and the maintainers.

If for any reason the Security Advisories flow is unavailable, email the maintainers at the address listed in `go.mod`'s module path owner's GitHub profile.

### What to include

- A description of the issue and its impact
- Steps to reproduce or a proof-of-concept (code, tool invocation, or sample input)
- The version of `mcp-ynab` affected (`mcp-ynab version` output)
- Your Go version and OS, if relevant
- Any suggested mitigation

## In scope

Issues that affect the confidentiality, integrity, or availability of a user's YNAB data via this server:

- **Token leakage** through any code path — logs, errors surfaced to MCP clients, serialized structs, stack traces, Go fmt verbs, JSON output, env inheritance to subprocesses, keyring misuse.
- **Host-lock bypass** — any way to cause the outbound `Authorization` header to reach a host other than `api.ynab.com` (including via redirects, URL parser edge cases, case variants, port variants, homograph attacks).
- **SSRF** via a tool argument, a YNAB response field, or the OpenAPI spec supply chain.
- **Rate-limit bypass** that could be used to exhaust a user's 200/hr YNAB quota.
- **Input-validation bypass** that allows an LLM-supplied argument to trigger behavior outside the declared tool contract (including path traversal in `plan_id`, injection into query params, integer overflow in amount formatting).
- **Write gate bypass** (v0.2+): any way to invoke write tools (`create_transaction`, `update_category_budgeted`, `update_transaction`, `approve_transaction`) without `YNAB_ALLOW_WRITES=1`, or any way to issue writes to YNAB without going through a registered handler.
- **Amount safety bypass** (v0.2+): any way to submit a write with `|amount| > 10_000_000` milliunits without a matching `amount_override_milliunits` echo-back.
- **Structural amount immutability bypass** (v0.2+): any way to change a transaction's amount via `update_transaction`. The input struct has no amount field, enforced by a reflection-based regression test.
- **Elicitation bypass** (v0.2+): any way to have a write handler proceed past `elicitConfirmation` when the MCP client has declined or cancelled the confirmation prompt.
- **Request body leakage in write error paths** (v0.2+): any way for a user-submitted transaction memo or other body content to be echoed back to the MCP client through an error surface.
- **Dependency vulnerabilities** that are actually reachable from our code.
- **Supply chain** — typosquatted deps, unpinned transitive deps, compromised release artifacts.

## Out of scope

- Social engineering of a user into pasting their YNAB token into the wrong place.
- Compromise of the user's local machine or MCP client.
- Attacks on `api.ynab.com` itself (report those to YNAB).
- Issues that require physical access to the running process (e.g. reading `/proc/PID/environ` with elevated privileges).
- Denial-of-service via a local prompt-injected LLM session exhausting the user's own rate budget — this is expected behavior and mitigated by the per-token rate limiter.
- Findings from automated scanners without a working proof-of-concept.

## Response SLA

Best effort, because this is a small project:

- **Initial acknowledgement**: within 3 business days.
- **Triage + severity assessment**: within 7 business days.
- **Fix + release**: depends on severity. Critical/High issues are prioritized over feature work.

You will be credited in the release notes unless you request otherwise.

## Security design notes

For background on the threat model this project was designed against, see the "Security model" section in [README.md](README.md). Relevant source files for security-sensitive review:

- `token.go` — redacting token type; only `reveal()` caller is `client.go`.
- `client.go` — host-locked HTTP transport, rate limiter, redirect refusal, token header injection.
- `errors.go` — bearer-token and `Authorization:` header scrubbing of error strings.
- `tools.go` — tool handlers; every handler runs errors through `sanitizedErr` at the boundary.

Any PR touching these files should be reviewed for security implications.
