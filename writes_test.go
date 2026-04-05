// SPDX-License-Identifier: MIT
package main

import (
	"context"
	"math"
	"net/http"
	"strings"
	"testing"
)

// ---- writeAllowed / requireWriteAllowed ------------------------------------

func TestWriteAllowed_OnlyExactlyOneEnables(t *testing.T) {
	// Cannot use t.Parallel() here — subtests use t.Setenv, which is
	// incompatible with parallel ancestors because env vars are
	// process-global state.
	//
	// Slice (not map) so failure output iteration order is stable across
	// runs. Review nit.
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"0", false},
		{"1", true},
		{"2", false},
		{"true", false}, // deliberately strict — only "1" enables
		{"TRUE", false},
		{"yes", false},
		{"  1  ", false}, // no trimming; whitespace-surrounded is not "1"
	}
	for _, c := range cases {
		// Subtests use t.Setenv which resets at the end of the subtest.
		t.Run("val="+c.in, func(t *testing.T) {
			t.Setenv(envAllowWrites, c.in)
			if got := writeAllowed(); got != c.want {
				t.Errorf("writeAllowed() with %q = %v; want %v", c.in, got, c.want)
			}
		})
	}
}

func TestRequireWriteAllowed_OkWhenEnabled(t *testing.T) {
	t.Setenv(envAllowWrites, "1")
	if err := requireWriteAllowed(); err != nil {
		t.Errorf("expected nil when YNAB_ALLOW_WRITES=1, got %v", err)
	}
}

func TestRequireWriteAllowed_ErrorMentionsHowToFix(t *testing.T) {
	t.Setenv(envAllowWrites, "")
	err := requireWriteAllowed()
	if err == nil {
		t.Fatal("expected error when YNAB_ALLOW_WRITES is unset")
	}
	msg := err.Error()
	// Every word in the actionable error string the LLM needs to resolve it.
	for _, want := range []string{"YNAB_ALLOW_WRITES", "1", "write"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %q, got %q", want, msg)
		}
	}
}

// ---- checkAmountBound -------------------------------------------------------

func TestCheckAmountBound_UnderThreshold(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		amount   int64
		override int64
	}{
		{"zero", 0, 0},
		{"small positive", 5000, 0},
		{"small negative", -5000, 0},
		{"at threshold", amountBoundMilliunits, 0},
		{"just under threshold", amountBoundMilliunits - 1, 0},
		{"negative at threshold", -amountBoundMilliunits, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if err := checkAmountBound(c.amount, c.override); err != nil {
				t.Errorf("amount=%d override=%d: expected nil, got %v", c.amount, c.override, err)
			}
		})
	}
}

func TestCheckAmountBound_OverThresholdRejectedWithoutOverride(t *testing.T) {
	t.Parallel()
	over := amountBoundMilliunits + 1
	cases := []struct {
		name   string
		amount int64
	}{
		{"just over positive", over},
		{"just over negative", -over},
		{"large positive", 100_000_000},
		{"large negative", -100_000_000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := checkAmountBound(c.amount, 0) // zero-value override
			if err == nil {
				t.Errorf("amount=%d: expected rejection, got nil", c.amount)
			}
			if err != nil && !strings.Contains(err.Error(), "amount_override_milliunits") {
				t.Errorf("error should mention override field, got %v", err)
			}
		})
	}
}

func TestCheckAmountBound_OverThresholdAcceptedWithMatchingOverride(t *testing.T) {
	t.Parallel()
	cases := []int64{
		amountBoundMilliunits + 1,
		-(amountBoundMilliunits + 1),
		50_000_000,
		-50_000_000,
	}
	for _, amount := range cases {
		if err := checkAmountBound(amount, amount); err != nil {
			t.Errorf("amount=%d with matching override: expected nil, got %v", amount, err)
		}
	}
}

func TestCheckAmountBound_OverThresholdRejectedWithMismatchedOverride(t *testing.T) {
	t.Parallel()
	amount := int64(50_000_000)
	mismatches := []int64{
		0,             // default
		-50_000_000,   // sign flip — same magnitude, still rejected
		50_000_001,    // off by one
		amount - 1,    // close but wrong
		amount + 1,    // close but wrong
		amount / 2,    // half — wrong
		-amount,       // negation
	}
	for _, override := range mismatches {
		err := checkAmountBound(amount, override)
		if err == nil {
			t.Errorf("amount=%d override=%d: should be rejected (override doesn't match)", amount, override)
		}
	}
}

