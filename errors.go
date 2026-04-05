// SPDX-License-Identifier: MIT
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
)

// maxErrorBodyBytes caps how much of a YNAB error response we parse. Errors
// are small; the limit stops a misbehaving upstream from ballooning our memory
// or our outbound tool response.
const maxErrorBodyBytes = 16 * 1024

// ynabErrorBody mirrors YNAB's ErrorResponse / ErrorDetail schema.
type ynabErrorBody struct {
	Error struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Detail string `json:"detail"`
	} `json:"error"`
}

// bearerRe matches an Authorization bearer value anywhere in a string, so it
// can be scrubbed before leaving the process. This is defense-in-depth: the
// primary guarantee is that no code path in this package ever formats a raw
// Authorization header or token into an error or log line.
//
// The character class is deliberately broad — base64url + "~" + "/" + "="
// covers every common bearer-token encoding (YNAB PATs themselves are hex,
// but nothing in this regex depends on the YNAB format). Being too strict
// would risk missing a token from a future YNAB encoding change; being
// broad only costs false positives on non-token strings, which is
// harmless (we'd just over-scrub). Review finding L7.
var bearerRe = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-~+/=]+`)

// authHeaderRe matches a full Authorization header line (defense-in-depth).
// Uses [^\n]+ (not \S+) so multi-token values like "Authorization: Bearer sk-abc"
// are fully redacted, not just the first whitespace-separated token.
// Review finding M13.
var authHeaderRe = regexp.MustCompile(`(?i)authorization:[^\n]+`)

// sanitize returns s with any bearer-token or Authorization-header pattern
// replaced. It is applied to every YNAB error detail we forward to MCP clients
// and should be applied to any string we log to stderr that crossed a network
// boundary.
func sanitize(s string) string {
	s = bearerRe.ReplaceAllString(s, "Bearer [REDACTED]")
	s = authHeaderRe.ReplaceAllString(s, "Authorization: [REDACTED]")
	return s
}

// apiError converts a non-2xx YNAB response into a safe error suitable for
// surfacing to an MCP client. It deliberately excludes the request URL,
// request headers, the raw response body, and YNAB's error.detail field.
//
// Why drop Detail: YNAB's detail is free-form text that can echo caller-
// supplied input (e.g. "invalid memo: <your-memo-here> contains disallowed
// chars") on validation failures. Forwarding it would leak user-submitted
// fields — including anything the user considers private — back through
// the error surface. The error.name field (short opaque identifier like
// "not_found", "unauthorized", "validation_error") gives the LLM enough
// to act on without the leak vector. Review finding L3.
//
// Both the status and the Name field are passed through sanitize() in
// case a proxied/compromised upstream embeds a token-shaped value there.
// Review finding H2.
//
// apiError never returns nil; if the body cannot be parsed as a YNAB error
// envelope, it still returns a status-only error.
func apiError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	// Lenient decode — YNAB may add fields to its error envelope in the
	// future, and we do NOT want schema drift to degrade a
	// "validation_error" into an opaque "http 422" (the LLM relies on
	// the name field for self-correction). Strict decoding was tried in
	// an earlier pass and reverted per review finding M2.
	var parsed ynabErrorBody
	parseErr := json.Unmarshal(body, &parsed)
	var nameSuffix string
	if parseErr == nil && parsed.Error.Name != "" {
		nameSuffix = ": " + parsed.Error.Name
	}
	msg := sanitize(fmt.Sprintf("ynab: http %d%s", resp.StatusCode, nameSuffix))
	// Wrap the 404 sentinel so composition tools can branch on it via
	// errors.Is without string matching. All other statuses return a
	// plain error.
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s: %w", msg, errYNABNotFound)
	}
	return errors.New(msg)
}

// errHostLocked is returned by the host-locked RoundTripper when a request is
// attempted against a host other than api.ynab.com. Sentinel so tests can
// match exactly.
var errHostLocked = errors.New("ynab: request blocked: non-YNAB host")

// errYNABNotFound is returned by apiError when YNAB responds with HTTP 404.
// Exposed as a sentinel so composition tools (YnabWeeklyCheckin, etc.)
// can distinguish "no prior month exists yet" (harmless) from any other
// upstream failure. Review finding M5.
var errYNABNotFound = errors.New("ynab: http 404")

// isNotFound reports whether err (or any wrapped error in its chain)
// represents a YNAB 404. Uses errors.Is so the sentinel survives
// sanitizedErr wrapping.
func isNotFound(err error) bool {
	return errors.Is(err, errYNABNotFound)
}
