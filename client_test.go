// SPDX-License-Identifier: MIT
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// testClient builds a Client pointed at an httptest server. It deliberately
// bypasses hostLockedTransport so we can exercise the rest of the client
// against an in-process fake. The hostLockedTransport itself has dedicated
// tests below.
func testClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Client{
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		token:   NewToken("sk-test-TOKEN-1234567890"),
		baseURL: srv.URL,
	}, srv
}

// ---- host lock --------------------------------------------------------------

func TestHostLockedTransport_RefusesNonYNABHost(t *testing.T) {
	t.Parallel()
	transport := &hostLockedTransport{
		inner:   http.DefaultTransport,
		limiter: rate.NewLimiter(rate.Inf, 1),
	}
	req, err := http.NewRequest("GET", "https://attacker.example/v1/plans", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-should-not-be-sent")

	resp, err := transport.RoundTrip(req)
	if resp != nil {
		t.Errorf("expected nil response, got %v", resp)
	}
	if !errors.Is(err, errHostLocked) {
		t.Errorf("expected errHostLocked, got %v", err)
	}
	// After RoundTrip returns errHostLocked, the Authorization header must
	// have been stripped from the request object as defense-in-depth.
	if h := req.Header.Get("Authorization"); h != "" {
		t.Errorf("Authorization header was not stripped, got %q", h)
	}
}

// TestHostLockedTransport_AllowsCaseInsensitiveAndPort verifies that the
// hardened host check accepts api.ynab.com, API.YNAB.COM, and
// api.ynab.com:443 — all of which are the same host — while still rejecting
// anything else.
func TestHostLockedTransport_AllowsCaseInsensitiveAndPort(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		url  string
		ok   bool
	}{
		{"lowercase", "https://api.ynab.com/v1/plans", true},
		{"uppercase", "https://API.YNAB.COM/v1/plans", true},
		{"mixed case", "https://Api.Ynab.Com/v1/plans", true},
		{"explicit port 443", "https://api.ynab.com:443/v1/plans", true},
		{"attacker similar prefix", "https://api.ynab.com.evil.io/v1/plans", false},
		{"attacker similar suffix", "https://evil.api.ynab.com.evil/v1/plans", false},
		{"attacker path trick", "https://evil.example/api.ynab.com/plans", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inner := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: http.NoBody, Header: make(http.Header)}, nil
			})
			transport := &hostLockedTransport{
				inner:   inner,
				limiter: rate.NewLimiter(rate.Inf, 1),
			}
			req, _ := http.NewRequest("GET", tc.url, nil)
			req.Header.Set("Authorization", "Bearer sk-test")
			_, err := transport.RoundTrip(req)
			if tc.ok && err != nil {
				t.Errorf("expected allowed, got %v", err)
			}
			if !tc.ok && !errors.Is(err, errHostLocked) {
				t.Errorf("expected errHostLocked, got %v", err)
			}
		})
	}
}

func TestHostLockedTransport_RateLimits(t *testing.T) {
	t.Parallel()
	called := 0
	inner := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		called++
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: make(http.Header)}, nil
	})
	transport := &hostLockedTransport{
		inner:   inner,
		limiter: rate.NewLimiter(rate.Every(time.Hour), 1), // burst 1, no refill in test window
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	req1, _ := http.NewRequestWithContext(ctx, "GET", "https://api.ynab.com/v1/plans", nil)
	if _, err := transport.RoundTrip(req1); err != nil {
		t.Fatalf("first request should succeed: %v", err)
	}
	req2, _ := http.NewRequestWithContext(ctx, "GET", "https://api.ynab.com/v1/plans", nil)
	if _, err := transport.RoundTrip(req2); err == nil {
		t.Errorf("second request should be rate-limited; got nil error")
	}
	if called != 1 {
		t.Errorf("inner transport called %d times; expected 1", called)
	}
}

// TestRateLimit_HourlyBudget is a sanity check that the configured rate
// stays under YNAB's 200 req/hr ceiling. It does NOT run the limiter for an
// hour; it just asserts the math.
func TestRateLimit_HourlyBudget(t *testing.T) {
	t.Parallel()
	// 1 req per 20 sec + burst 10 = 10 + (3600/20) = 190 calls/hr max.
	perHour := defaultBurst + int(float64(time.Hour)/float64(20*time.Second))
	if perHour >= 200 {
		t.Errorf("rate limit config allows %d/hr, exceeds YNAB 200/hr ceiling", perHour)
	}
	if perHour < 100 {
		t.Errorf("rate limit config allows only %d/hr, too restrictive", perHour)
	}
}

