// SPDX-License-Identifier: MIT
package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestSanitize_StripsBearerToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		in    string
		deny  string // substring that MUST NOT appear in output
		allow string // substring that MUST appear in output
	}{
		{
			name:  "bearer lowercase",
			in:    "request failed with bearer sk-abc123xyzDEF_456",
			deny:  "sk-abc123xyzDEF_456",
			allow: "[REDACTED]",
		},
		{
			name:  "Bearer titlecase",
			in:    "Authorization: Bearer sk-abc123xyzDEF_456",
			deny:  "sk-abc123xyzDEF_456",
			allow: "[REDACTED]",
		},
		{
			name:  "BEARER uppercase",
			in:    "BEARER SECRET_TOKEN_1234",
			deny:  "SECRET_TOKEN_1234",
			allow: "[REDACTED]",
		},
		{
			name:  "authorization header value",
			in:    "bad request: authorization: mySecretScheme abc",
			deny:  "mySecretScheme",
			allow: "Authorization: [REDACTED]",
		},
		{
			name:  "no token",
			in:    "ynab: http 404: not_found: the specified plan was not found",
			deny:  "[REDACTED]",
			allow: "not_found",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := sanitize(tc.in)
			if tc.deny != "" && strings.Contains(got, tc.deny) {
				t.Errorf("sanitize(%q) = %q; must not contain %q", tc.in, got, tc.deny)
			}
			if tc.allow != "" && !strings.Contains(got, tc.allow) {
				t.Errorf("sanitize(%q) = %q; must contain %q", tc.in, got, tc.allow)
			}
		})
	}
}

// TestApiError_SanitizesDetail verifies that if a YNAB error response contains
// a Bearer token or Authorization header fragment in its detail field, the
// resulting error string has it stripped before it can reach an MCP client.
func TestApiError_SanitizesDetail(t *testing.T) {
	t.Parallel()
	body := `{"error":{"id":"401","name":"unauthorized","detail":"invalid Bearer sk-leaked-token-value credentials"}}`
	resp := &http.Response{
		StatusCode: 401,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	err := apiError(resp)
	if err == nil {
		t.Fatal("apiError returned nil for a 401 response")
	}
	msg := err.Error()
	if strings.Contains(msg, "sk-leaked-token-value") {
		t.Errorf("apiError leaked the token: %q", msg)
	}
	if !strings.Contains(msg, "http 401") {
		t.Errorf("apiError should include the status code, got %q", msg)
	}
	if !strings.Contains(msg, "unauthorized") {
		t.Errorf("apiError should include the YNAB error name, got %q", msg)
	}
}

// TestApiError_UnparseableBodyStillSafe verifies that if a YNAB error response
// body is not valid JSON (which should never happen, but might), we still
// return a useful error that does not echo the raw body.
func TestApiError_UnparseableBodyStillSafe(t *testing.T) {
	t.Parallel()
	body := `this is not json, and it contains Bearer sk-still-leaked`
	resp := &http.Response{
		StatusCode: 500,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	err := apiError(resp)
	if err == nil {
		t.Fatal("apiError returned nil")
	}
	msg := err.Error()
	if strings.Contains(msg, "sk-still-leaked") {
		t.Errorf("apiError echoed an unparseable body: %q", msg)
	}
	if !strings.Contains(msg, "http 500") {
		t.Errorf("apiError should include the status code, got %q", msg)
	}
}
