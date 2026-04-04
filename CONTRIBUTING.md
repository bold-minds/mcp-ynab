# Contributing to mcp-ynab

Thanks for your interest in contributing. This project is deliberately small and security-sensitive — please read this file before opening a PR.

## Ground rules

1. **Read-only stays read-only in v0.1.x.** Write tools are planned for v0.2 behind an explicit opt-in (`YNAB_ALLOW_WRITES=1`) with per-call confirmation via MCP elicitation. PRs that add write tools to v0.1.x will be closed.
2. **No new dependencies without a compelling reason.** Every dep is an attack surface. The current set is intentionally minimal (MCP SDK, `golang.org/x/time/rate`, `go-keyring`, and their transitives). A PR adding a new direct dependency should explain in the description why a stdlib solution will not work.
3. **Security-sensitive files require extra scrutiny.** `token.go`, `client.go`, and `errors.go` touch the credential handling path. Changes to these files should include a test that demonstrates the new behavior does not leak the token.
4. **Never log or format the token directly.** The `Token.reveal()` method is called in exactly one place (setting the `Authorization` header). New callers are a security change that must be justified in the PR description.
5. **Code of Conduct applies.** See [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

## Reporting security issues

**Do not open a public issue for security problems.** See [SECURITY.md](SECURITY.md) for the private reporting channel.

## Development setup

Requirements:

- Go 1.25 or newer (see `go.mod`)
- `git`
- (optional) `docker` — only needed to build/test the container image

Clone and install dev tools:

```bash
git clone https://github.com/bold-minds/mcp-ynab
cd mcp-ynab
go mod download
go install honnef.co/go/tools/cmd/staticcheck@latest
go install golang.org/x/vuln/cmd/govulncheck@latest
```

## Running checks locally

All of the following must pass before a PR merges — CI enforces them.

```bash
go vet ./...                     # static checks
staticcheck ./...                # extra lints
go test -race -cover ./...       # race-detected tests with coverage
govulncheck ./...                # known-vuln scan against current Go + deps
```

The subprocess tests (`subprocess_test.go`) build a real binary and speak JSON-RPC to it. They are skipped under `go test -short`.

## Running the server locally

```bash
echo -n "your-ynab-personal-access-token" | go run . store-token   # one-time
go run .                                                           # stdio server
```

The server reads JSON-RPC messages from stdin and writes responses to stdout. **Never print to stdout from your own code** — it will corrupt the transport framing. Use `log.Printf` (already redirected to stderr in `main.go`) or `os.Stderr` directly.

## Adding a tool

1. Define input and output struct types in `tools.go` with `json` and `jsonschema` tags. Mark required fields without `,omitempty`.
2. Add a handler method on `*Client` with the signature:
   ```go
   func (c *Client) YourTool(ctx context.Context, _ *mcp.CallToolRequest, in YourToolInput) (*mcp.CallToolResult, YourToolOutput, error)
   ```
3. Register it in `registerTools` with `mcp.ToolAnnotations{ReadOnlyHint: true}` (or `false` with explicit justification if the tool mutates YNAB).
4. Add unit tests in `tools_test.go` using the `testClient` helper, covering at least:
   - Happy path with a mocked YNAB response.
   - At least one error path.
   - Filter / limit / required-field edge cases.
5. Add the tool to the `want` map in `TestSubprocess_InitializeAndListTools` in `subprocess_test.go`.
6. Add it to the tool table in `README.md`.
7. Add a `### Added` entry in `CHANGELOG.md` under `[Unreleased]`.

## Commit messages

Format: `<scope>: <subject>` where scope is one of `tools`, `client`, `token`, `money`, `errors`, `ci`, `docs`, `deps`, `docker`.

Examples:

```
tools: add list_payees with soft-delete filter
client: tighten host-lock on port-qualified URLs
errors: strip Basic auth headers in addition to Bearer
docs: correct claude desktop config example on macOS
```

Keep subject lines under 72 characters. Use the body for the why.

## Pull request checklist

The PR template will walk you through this, but the short form:

- [ ] `go vet ./...` passes
- [ ] `staticcheck ./...` passes
- [ ] `go test -race ./...` passes
- [ ] `govulncheck ./...` passes
- [ ] `CHANGELOG.md` updated under `[Unreleased]`
- [ ] If touching `token.go`, `client.go`, or `errors.go`: a test demonstrates no token leakage on the new code path
- [ ] If adding a dependency: justification in the PR description
- [ ] If adding a tool: README tool table and subprocess test updated

## Release process (maintainers)

1. Update `CHANGELOG.md`: move `[Unreleased]` content into a new `[X.Y.Z]` section, add a new empty `[Unreleased]` above it, update the compare link.
2. Tag: `git tag -s vX.Y.Z -m "vX.Y.Z"` (signed).
3. Push: `git push origin vX.Y.Z`.
4. GoReleaser runs via `.github/workflows/release.yml` and publishes:
   - GitHub Release with cross-platform archives and checksums
   - Multi-arch container images to `ghcr.io/bold-minds/mcp-ynab`

## License

By contributing you agree that your contributions will be licensed under the [MIT License](LICENSE).
