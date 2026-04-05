// SPDX-License-Identifier: MIT
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/zalando/go-keyring"
	"golang.org/x/time/rate"
)

const (
	// ynabHost is the only hostname this client is willing to talk to.
	// The host-lock RoundTripper enforces this on every outbound request,
	// matching via strings.EqualFold on URL.Hostname() so that URL forms
	// like "API.YNAB.COM" or "api.ynab.com:443" are treated as the same
	// host but any other name is refused.
	ynabHost = "api.ynab.com"

	// ynabBaseURL is a compile-time constant. We never accept a runtime
	// override; the brief explicitly warns against a configurable base URL
	// that could accept http:// or a non-YNAB host.
	ynabBaseURL = "https://api.ynab.com/v1"

	// keyringService is the service name under which the token is stored
	// in the OS keyring (Keychain on macOS, Secret Service on Linux,
	// Credential Manager on Windows).
	keyringService = "mcp-ynab"
	keyringUser    = "default"
)

// YNAB's published rate limit is 200 requests per hour per token with a
// rolling window. We budget 180/hr to leave headroom for clock skew and the
// occasional retry, expressed as 1 request per 20 seconds with a burst of
// 10 so a brief activity spike at session start stays responsive.
//
//	Over an hour (starting with a full bucket): 10 + (3600/20) = 190 calls
//
// well under the 200/hr ceiling.
//
// The limiter is per-Client, which in this binary is per-process since
// main constructs exactly one Client. With one YNAB token per process,
// this is effectively per-token (the behavior the YNAB docs describe).
var (
	defaultRate  = rate.Every(20 * time.Second)
	defaultBurst = 10
)

const (
	// requestTimeout is applied per HTTP call. YNAB responses are typically
	// sub-second; beyond 30s indicates a stuck connection.
	requestTimeout = 30 * time.Second

	// maxResponseBytes caps the response body we will decode. Real YNAB
	// responses for our endpoints are well below this; the limit is a
	// safeguard against an abusive or compromised upstream.
	maxResponseBytes = 8 * 1024 * 1024
)

// maxTokenFileBytes caps how much of YNAB_API_TOKEN_FILE we read. YNAB
// PATs are ~64 bytes; 4 KB is absurdly generous and protects against
// pathological misconfiguration (e.g. YNAB_API_TOKEN_FILE pointed at
// /dev/urandom or a multi-gigabyte log file) without affecting any
// realistic use case. Matches the cap on storeTokenFromStdin.
const maxTokenFileBytes = 4096

// loadToken resolves the YNAB personal access token from, in order:
//
//  1. YNAB_API_TOKEN environment variable (raw value)
//  2. YNAB_API_TOKEN_FILE environment variable (path to a file containing the token)
//  3. OS keyring entry under service="mcp-ynab", user="default" — stored via
//     `mcp-ynab store-token` or equivalent OS-specific tooling.
//
// The returned token is whitespace-trimmed.
func loadToken() (Token, error) {
	if raw := strings.TrimSpace(os.Getenv("YNAB_API_TOKEN")); raw != "" {
		return NewToken(raw), nil
	}
	if path := os.Getenv("YNAB_API_TOKEN_FILE"); path != "" {
		// Bounded read — see maxTokenFileBytes doc. Review finding M5.
		f, err := os.Open(path)
		if err != nil {
			return Token{}, fmt.Errorf("read YNAB_API_TOKEN_FILE: %w", err)
		}
		defer func() { _ = f.Close() }()
		b, err := io.ReadAll(io.LimitReader(f, maxTokenFileBytes))
		if err != nil {
			return Token{}, fmt.Errorf("read YNAB_API_TOKEN_FILE: %w", err)
		}
		raw := strings.TrimSpace(string(b))
		if raw == "" {
			return Token{}, errors.New("YNAB_API_TOKEN_FILE is empty")
		}
		return NewToken(raw), nil
	}
	// Keyring fallback. Errors here are expected in some environments
	// (no Secret Service running, no Keychain access). We wrap the error
	// with a user-facing hint so the message tells the user what to do,
	// not just that it failed.
	raw, err := keyring.Get(keyringService, keyringUser)
	if err == nil {
		raw = strings.TrimSpace(raw)
		if raw != "" {
			return NewToken(raw), nil
		}
		return Token{}, errors.New("keyring entry is empty")
	}
	return Token{}, errors.New(
		"no YNAB token found. Set YNAB_API_TOKEN, or YNAB_API_TOKEN_FILE, " +
			"or run 'mcp-ynab store-token' to store one in the OS keyring",
	)
}

// storeTokenFromStdin reads a token from os.Stdin (one line, trimmed) and
// writes it to the OS keyring. Used by the 'store-token' subcommand. We
// read only from stdin so the token never appears on the command line,
// where it would land in shell history and /proc/PID/cmdline.
func storeTokenFromStdin() error {
	buf, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	token := strings.TrimSpace(string(buf))
	if token == "" {
		return errors.New("empty token on stdin")
	}
	if err := keyring.Set(keyringService, keyringUser, token); err != nil {
		return fmt.Errorf("write to keyring: %w", err)
	}
	return nil
}

// hostLockedTransport wraps an http.RoundTripper and enforces that every
// request targets api.ynab.com. Any other host causes the request to fail
// BEFORE the inner transport sees it, so the Authorization header never
// reaches an attacker-chosen host. This is the single most important
// defense against a spec-injection or redirect-based exfiltration attack.
//
// It also applies a token-bucket rate limit to every request it handles.
type hostLockedTransport struct {
	inner   http.RoundTripper
	limiter *rate.Limiter
}

