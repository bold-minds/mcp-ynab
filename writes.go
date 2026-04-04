// SPDX-License-Identifier: MIT
//
// writes.go holds the infrastructure for write-path tools. The write handlers
// themselves live in tools_writes.go; this file contains only the shared
// guards that every write MUST pass through:
//
//   1. requireWriteAllowed — re-checks YNAB_ALLOW_WRITES=1 at call time
//      (belt-and-braces; registerTools also gates at startup).
//   2. checkAmountBound — enforces the $10K safety cap with an echo-back
//      override mechanism.
//   3. elicitConfirmation — requests a per-call confirmation from the MCP
//      client via the SDK's elicitation API, with graceful fallback when
//      the client doesn't support it.
//
// Every write handler calls all three before issuing any HTTP request to
// YNAB.

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// envAllowWrites is the environment variable that enables write tools.
	// If unset or any value other than "1", write tools are not registered
	// at startup and each write handler refuses at call time as a second
	// line of defense.
	envAllowWrites = "YNAB_ALLOW_WRITES"

	// amountBoundMilliunits is the hard safety cap on any write tool
	// amount argument (including per-category budgeted assignments and
	// transaction amounts). $10,000 USD = 10,000,000 milliunits. Writes
	// with |amount| > this threshold require an echo-back override via
	// amount_override_milliunits matching the original amount exactly.
	//
	// This is a single universal milliunit threshold, not currency-aware.
	// For USD plans (the vast majority), the cap maps to $10K. For plans
	// in currencies with very different subunit scales, the cap may be
	// more or less restrictive than $10K-equivalent — this is an accepted
	// limitation, documented in docs/ASSUMPTIONS.md.
	amountBoundMilliunits int64 = 10_000_000
)

// writeAllowed reports whether writes are enabled via the environment. Used
// by registerTools to decide whether to register write handlers at startup,
// and again by each handler at call time.
func writeAllowed() bool {
	return os.Getenv(envAllowWrites) == "1"
}

// requireWriteAllowed is called at the top of every write handler as the
// per-call gate. The error message is intentionally actionable: an LLM that
// sees it should know exactly how to enable the feature.
func requireWriteAllowed() error {
	if !writeAllowed() {
		return errors.New(
			"writes disabled: set " + envAllowWrites + "=1 in the mcp-ynab " +
				"process environment to enable write tools (tools are opt-in " +
				"and not registered at startup when the variable is unset)",
		)
	}
	return nil
}

// checkAmountBound enforces the universal $10K-milliunit safety cap on any
// write amount. If |amount| exceeds the cap, the call is rejected unless
// override equals amount exactly — forcing the LLM to echo the value back
// as an explicit "I meant it" acknowledgment.
//
// This is a safety gate, not an accounting control. The override cannot
// be set accidentally: it must be the exact same signed integer as amount,
// passed via a separate argument. A typo, a default value, or a zero-value
// override all fail the equality check and bounce the write.
func checkAmountBound(amount, override int64) error {
	// Guard against math.MinInt64 which cannot be negated without overflow.
	// No legitimate YNAB amount comes anywhere near this, but defensive
	// coding here is cheap.
	if amount == math.MinInt64 {
		return errors.New("amount out of representable range")
	}
	abs := amount
	if abs < 0 {
		abs = -abs
	}
	if abs <= amountBoundMilliunits {
		return nil
	}
	if override == amount {
		return nil
	}
	return fmt.Errorf(
		"amount %d milliunits exceeds the %d milliunit safety threshold; "+
			"to confirm, re-invoke with amount_override_milliunits set to exactly %d",
		amount, amountBoundMilliunits, amount,
	)
}

// elicitConfirmation asks the MCP client to confirm the pending write via
// the SDK's elicitation API. The flow is:
//
//  1. Build a minimal ElicitParams with a human-readable message summarizing
//     the pending write and a single-field boolean schema.
//  2. Call req.Session.Elicit(...).
//  3. If elicit returns an error, the client does not support elicitation.
//     In that case we log to stderr and return nil — the env-var gate is
//     the whole defense for clients that cannot prompt the user. This is
//     the graceful-degradation path the brief specifies.
//  4. If elicit returns action="accept", proceed.
//  5. If elicit returns action="decline" or "cancel", return an error so
//     the handler aborts the write.
//
// message should be specific: include the amount, payee, category name,
// etc. so the user sees what they are confirming.
//
// If session is nil, confirmation is skipped and the function returns nil.
// This path is reachable ONLY in unit tests that call handler methods
// directly without wiring up a full MCP server session (see tools_writes_test.go).
// Production MCP invocations always populate req.Session with a non-nil
// ServerSession from the SDK. The YNAB_ALLOW_WRITES env-var gate remains
// the primary defense in this case, plus the amount-bound check which
// runs before elicitConfirmation is called.
func elicitConfirmation(ctx context.Context, session *mcp.ServerSession, message string) error {
	if session == nil {
		return nil // unit-test path; see doc above
	}

	// Minimal confirmation schema. We do not actually consume the Content
	// field of the result — only Action — but the protocol requires a
	// schema be present.
	schema := &jsonschema.Schema{
		Type:        "object",
		Description: "Confirm or decline the pending write",
		Properties: map[string]*jsonschema.Schema{
			"confirmed": {
				Type:        "boolean",
				Description: "true to proceed with the write, false to cancel",
			},
		},
	}

	result, err := session.Elicit(ctx, &mcp.ElicitParams{
		Message:         message,
		RequestedSchema: schema,
	})
	if err != nil {
		// Client does not support elicitation. Log to stderr and proceed
		// — the env-var gate is the whole defense in this mode. Do not
		// propagate the error upward as a write failure; the brief
		// explicitly specifies graceful degradation.
		logElicitationUnsupported(err)
		return nil
	}

	switch result.Action {
	case "accept":
		return nil
	case "decline":
		return errors.New("write cancelled: user declined confirmation")
	case "cancel":
		return errors.New("write cancelled: user cancelled the elicitation")
	default:
		return fmt.Errorf("unexpected elicitation action: %q", result.Action)
	}
}

// logElicitationUnsupported writes a single-line notice to stderr when a
// client rejects elicitation. Isolated as a helper so the log format can
// be reviewed in one place and rate-limited if necessary in a future
// revision.
func logElicitationUnsupported(err error) {
	// The standard logger is already redirected to stderr in main.go.
	// Sanitize defensively in case the error ever contains request context.
	log.Printf("elicitation unsupported by client; falling back to env-gate only: %v", sanitize(err.Error()))
}