// ---- doJSON error sanitization — the critical security regression -----------

// TestDoJSON_401DoesNotLeakBearerToken is the most important test in this
// package. It verifies that when YNAB returns a 401, the final error string
// that would reach an MCP client contains neither the literal "Bearer "
// followed by token characters nor any substring of the token.
func TestDoJSON_401DoesNotLeakBearerToken(t *testing.T) {
	t.Parallel()
	const secretToken = "sk-extremely-secret-token-value-do-not-leak"
	client, _ := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		// Pathological: YNAB echoes "Bearer <token>" in its error detail.
		// Our sanitize() must strip this before it reaches the caller.
		_, _ = w.Write([]byte(`{"error":{"id":"401","name":"unauthorized","detail":"invalid token: Bearer ` + secretToken + ` is not recognized"}}`))
	})
	client.token = NewToken(secretToken)

	var out struct{}
	err := client.doJSON(context.Background(), "/plans", nil, &out)
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	msg := err.Error()
	if strings.Contains(msg, secretToken) {
		t.Errorf("SECURITY REGRESSION: token leaked into error string: %q", msg)
	}
	if !strings.Contains(msg, "http 401") || !strings.Contains(msg, "unauthorized") {
		t.Errorf("error should still contain status and error name, got %q", msg)
	}
}

// TestLogLeak_PathologicalRoundTripper is the adversarial test specified in
// the security brief. A misbehaving HTTP transport returns a Go error whose
// .Error() string embeds the bearer token literally (simulating axios/reqwest
// libraries that echo request config into error messages). Every tool's
// error path must still produce an error string that does NOT contain the
// token.
func TestLogLeak_PathologicalRoundTripper(t *testing.T) {
	t.Parallel()
	const secret = "sk-ultra-secret-12345-ABCDEF-leak-sentinel"

	// An evil RoundTripper that puts the token into every error it returns.
	evilRT := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("pretend-connection-error to %s [auth=Bearer %s]", req.URL.Host, secret)
	})
	client := &Client{
		httpClient: &http.Client{Transport: evilRT, Timeout: 5 * time.Second},
		token:      NewToken(secret),
		baseURL:    "https://api.ynab.com/v1",
	}

	ctx := context.Background()
	calls := []struct {
		name string
		fn   func() error
	}{
		{"ListPlans", func() error {
			_, _, err := client.ListPlans(ctx, nil, ListPlansInput{})
			return err
		}},
		{"GetMonth", func() error {
			_, _, err := client.GetMonth(ctx, nil, GetMonthInput{PlanID: "p"})
			return err
		}},
		{"ListAccounts", func() error {
			_, _, err := client.ListAccounts(ctx, nil, ListAccountsInput{PlanID: "p"})
			return err
		}},
		{"ListTransactions", func() error {
			_, _, err := client.ListTransactions(ctx, nil, ListTransactionsInput{PlanID: "p"})
			return err
		}},
		{"ListCategories", func() error {
			_, _, err := client.ListCategories(ctx, nil, ListCategoriesInput{PlanID: "p"})
			return err
		}},
	}
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			err := c.fn()
			if err == nil {
				t.Fatal("expected error from pathological transport")
			}
			msg := err.Error()
			if strings.Contains(msg, secret) {
				t.Errorf("SECURITY REGRESSION: token leaked via %s: %q", c.name, msg)
			}
			if !strings.Contains(msg, "[REDACTED]") && strings.Contains(msg, "Bearer") {
				t.Errorf("%s: 'Bearer ' appears in error without [REDACTED] next to it: %q", c.name, msg)
			}
		})
	}
}

