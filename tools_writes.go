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
	"strings"
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
	// Before is always null for create_transaction — there is no prior
	// state for a newly-created entity. It's declared as *struct{} rather
	// than `any` or a dedicated sentinel so the field's presence in the
	// response is unambiguous to JSON-schema-aware clients: present,
	// null, and typed. If you're wondering why this isn't just omitted:
	// the skill's audit log writer reads it explicitly to confirm it's
	// handling a create vs an update, and absence would be indistinguishable
	// from "before snapshot failed to fetch".
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

// UpdateTransactionInput is the partial-update shape for update_transaction.
// All mutable fields are pointer types so nil unambiguously means "leave
// this field alone" while non-nil means "set it". At least one mutable field
// must be non-nil or the call is rejected as a no-op.
//
// Amount is DELIBERATELY NOT a field on this struct. Amount changes via an
// LLM are too easy to get wrong; users who need to change an amount should
// delete the transaction in YNAB and create a new one. The absence of an
// amount field here is enforced by regression test.
type UpdateTransactionInput struct {
	PlanID        string `json:"plan_id" jsonschema:"YNAB plan id (UUID), or 'last-used' / 'default'"`
	TransactionID string `json:"transaction_id" jsonschema:"id of the transaction to update"`

	CategoryID *string `json:"category_id,omitempty" jsonschema:"new category id (UUID); nil to leave unchanged"`
	PayeeID    *string `json:"payee_id,omitempty" jsonschema:"new payee id (UUID); nil to leave unchanged"`
	PayeeName  *string `json:"payee_name,omitempty" jsonschema:"new payee name (max 200 chars); nil to leave unchanged"`
	Memo       *string `json:"memo,omitempty" jsonschema:"new memo (max 500 chars); nil to leave unchanged"`
	Approved   *bool   `json:"approved,omitempty" jsonschema:"new approved state; nil to leave unchanged"`
	Cleared    *string `json:"cleared,omitempty" jsonschema:"cleared|uncleared|reconciled; nil to leave unchanged"`
	FlagColor  *string `json:"flag_color,omitempty" jsonschema:"red|orange|yellow|green|blue|purple; nil to leave unchanged"`
}

// UpdateTransactionOutput returns the updated transaction plus before/after
// snapshots containing ONLY the fields that actually changed. Fields the
// caller did not touch are absent from both snapshots.
type UpdateTransactionOutput struct {
	Transaction Transaction         `json:"transaction"`
	Before      TransactionSnapshot `json:"before"`
	After       TransactionSnapshot `json:"after"`
}

// TransactionSnapshot holds any subset of the mutable fields on a transaction.
// All fields use pointer types so we can distinguish "changed to empty" from
// "not changed". Fields not present in both Before and After are absent from
// the JSON output (omitempty).
type TransactionSnapshot struct {
	CategoryID   *string `json:"category_id,omitempty"`
	CategoryName *string `json:"category_name,omitempty"`
	PayeeID      *string `json:"payee_id,omitempty"`
	PayeeName    *string `json:"payee_name,omitempty"`
	Memo         *string `json:"memo,omitempty"`
	Approved     *bool   `json:"approved,omitempty"`
	Cleared      *string `json:"cleared,omitempty"`
	FlagColor    *string `json:"flag_color,omitempty"`
}

