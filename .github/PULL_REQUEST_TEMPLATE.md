<!--
Thanks for contributing! Please fill in the sections below. Delete any that do not apply.

For security issues, do NOT use a public PR — report via https://github.com/bold-minds/mcp-ynab/security/advisories/new instead.
-->

## Summary

<!-- What does this PR change, and why? One or two sentences. -->

## Type of change

<!-- Check all that apply. -->

- [ ] Bug fix (non-breaking)
- [ ] New feature (non-breaking)
- [ ] Breaking change
- [ ] Documentation / comments only
- [ ] CI / build / infra
- [ ] Dependency update
- [ ] Test-only change
- [ ] Security fix

## Related issue

<!-- Closes #123, or "no related issue". -->

## Changes

<!-- Bullet list of what changed. Keep it skimmable. -->

-
-

## Testing

<!-- How did you test this? What cases did you cover? -->

- [ ] `go vet ./...` passes
- [ ] `staticcheck ./...` passes (clean)
- [ ] `go test -race ./...` passes
- [ ] `govulncheck ./...` passes
- [ ] New code has unit tests
- [ ] If a new tool was added: subprocess test (`subprocess_test.go`) asserts the tool exists and is read-only
- [ ] If a new tool was added: README tool table and `CHANGELOG.md` updated

## Security checklist

**Fill out this section if your PR touches `token.go`, `client.go`, `errors.go`, `main.go`, or any file involved in the credential or HTTP path.**

- [ ] No new calls to `Token.reveal()` (or justified in the PR description if there is one)
- [ ] No new `fmt.Errorf` / `log.Printf` / `return err` sites that could contain a bearer token, Authorization header, or YNAB response body verbatim
- [ ] No new direct dependencies (or dependency addition justified in the PR description)
- [ ] No `os.Stdout` writes or `fmt.Print*` to default stdout (which would corrupt MCP stdio framing)
- [ ] Host-lock is still effective for any new request path (hostname check still fires, no bypass via URL construction tricks)
- [ ] Rate limiter still wraps any new outbound HTTP path
- [ ] A test exists that demonstrates the new code path does not leak credentials under adversarial conditions

## Breaking changes

<!-- If this PR breaks anything, describe the break and migration. -->

## Changelog entry

<!-- Copy what you put under `[Unreleased]` in CHANGELOG.md. -->

```
### Added / Changed / Fixed / Removed / Security

-
```
