// SPDX-License-Identifier: MIT
//
// Write-path tool handlers. Every handler in this file follows the same
// shape:
//
//  1. requireWriteAllowed — per-call gate on YNAB_ALLOW_WRITES=1. Belt-
//     and-braces; registerTools also gates at startup so handlers never
//     run when the env var is unset.
//  2. Required-field checks.
//  3. checkAmountBound — $10K milliunit safety cap with echo-back override.
//  4. elicitConfirmation — asks the MCP client for per-call confirmation.
//     Gracefully degrades to "env-gate-only" when the client does not
//     support elicitation.
//  5. (Where applicable) fetch current state for the "before" snapshot.
//  6. Issue the YNAB POST/PATCH/PUT.
//  7. Return the entity + before/after snapshot. All error paths run
//     through sanitizedErr at the tool boundary.
//
// The skill layer is responsible for persisting an audit trail from the
// response; the MCP server itself writes no logs to disk.

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ============================================================================
// Inputs and outputs.
// ============================================================================

// CreateTransactionInput has many optional fields because YNAB's NewTransaction
// schema does. Pointer/omitempty semantics:
//
//   - amount_milliunits is REQUIRED (signed; negative = outflow, positive = inflow)
//   - one of payee_id or payee_name is required
//   - category_id is optional (transaction lands as "uncategorized" if omitted)
//   - date defaults to today in UTC when empty
//   - cleared defaults to "uncleared"
//   - approved defaults to true (use a pointer to distinguish "not set" from "false")
//   - import_id is optional; YNAB dedupes server-side on it
//   - amount_override_milliunits gates the $10K cap (see checkAmountBound)
type CreateTransactionInput struct {
	PlanID    string `json:"plan_id" jsonschema:"YNAB plan id (UUID), or 'last-used' / 'default'"`
	AccountID string `json:"account_id" jsonschema:"account to record the transaction against"`
	AmountMilliunits int64 `json:"amount_milliunits" jsonschema:"signed amount in milliunits; negative for outflow, positive for inflow"`

	PayeeName  string `json:"payee_name,omitempty" jsonschema:"payee name; one of payee_name or payee_id is required. New payees are created automatically if the name does not match an existing payee."`
	PayeeID    string `json:"payee_id,omitempty" jsonschema:"payee id (UUID); one of payee_name or payee_id is required"`
	CategoryID string `json:"category_id,omitempty" jsonschema:"category id (UUID); transaction is uncategorized if omitted"`
	Memo       string `json:"memo,omitempty" jsonschema:"freeform memo, max 200 characters"`
	Date       string `json:"date,omitempty" jsonschema:"ISO date (YYYY-MM-DD); defaults to today (UTC) if omitted"`
	Cleared    string `json:"cleared,omitempty" jsonschema:"cleared|uncleared|reconciled; defaults to 'uncleared'"`
	Approved   *bool  `json:"approved,omitempty" jsonschema:"defaults to true; pass false to leave the transaction unapproved"`
	ImportID   string `json:"import_id,omitempty" jsonschema:"optional idempotency key; YNAB dedupes on this. Max 36 characters."`

	AmountOverrideMilliunits int64 `json:"amount_override_milliunits,omitempty" jsonschema:"if |amount_milliunits| exceeds the $10K safety threshold, set this to the same value as amount_milliunits to acknowledge the large transaction"`
}

type CreateTransactionOutput struct {
	Transaction Transaction `json:"transaction"`
	// Before is always null for create_transaction — there is no prior state.
	Before *struct{} `json:"before"`
	// After is the account balance snapshot after the transaction posted.
	// May be nil if the post-create account fetch failed (the transaction
	// was still created; check Transaction.ID).
	After *Money `json:"after,omitempty"`
}