func TestDoJSON_RefusesAbsoluteURL(t *testing.T) {
	t.Parallel()
	client, _ := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	})
	var out struct{}
	err := client.doJSON(context.Background(), "https://evil.example/plans", nil, &out)
	if err == nil {
		t.Fatal("expected error for absolute URL path")
	}
	if !strings.Contains(err.Error(), "absolute URL not allowed") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestDoJSON_RefusesRedirect(t *testing.T) {
	t.Parallel()
	client, _ := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://attacker.example/")
		w.WriteHeader(302)
	})
	var out struct{}
	err := client.doJSON(context.Background(), "/plans", nil, &out)
	if err == nil {
		t.Fatal("expected error for redirect response")
	}
	if !strings.Contains(err.Error(), "http 302") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestDoJSON_SendsBearerToken(t *testing.T) {
	t.Parallel()
	var seen string
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"plans":[]}}`))
	})
	var out wirePlanSummaryResponse
	if err := client.doJSON(context.Background(), "/plans", nil, &out); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if seen != "Bearer sk-test-TOKEN-1234567890" {
		t.Errorf("Authorization header wrong, got %q", seen)
	}
}

func TestDoJSON_ForwardsQueryParams(t *testing.T) {
	t.Parallel()
	var seenQuery string
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		seenQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"transactions":[]}}`))
	})
	q := url.Values{}
	q.Set("since_date", "2026-01-01")
	q.Set("type", "uncategorized")
	var out wireTransactionsResponse
	if err := client.doJSON(context.Background(), "/plans/x/transactions", q, &out); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(seenQuery, "since_date=2026-01-01") || !strings.Contains(seenQuery, "type=uncategorized") {
		t.Errorf("query params not forwarded, got %q", seenQuery)
	}
}

// ---- token loading ----------------------------------------------------------

func TestLoadToken_FromEnv(t *testing.T) {
	t.Setenv("YNAB_API_TOKEN", "  sk-env-token  ")
	t.Setenv("YNAB_API_TOKEN_FILE", "")
	tok, err := loadToken()
	if err != nil {
		t.Fatal(err)
	}
	// Cannot compare to string directly because Token redacts. Use reveal()
	// which is package-private.
	if tok.reveal() != "sk-env-token" {
		t.Errorf("expected trimmed token, got reveal=%q", tok.reveal())
	}
}

func TestLoadToken_FromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("sk-file-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Env var set to empty so precedence falls to file.
	t.Setenv("YNAB_API_TOKEN", "")
	t.Setenv("YNAB_API_TOKEN_FILE", path)
	tok, err := loadToken()
	if err != nil {
		t.Fatal(err)
	}
	if tok.reveal() != "sk-file-token" {
		t.Errorf("expected file token, got reveal=%q", tok.reveal())
	}
}

func TestLoadToken_EnvWinsOverFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	_ = os.WriteFile(path, []byte("file-value"), 0600)
	t.Setenv("YNAB_API_TOKEN", "env-value")
	t.Setenv("YNAB_API_TOKEN_FILE", path)
	tok, err := loadToken()
	if err != nil {
		t.Fatal(err)
	}
	if tok.reveal() != "env-value" {
		t.Errorf("env should win, got reveal=%q", tok.reveal())
	}
}

// TestLoadToken_MissingAll does NOT depend on the keyring behavior (which is
// platform-dependent in CI). It only checks that when neither env var is set
// and the keyring fallback either returns no entry or errors, loadToken
// returns a helpful error mentioning all three sources.
func TestLoadToken_MissingAll(t *testing.T) {
	t.Setenv("YNAB_API_TOKEN", "")
	t.Setenv("YNAB_API_TOKEN_FILE", "")
	// If keyring is available on this host and already has an entry, this
	// test would spuriously pass. We accept that risk — the important
	// regression is that when the fallback error IS hit, its message is
	// actionable.
	_, err := loadToken()
	if err == nil {
		// Keyring must have had an entry; skip the assertion check. Cover
		// the message assertion via a dedicated hermetic test if ever added.
		t.Skip("keyring returned a token; skipping error-message check")
	}
	msg := err.Error()
	for _, want := range []string{"YNAB_API_TOKEN", "YNAB_API_TOKEN_FILE", "keyring", "store-token"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q: %q", want, msg)
		}
	}
}

// ---- formatMilliunits -------------------------------------------------------

func TestFormatMilliunits(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0.000"},
		{1000, "1.000"},
		{12340, "12.340"},
		{-12340, "-12.340"},
		{1, "0.001"},
		{-1, "-0.001"},
		{999, "0.999"},
		{1001, "1.001"},
		{-9223372036854775808, "-9223372036854775.808"}, // math.MinInt64
	}
	for _, c := range cases {
		got := formatMilliunits(c.in)
		if got != c.want {
			t.Errorf("formatMilliunits(%d) = %q; want %q", c.in, got, c.want)
		}
	}
}

// ---- helpers ----------------------------------------------------------------

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