func (t *hostLockedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Hostname() strips any port; EqualFold makes the check case-insensitive.
	// Both legitimate (api.ynab.com:443) and case-variant (API.YNAB.COM)
	// forms resolve to the same host. Anything else is refused.
	if !strings.EqualFold(req.URL.Hostname(), ynabHost) {
		// Strip the Authorization header defensively so that even if we
		// returned the request object somewhere (e.g. via an error), it
		// would not carry the token.
		req.Header.Del("Authorization")
		return nil, errHostLocked
	}
	if err := t.limiter.Wait(req.Context()); err != nil {
		return nil, fmt.Errorf("rate limit wait: %w", err)
	}
	return t.inner.RoundTrip(req)
}

// Client is the YNAB HTTP client used by all tool handlers. All outbound
// requests go through hostLockedTransport and are rate-limited.
type Client struct {
	httpClient *http.Client
	token      Token
	baseURL    string // overridable for tests

	// Delta-sync caches for unfiltered read endpoints. nil means delta
	// sync disabled for that endpoint — handlers that see nil fall back
	// to full-fetch mode. Production Clients (constructed via NewClient)
	// always populate these; the test-only testClient helper leaves them
	// nil so each test sees fresh state.
	//
	// Scope is intentionally narrow per the v0.2 brief's A4 decision:
	// only the two high-volume list endpoints. list_categories,
	// list_plans, list_months, and get_month do NOT get delta sync in
	// v0.2 because their responses are small and the added complexity
	// is not justified.
	accountsDelta     *deltaCache[wireAccount]
	transactionsDelta *deltaCache[wireTransaction]
}

// NewClient constructs a YNAB client bound to the real YNAB API.
func NewClient(token Token) (*Client, error) {
	if token.IsZero() {
		return nil, errors.New("ynab: token is empty")
	}
	transport := &hostLockedTransport{
		inner:   http.DefaultTransport,
		limiter: rate.NewLimiter(defaultRate, defaultBurst),
	}
	return &Client{
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   requestTimeout,
			// Refuse all redirects. The YNAB v1 API does not redirect; if
			// it ever starts, we fail loud rather than silently forward
			// the Authorization header across hosts.
			//
			// When a redirect is refused, log the target URL to stderr
			// (our standard logger is redirected to stderr in main.go)
			// so an operator diagnosing "why is YNAB returning 3xx" has
			// the location. The URL is safe to log — our code never puts
			// secrets in URLs, so req.URL contains no tokens. The error
			// returned to the MCP client remains status-only via
			// apiError. Review finding L2.
			CheckRedirect: func(req *http.Request, _ []*http.Request) error {
				if req != nil && req.URL != nil {
					log.Printf("ynab: refused redirect to %q (all redirects blocked to prevent Authorization forwarding)", req.URL.String())
				}
				return http.ErrUseLastResponse
			},
		},
		token:             token,
		baseURL:           ynabBaseURL,
		accountsDelta:     newDeltaCache[wireAccount](),
		transactionsDelta: newDeltaCache[wireTransaction](),
	}, nil
}

// doJSON issues a GET against the YNAB API and decodes the response body
// into out. path must be a YNAB API path (starting with "/plans" or "/user"),
// not an absolute URL. Query parameters may be provided via query. All error
// paths return sanitized errors — the Authorization header is never echoed
// and the raw response body is never included verbatim.
//
// doJSON is a thin wrapper around doJSONWithBody for the common GET case.
// Read handlers call this; write handlers call doJSONWithBody directly.
func (c *Client) doJSON(ctx context.Context, path string, query url.Values, out any) error {
	return c.doJSONWithBody(ctx, http.MethodGet, path, query, nil, out)
}

// doJSONWithBody is the low-level HTTP entry point. It is shared by read
// (GET) and write (POST / PATCH / PUT) paths. All outbound requests flow
// through this one function, which means:
//
//   - The host-locked RoundTripper enforces api.ynab.com on every call.
//   - The rate limiter fires on every call.
//   - Authorization header injection happens in exactly one place.
//   - c.token.reveal() is called in exactly one place (below).
//   - Error sanitization is consistent across read and write paths.
//
// body is marshaled to JSON if non-nil; for reads callers pass nil. out is
// decoded from the response body; callers may pass nil if they do not need
// the response (e.g. for write tools that only care about success).
func (c *Client) doJSONWithBody(ctx context.Context, method, path string, query url.Values, body, out any) error {
	if strings.Contains(path, "://") {
		return errors.New("ynab: absolute URL not allowed")
	}
	reqURL := c.baseURL + path
	if len(query) > 0 {
		reqURL += "?" + query.Encode()
	}

	var bodyReader io.Reader
	if body != nil {
		marshaled, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("ynab: marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(marshaled)
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, bodyReader)
	if err != nil {
		return fmt.Errorf("ynab: build request: %w", err)
	}
	// SECURITY: this is the ONLY call to c.token.reveal() in the codebase.
	// Any new caller is a potential leak vector and requires explicit
	// review per token.go's contract.
	req.Header.Set("Authorization", "Bearer "+c.token.reveal())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "mcp-ynab/"+Version)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// http.Client wraps transport errors in *url.Error. Its Error()
		// method formats the method and URL but never the request headers
		// or the request body, so neither the token nor the submitted
		// write payload can leak through this path. Our URLs never
		// contain secrets. As additional defense in depth, the caller
		// runs sanitize() over the final error string before returning
		// to the MCP client.
		return fmt.Errorf("ynab: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return apiError(resp)
	}

	if out == nil {
		// Caller explicitly wants no decode. Still read and discard the
		// body to let the underlying connection be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		return nil
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("ynab: read response: %w", err)
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("ynab: decode response: %w", err)
	}
	return nil
}