// UpdateCategoryBudgetedInput is the primitive for Rule 3 money moves during
// the Sunday ritual. Only the `budgeted` (assigned) amount is mutable via
// YNAB's API for month-categories.
type UpdateCategoryBudgetedInput struct {
	PlanID     string `json:"plan_id" jsonschema:"YNAB plan id (UUID), or 'last-used' / 'default'"`
	Month      string `json:"month" jsonschema:"ISO month (YYYY-MM-01) or 'current' for the current calendar month (UTC)"`
	CategoryID string `json:"category_id" jsonschema:"category id (UUID)"`
	NewBudgetedMilliunits int64 `json:"new_budgeted_milliunits" jsonschema:"the new assigned amount for the category in milliunits; replaces the current value"`

	AmountOverrideMilliunits int64 `json:"amount_override_milliunits,omitempty" jsonschema:"if |new_budgeted_milliunits| exceeds the $10K safety threshold, set this to the same value to acknowledge"`
}

type UpdateCategoryBudgetedOutput struct {
	Category Category          `json:"category"`
	Before   CategorySnapshot  `json:"before"`
	After    CategorySnapshot  `json:"after"`
}

// CategorySnapshot is a lean before/after view of the two fields that matter
// for Rule 3: assigned amount and available balance.
type CategorySnapshot struct {
	Budgeted Money `json:"budgeted"`
	Balance  Money `json:"balance"`
}

// ============================================================================
// Wire types for POST and PATCH bodies and responses.
// ============================================================================

type wireNewTransactionWrapper struct {
	Transaction wireNewTransaction `json:"transaction"`
}

type wireNewTransaction struct {
	AccountID  string  `json:"account_id"`
	Date       string  `json:"date"`
	Amount     int64   `json:"amount"`
	PayeeID    *string `json:"payee_id,omitempty"`
	PayeeName  *string `json:"payee_name,omitempty"`
	CategoryID *string `json:"category_id,omitempty"`
	Memo       *string `json:"memo,omitempty"`
	Cleared    string  `json:"cleared,omitempty"`
	Approved   bool    `json:"approved"`
	ImportID   *string `json:"import_id,omitempty"`
}

// wireSaveTransactionsResponse matches YNAB's SaveTransactionsResponse
// schema for the single-transaction variant. The bulk variant populates
// .data.transactions[] and .data.duplicate_import_ids[], which we do not
// use — single-transaction POST always populates .data.transaction.
type wireSaveTransactionsResponse struct {
	Data struct {
		Transaction    wireTransaction `json:"transaction"`
		TransactionIDs []string        `json:"transaction_ids"`
	} `json:"data"`
}

type wireSingleAccountResponse struct {
	Data struct {
		Account wireAccount `json:"account"`
	} `json:"data"`
}

type wirePatchMonthCategoryWrapper struct {
	Category wireSaveMonthCategory `json:"category"`
}

type wireSaveMonthCategory struct {
	Budgeted int64 `json:"budgeted"`
}

type wireCategoryResponse struct {
	Data struct {
		Category wireCategory `json:"category"`
	} `json:"data"`
}

// ============================================================================
// Handlers.
// ============================================================================