func TestCheckAmountBound_MinInt64Rejected(t *testing.T) {
	t.Parallel()
	// math.MinInt64 cannot be safely negated; we reject it at the boundary.
	err := checkAmountBound(math.MinInt64, math.MinInt64)
	if err == nil {
		t.Fatal("expected error for math.MinInt64 amount")
	}
	if !strings.Contains(err.Error(), "out of representable range") {
		t.Errorf("wrong error for MinInt64: %v", err)
	}
}

// ---- doJSONWithBody --------------------------------------------------------

func TestDoJSONWithBody_POSTSendsBodyAndContentType(t *testing.T) {
	t.Parallel()
	type req struct {
		Name   string `json:"name"`
		Amount int64  `json:"amount"`
	}
	var seenBody, seenContentType, seenMethod string
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		seenMethod = r.Method
		seenContentType = r.Header.Get("Content-Type")
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		seenBody = string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"ok":true}}`))
	})
	var out struct {
		Data struct{ OK bool } `json:"data"`
	}
	body := req{Name: "Groceries", Amount: 12340}
	if err := client.doJSONWithBody(context.Background(), "POST", "/plans/p/transactions", nil, body, &out); err != nil {
		t.Fatal(err)
	}
	if seenMethod != "POST" {
		t.Errorf("wrong method, got %q", seenMethod)
	}
	if seenContentType != "application/json" {
		t.Errorf("wrong content type, got %q", seenContentType)
	}
	if !strings.Contains(seenBody, `"name":"Groceries"`) || !strings.Contains(seenBody, `"amount":12340`) {
		t.Errorf("body not serialized correctly: %q", seenBody)
	}
}

func TestDoJSONWithBody_PATCHWorksSameAsPOST(t *testing.T) {
	t.Parallel()
	var seenMethod string
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		seenMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{}}`))
	})
	var out struct{}
	err := client.doJSONWithBody(context.Background(), "PATCH", "/plans/p/transactions/t", nil, map[string]string{"memo": "x"}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if seenMethod != "PATCH" {
		t.Errorf("expected PATCH, got %q", seenMethod)
	}
}

// TestDoJSONWithBody_ErrorDoesNotLeakSubmittedBody verifies the critical
// security property from the brief: if the server returns an error response
// that echoes the submitted body (e.g. a 400 with the offending memo in the
// detail field), our returned error does NOT contain the original payload
// or any user-submitted memo string.
func TestDoJSONWithBody_ErrorDoesNotLeakSubmittedBody(t *testing.T) {
	t.Parallel()
	const secretMemo = "PERSONAL-NOTE-do-not-echo-12345"
	client, _ := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		// Pathological: YNAB echoes the submitted memo back in the error
		// detail. Our error surface should scrub it or truncate detail
		// before the string reaches the MCP client.
		_, _ = w.Write([]byte(`{"error":{"id":"400","name":"validation_error","detail":"invalid memo: ` + secretMemo + ` contains disallowed chars"}}`))
	})
	type req struct {
		Memo string `json:"memo"`
	}
	var out struct{}
	err := client.doJSONWithBody(context.Background(), "POST", "/plans/p/transactions", nil,
		req{Memo: secretMemo}, &out)
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
	msg := err.Error()
	// The error must include the HTTP status and YNAB's error.name (an
	// opaque identifier the LLM can act on) so callers get actionable
	// feedback.
	if !strings.Contains(msg, "http 400") || !strings.Contains(msg, "validation_error") {
		t.Errorf("error should include status and name, got %q", msg)
	}
	// Critical invariant (review finding L3): the submitted memo MUST NOT
	// appear in the error surface. apiError drops YNAB's error.detail
	// field entirely because it can echo caller-supplied input.
	if strings.Contains(msg, secretMemo) {
		t.Errorf("REDACTION FAILURE: submitted memo leaked into error output: %q", msg)
	}
}