// ApproveTransactionInput is the minimal shape for approve_transaction: it
// sets approved=true on an existing transaction. No other fields are
// accepted.
type ApproveTransactionInput struct {
	PlanID        string `json:"plan_id" jsonschema:"YNAB plan id (UUID), or 'last-used' / 'default'"`
	TransactionID string `json:"transaction_id" jsonschema:"id of the transaction to approve"`
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

// wirePutTransactionWrapper is the PUT body for /plans/{id}/transactions/{txn_id}.
// Matches YNAB's PutTransactionWrapper schema; the inner Transaction is
// SaveTransactionWithOptionalFields. We DO NOT include amount as a struct
// field: by structural absence, it is impossible for an amount change to
// slip through the write path.
type wirePutTransactionWrapper struct {
	Transaction wireUpdateTransaction `json:"transaction"`
}

// wireUpdateTransaction matches the fields of SaveTransactionWithOptionalFields
// we allow updating via this tool. Notably missing: amount, account_id, date.
// Everything is a pointer because YNAB treats omitted JSON fields as
// "leave unchanged".
type wireUpdateTransaction struct {
	CategoryID *string `json:"category_id,omitempty"`
	PayeeID    *string `json:"payee_id,omitempty"`
	PayeeName  *string `json:"payee_name,omitempty"`
	Memo       *string `json:"memo,omitempty"`
	Approved   *bool   `json:"approved,omitempty"`
	Cleared    *string `json:"cleared,omitempty"`
	FlagColor  *string `json:"flag_color,omitempty"`
}

// wireTransactionResponse matches YNAB's TransactionResponse schema: a
// single-transaction response from GET / PUT / DELETE on a transaction.
type wireTransactionResponse struct {
	Data struct {
		Transaction wireTransaction `json:"transaction"`
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

// UpdateTransaction applies a partial update to an existing transaction.
// At least one of the mutable fields must be non-nil; otherwise the call
// is rejected as a no-op. Amount is NOT a field on the input struct by
// design — amount changes are too error-prone via an LLM and are not
// supported by this tool. Users who need to change an amount should delete
// the transaction in the YNAB app and create a new one.
func (c *Client) UpdateTransaction(ctx context.Context, req *mcp.CallToolRequest, in UpdateTransactionInput) (*mcp.CallToolResult, UpdateTransactionOutput, error) {
	if err := requireWriteAllowed(); err != nil {
		return nil, UpdateTransactionOutput{}, sanitizedErr(err)
	}
	if in.PlanID == "" {
		return nil, UpdateTransactionOutput{}, errors.New("plan_id is required")
	}
	if in.TransactionID == "" {
		return nil, UpdateTransactionOutput{}, errors.New("transaction_id is required")
	}

	// Validate mutable field subset and collect the non-nil ones for the
	// "fields changed" set. If nothing was specified, reject as a no-op.
	if in.CategoryID == nil && in.PayeeID == nil && in.PayeeName == nil &&
		in.Memo == nil && in.Approved == nil && in.Cleared == nil && in.FlagColor == nil {
		return nil, UpdateTransactionOutput{}, errors.New("at least one field must be specified to update")
	}

	// Length and enum validations.
	if in.PayeeName != nil && len(*in.PayeeName) > 200 {
		return nil, UpdateTransactionOutput{}, errors.New("payee_name must be at most 200 characters")
	}
	if in.Memo != nil && len(*in.Memo) > 500 {
		return nil, UpdateTransactionOutput{}, errors.New("memo must be at most 500 characters")
	}
	if in.Cleared != nil {
		switch *in.Cleared {
		case "cleared", "uncleared", "reconciled":
		default:
			return nil, UpdateTransactionOutput{}, errors.New("cleared must be one of: cleared, uncleared, reconciled")
		}
	}
	if in.FlagColor != nil {
		switch *in.FlagColor {
		case "red", "orange", "yellow", "green", "blue", "purple", "":
		default:
			return nil, UpdateTransactionOutput{}, errors.New("flag_color must be one of: red, orange, yellow, green, blue, purple, or empty string to clear")
		}
	}

	return c.doUpdateTransaction(ctx, req, in, true /* elicit */)
}

// ApproveTransaction sets approved=true on an existing transaction. Thin
// wrapper over UpdateTransaction with only the approved field populated.
//
// Unlike UpdateTransaction, approve_transaction does NOT elicit per-call
// confirmation: its use case is batch "daily pending cleanup" where a skill
// invokes it many times in a loop, and prompting on every call would break
// the batch workflow. The YNAB_ALLOW_WRITES env-var gate remains the
// primary defense. Approval is a low-stakes operation compared to the
// other write tools — it does not change amounts, categories, or payees.
func (c *Client) ApproveTransaction(ctx context.Context, req *mcp.CallToolRequest, in ApproveTransactionInput) (*mcp.CallToolResult, UpdateTransactionOutput, error) {
	if err := requireWriteAllowed(); err != nil {
		return nil, UpdateTransactionOutput{}, sanitizedErr(err)
	}
	if in.PlanID == "" {
		return nil, UpdateTransactionOutput{}, errors.New("plan_id is required")
	}
	if in.TransactionID == "" {
		return nil, UpdateTransactionOutput{}, errors.New("transaction_id is required")
	}
	approved := true
	return c.doUpdateTransaction(ctx, req, UpdateTransactionInput{
		PlanID:        in.PlanID,
		TransactionID: in.TransactionID,
		Approved:      &approved,
	}, false /* skip elicitation for batch approval flow */)
}

// doUpdateTransaction is the shared implementation of UpdateTransaction and
// ApproveTransaction. It fetches the current transaction, (optionally)
// elicits confirmation, issues the PUT, and builds the before/after snapshot
// containing only the fields that actually changed.
func (c *Client) doUpdateTransaction(ctx context.Context, req *mcp.CallToolRequest, in UpdateTransactionInput, elicit bool) (*mcp.CallToolResult, UpdateTransactionOutput, error) {
	basePath := "/plans/" + url.PathEscape(in.PlanID) + "/transactions/" + url.PathEscape(in.TransactionID)

	// Fetch current state for the "before" snapshot.
	var beforeWire wireTransactionResponse
	if err := c.doJSON(ctx, basePath, nil, &beforeWire); err != nil {
		return nil, UpdateTransactionOutput{}, sanitizedErr(err)
	}

	if elicit {
		msg := buildUpdateTransactionElicitMessage(beforeWire.Data.Transaction, in)
		if err := elicitConfirmation(ctx, req.Session, msg); err != nil {
			return nil, UpdateTransactionOutput{}, sanitizedErr(err)
		}
	}

	// Build the PUT body with only the fields the caller set.
	body := wirePutTransactionWrapper{
		Transaction: wireUpdateTransaction{
			CategoryID: in.CategoryID,
			PayeeID:    in.PayeeID,
			PayeeName:  in.PayeeName,
			Memo:       in.Memo,
			Approved:   in.Approved,
			Cleared:    in.Cleared,
			FlagColor:  in.FlagColor,
		},
	}

	var afterWire wireTransactionResponse
	if err := c.doJSONWithBody(ctx, http.MethodPut, basePath, nil, body, &afterWire); err != nil {
		return nil, UpdateTransactionOutput{}, sanitizedErr(err)
	}

	return nil, UpdateTransactionOutput{
		Transaction: toTransaction(afterWire.Data.Transaction),
		Before:      buildTransactionSnapshotBefore(beforeWire.Data.Transaction, in),
		After:       buildTransactionSnapshotAfter(afterWire.Data.Transaction, in),
	}, nil
}

// buildTransactionSnapshotBefore returns a TransactionSnapshot containing
// the OLD values of the fields that the caller specified in the update
// request. Fields not touched by the caller are nil in the snapshot.
func buildTransactionSnapshotBefore(before wireTransaction, in UpdateTransactionInput) TransactionSnapshot {
	var s TransactionSnapshot
	if in.CategoryID != nil {
		s.CategoryID = before.CategoryID
		s.CategoryName = before.CategoryName
	}
	if in.PayeeID != nil || in.PayeeName != nil {
		s.PayeeID = before.PayeeID
		s.PayeeName = before.PayeeName
	}
	if in.Memo != nil {
		s.Memo = before.Memo
	}
	if in.Approved != nil {
		approved := before.Approved
		s.Approved = &approved
	}
	if in.Cleared != nil {
		c := before.Cleared
		s.Cleared = &c
	}
	if in.FlagColor != nil {
		s.FlagColor = before.FlagColor
	}
	return s
}

// buildTransactionSnapshotAfter returns a TransactionSnapshot containing
// the NEW values of the fields that the caller specified, taken from the
// post-update transaction returned by YNAB.
func buildTransactionSnapshotAfter(after wireTransaction, in UpdateTransactionInput) TransactionSnapshot {
	var s TransactionSnapshot
	if in.CategoryID != nil {
		s.CategoryID = after.CategoryID
		s.CategoryName = after.CategoryName
	}
	if in.PayeeID != nil || in.PayeeName != nil {
		s.PayeeID = after.PayeeID
		s.PayeeName = after.PayeeName
	}
	if in.Memo != nil {
		s.Memo = after.Memo
	}
	if in.Approved != nil {
		approved := after.Approved
		s.Approved = &approved
	}
	if in.Cleared != nil {
		c := after.Cleared
		s.Cleared = &c
	}
	if in.FlagColor != nil {
		s.FlagColor = after.FlagColor
	}
	return s
}

// buildUpdateTransactionElicitMessage renders a human-readable confirmation
// message describing what's about to change, for display by the MCP client
// during elicitation.
//
// Note on the name/ID asymmetry: the "before" values come from YNAB and
// carry human-readable names (CategoryName, PayeeName). The "after" values
// come from the LLM and are raw UUIDs because the update_transaction
// tool takes IDs, not names. Rendering the new value as "Groceries →
// a1b2c3..." would look misleading, so we label it explicitly as
// "(new id=...)" to set the user's expectation that they are verifying
// an ID change, not a name change. Review finding M2.
func buildUpdateTransactionElicitMessage(before wireTransaction, in UpdateTransactionInput) string {
	var changes []string
	if in.CategoryID != nil {
		changes = append(changes, fmt.Sprintf("category: %s → (new id=%s)", deref(before.CategoryName), *in.CategoryID))
	}
	if in.PayeeID != nil {
		changes = append(changes, fmt.Sprintf("payee: %s → (new id=%s)", deref(before.PayeeName), *in.PayeeID))
	}
	if in.PayeeName != nil {
		changes = append(changes, fmt.Sprintf("payee_name: %q → %q", deref(before.PayeeName), *in.PayeeName))
	}
	if in.Memo != nil {
		changes = append(changes, fmt.Sprintf("memo: %q → %q", deref(before.Memo), *in.Memo))
	}
	if in.Approved != nil {
		changes = append(changes, fmt.Sprintf("approved: %v → %v", before.Approved, *in.Approved))
	}
	if in.Cleared != nil {
		changes = append(changes, fmt.Sprintf("cleared: %s → %s", before.Cleared, *in.Cleared))
	}
	if in.FlagColor != nil {
		changes = append(changes, fmt.Sprintf("flag: %s → %s", deref(before.FlagColor), *in.FlagColor))
	}
	summary := "(no changes)"
	if len(changes) > 0 {
		summary = strings.Join(changes, ", ")
	}
	return fmt.Sprintf(
		"Update YNAB transaction %s (amount: %s, date: %s, payee: %s): %s",
		in.TransactionID,
		formatSignedMoney(before.Amount),
		before.Date,
		deref(before.PayeeName),
		summary,
	)
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