// CreateTransaction posts a new transaction to YNAB. See the CreateTransactionInput
// struct doc for argument semantics. Every call:
//
//   - Rejects if YNAB_ALLOW_WRITES is not "1" (per-call gate).
//   - Enforces the $10K milliunit safety cap on amount_milliunits (echo-back override).
//   - Asks the MCP client to confirm via elicitation (with graceful degradation).
//   - Posts to /plans/{plan_id}/transactions.
//   - Fetches the updated account balance for the "after" snapshot.
//
// The transaction is created even if the post-create balance fetch fails;
// the response's Transaction.ID indicates success regardless.
func (c *Client) CreateTransaction(ctx context.Context, req *mcp.CallToolRequest, in CreateTransactionInput) (*mcp.CallToolResult, CreateTransactionOutput, error) {
	if err := requireWriteAllowed(); err != nil {
		return nil, CreateTransactionOutput{}, sanitizedErr(err)
	}
	if in.PlanID == "" {
		return nil, CreateTransactionOutput{}, errors.New("plan_id is required")
	}
	if in.AccountID == "" {
		return nil, CreateTransactionOutput{}, errors.New("account_id is required")
	}
	if in.PayeeName == "" && in.PayeeID == "" {
		return nil, CreateTransactionOutput{}, errors.New("one of payee_name or payee_id is required")
	}
	if in.Memo != "" && len(in.Memo) > 200 {
		return nil, CreateTransactionOutput{}, errors.New("memo must be at most 200 characters")
	}
	if in.ImportID != "" && len(in.ImportID) > 36 {
		return nil, CreateTransactionOutput{}, errors.New("import_id must be at most 36 characters")
	}
	if err := checkAmountBound(in.AmountMilliunits, in.AmountOverrideMilliunits); err != nil {
		return nil, CreateTransactionOutput{}, sanitizedErr(err)
	}

	// Elicit confirmation with a specific summary. The exact message is
	// what the user sees on their MCP client.
	msg := fmt.Sprintf(
		"Create YNAB transaction: %s on %s to %q (account %s)",
		formatSignedMoney(in.AmountMilliunits),
		orDefault(in.Date, time.Now().UTC().Format("2006-01-02")),
		orDefault(in.PayeeName, in.PayeeID),
		in.AccountID,
	)
	if err := elicitConfirmation(ctx, req.Session, msg); err != nil {
		return nil, CreateTransactionOutput{}, sanitizedErr(err)
	}

	// Build the YNAB body. YNAB's NewTransaction treats omitted fields
	// as "not set"; we use *string for every optional string to produce
	// a clean body that omits zero values.
	approved := true
	if in.Approved != nil {
		approved = *in.Approved
	}
	cleared := in.Cleared
	if cleared == "" {
		cleared = "uncleared"
	}
	date := in.Date
	if date == "" {
		date = time.Now().UTC().Format("2006-01-02")
	}
	body := wireNewTransactionWrapper{
		Transaction: wireNewTransaction{
			AccountID: in.AccountID,
			Date:      date,
			Amount:    in.AmountMilliunits,
			Cleared:   cleared,
			Approved:  approved,
		},
	}
	if in.PayeeID != "" {
		body.Transaction.PayeeID = &in.PayeeID
	}
	if in.PayeeName != "" {
		body.Transaction.PayeeName = &in.PayeeName
	}
	if in.CategoryID != "" {
		body.Transaction.CategoryID = &in.CategoryID
	}
	if in.Memo != "" {
		body.Transaction.Memo = &in.Memo
	}
	if in.ImportID != "" {
		body.Transaction.ImportID = &in.ImportID
	}

	path := "/plans/" + url.PathEscape(in.PlanID) + "/transactions"
	var wire wireSaveTransactionsResponse
	if err := c.doJSONWithBody(ctx, http.MethodPost, path, nil, body, &wire); err != nil {
		return nil, CreateTransactionOutput{}, sanitizedErr(err)
	}

	out := CreateTransactionOutput{
		Transaction: toTransaction(wire.Data.Transaction),
		Before:      nil,
	}

	// Fetch the account balance for the "after" snapshot. If this fails,
	// we still return a successful result — the transaction itself was
	// created, and the response's Transaction.ID identifies it.
	acctPath := "/plans/" + url.PathEscape(in.PlanID) + "/accounts/" + url.PathEscape(in.AccountID)
	var acctWire wireSingleAccountResponse
	if err := c.doJSON(ctx, acctPath, nil, &acctWire); err == nil {
		balance := NewMoney(acctWire.Data.Account.Balance)
		out.After = &balance
	}
	return nil, out, nil
}

