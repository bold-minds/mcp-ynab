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
var bearerRe = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-~+/=]+`)

// authHeaderRe matches a full Authorization header line (defense-in-depth).
var authHeaderRe = regexp.MustCompile(`(?i)authorization:\s*\S+`)

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
// request headers, and the raw response body. It returns the HTTP status,
// the YNAB error.name (short opaque identifier like "not_found" or
// "unauthorized"), and a sanitized version of error.detail.
//
// apiError never returns nil; if the body cannot be parsed as a YNAB error
// envelope, it still returns a status-only error.
func apiError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	var parsed ynabErrorBody
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error.Name != "" {
		return fmt.Errorf(
			"ynab: http %d: %s: %s",
			resp.StatusCode,
			parsed.Error.Name,
			sanitize(parsed.Error.Detail),
		)
	}
	return fmt.Errorf("ynab: http %d", resp.StatusCode)
}

// errHostLocked is returned by the host-locked RoundTripper when a request is
// attempted against a host other than api.ynab.com. Sentinel so tests can
// match exactly.
var errHostLocked = errors.New("ynab: request blocked: non-YNAB host")