// UpdateCategoryBudgeted changes the assigned (budgeted) amount for a
// single category in a single month. The primitive for Sunday-ritual
// Rule 3 money moves. Returns before/after snapshots of budgeted and
// balance so the skill can render a diff and log an audit entry.
func (c *Client) UpdateCategoryBudgeted(ctx context.Context, req *mcp.CallToolRequest, in UpdateCategoryBudgetedInput) (*mcp.CallToolResult, UpdateCategoryBudgetedOutput, error) {
	if err := requireWriteAllowed(); err != nil {
		return nil, UpdateCategoryBudgetedOutput{}, sanitizedErr(err)
	}
	if in.PlanID == "" {
		return nil, UpdateCategoryBudgetedOutput{}, errors.New("plan_id is required")
	}
	if in.CategoryID == "" {
		return nil, UpdateCategoryBudgetedOutput{}, errors.New("category_id is required")
	}
	month := in.Month
	if month == "" {
		return nil, UpdateCategoryBudgetedOutput{}, errors.New("month is required (YYYY-MM-01 or 'current')")
	}
	if err := checkAmountBound(in.NewBudgetedMilliunits, in.AmountOverrideMilliunits); err != nil {
		return nil, UpdateCategoryBudgetedOutput{}, sanitizedErr(err)
	}

	basePath := "/plans/" + url.PathEscape(in.PlanID) +
		"/months/" + url.PathEscape(month) +
		"/categories/" + url.PathEscape(in.CategoryID)

	// Fetch current state for the "before" snapshot. This is an extra
	// round-trip cost per write (~1 request), but it guarantees the
	// before value is what YNAB actually had, not an LLM guess.
	var beforeWire wireCategoryResponse
	if err := c.doJSON(ctx, basePath, nil, &beforeWire); err != nil {
		return nil, UpdateCategoryBudgetedOutput{}, sanitizedErr(err)
	}
	before := CategorySnapshot{
		Budgeted: NewMoney(beforeWire.Data.Category.Budgeted),
		Balance:  NewMoney(beforeWire.Data.Category.Balance),
	}

	// Elicit confirmation AFTER we know the "before" state so the user
	// sees the exact delta.
	delta := in.NewBudgetedMilliunits - beforeWire.Data.Category.Budgeted
	msg := fmt.Sprintf(
		"Update YNAB category budgeted: %q (%s) from %s to %s (change: %s)",
		beforeWire.Data.Category.Name,
		deref(beforeWire.Data.Category.Note),
		formatSignedMoney(beforeWire.Data.Category.Budgeted),
		formatSignedMoney(in.NewBudgetedMilliunits),
		formatSignedMoney(delta),
	)
	if err := elicitConfirmation(ctx, req.Session, msg); err != nil {
		return nil, UpdateCategoryBudgetedOutput{}, sanitizedErr(err)
	}

	// PATCH with only the budgeted field. YNAB's API explicitly documents
	// that "only budgeted (assigned) amount can be updated and any other
	// fields specified will be ignored."
	body := wirePatchMonthCategoryWrapper{
		Category: wireSaveMonthCategory{
			Budgeted: in.NewBudgetedMilliunits,
		},
	}
	var afterWire wireCategoryResponse
	if err := c.doJSONWithBody(ctx, http.MethodPatch, basePath, nil, body, &afterWire); err != nil {
		return nil, UpdateCategoryBudgetedOutput{}, sanitizedErr(err)
	}

	return nil, UpdateCategoryBudgetedOutput{
		Category: toCategory(afterWire.Data.Category),
		Before:   before,
		After: CategorySnapshot{
			Budgeted: NewMoney(afterWire.Data.Category.Budgeted),
			Balance:  NewMoney(afterWire.Data.Category.Balance),
		},
	}, nil
}

// ============================================================================
// Small helpers used across write tools.
// ============================================================================

// formatSignedMoney renders a milliunit amount as a signed currency string
// using the same 3-decimal formatting as Money.Decimal. Used in elicitation
// messages where a concise signed value is clearer than a nested Money
// object.
func formatSignedMoney(milliunits int64) string {
	return formatMilliunits(milliunits)
}

// orDefault returns s if non-empty, otherwise def.
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
