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
	"unicode/utf8"

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
	// Memo is capped at 200 characters on create — this is a project
	// policy (not YNAB's limit, which is 500). Create-time memos should
	// be short descriptive strings; longer context belongs on update.
	Memo       string `json:"memo,omitempty" jsonschema:"freeform memo, max 200 characters (project policy; YNAB's server limit is 500)"`
	Date       string `json:"date,omitempty" jsonschema:"ISO date (YYYY-MM-DD); defaults to today (UTC) if omitted"`
	Cleared    string `json:"cleared,omitempty" jsonschema:"cleared|uncleared|reconciled; defaults to 'uncleared'"`
	Approved   *bool  `json:"approved,omitempty" jsonschema:"defaults to true; pass false to leave the transaction unapproved"`
	ImportID   string `json:"import_id,omitempty" jsonschema:"optional idempotency key; YNAB dedupes on this. Max 36 characters."`

	AmountOverrideMilliunits int64 `json:"amount_override_milliunits,omitempty" jsonschema:"if |amount_milliunits| exceeds the $10K safety threshold, set this to the same value as amount_milliunits to acknowledge the large transaction"`
}

// CreateTransactionOutput is the response shape for create_transaction.
// Before is always null for a create (no prior state); After carries the
// account balance snapshot after the insert so the skill can render a
// diff and persist an audit entry.
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

// UpdateCategoryBudgetedOutput returns the post-update category plus
// before/after CategorySnapshot values so the caller can compute the
// exact delta applied to Rule 3 money moves.
type UpdateCategoryBudgetedOutput struct {
	Category Category         `json:"category"`
	Before   CategorySnapshot `json:"before"`
	After    CategorySnapshot `json:"after"`
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
	Cleared    *string `json:"cleared,omitempty"`
	Approved   bool    `json:"approved"`
	ImportID   *string `json:"import_id,omitempty"`
}

// wireSaveTransactionsResponse matches YNAB's SaveTransactionsResponse
// schema. For a single-transaction POST, YNAB populates .data.transaction
// on a fresh insert. On the retry-dedup path (same import_id as a prior
// post), YNAB instead returns the duplicate id in .data.duplicate_import_ids
// and may leave .data.transaction zero-valued. We decode both fields so
// the handler can detect the dedup case and surface it explicitly rather
// than returning a zero-value Transaction with ID="" masquerading as
// success. Review finding H2.
type wireSaveTransactionsResponse struct {
	Data struct {
		Transaction        wireTransaction `json:"transaction"`
		Transactions       []wireTransaction `json:"transactions"`
		TransactionIDs     []string        `json:"transaction_ids"`
		DuplicateImportIDs []string        `json:"duplicate_import_ids"`
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
	if err := validatePlanID(in.PlanID); err != nil {
		return nil, CreateTransactionOutput{}, err
	}
	if err := validateEntityID("account_id", in.AccountID); err != nil {
		return nil, CreateTransactionOutput{}, err
	}
	if in.CategoryID != "" {
		if err := validateEntityID("category_id", in.CategoryID); err != nil {
			return nil, CreateTransactionOutput{}, err
		}
	}
	if in.PayeeID != "" {
		if err := validateEntityID("payee_id", in.PayeeID); err != nil {
			return nil, CreateTransactionOutput{}, err
		}
	}
	if in.PayeeName == "" && in.PayeeID == "" {
		return nil, CreateTransactionOutput{}, errors.New("one of payee_name or payee_id is required")
	}
	// Memo length measured in runes, not bytes — so a multi-byte CJK or
	// emoji memo is accepted up to 200 user-visible characters. Review
	// finding on memo length byte-vs-rune.
	if in.Memo != "" && utf8.RuneCountInString(in.Memo) > 200 {
		return nil, CreateTransactionOutput{}, errors.New("memo must be at most 200 characters")
	}
	if in.ImportID != "" && len(in.ImportID) > 36 {
		return nil, CreateTransactionOutput{}, errors.New("import_id must be at most 36 characters")
	}
	// Validate date if supplied; fall-through defaults to today (UTC)
	// below. Catches LLM-generated strings like "yesterday" or
	// "2026-13-01" at the boundary instead of YNAB returning an opaque
	// http 400. Review finding H3.
	if in.Date != "" {
		if err := validateISODate(in.Date); err != nil {
			return nil, CreateTransactionOutput{}, errors.New("date " + err.Error())
		}
	}
	// Validate cleared enum if caller supplied it. YNAB's API accepts only
	// these three values; pre-validating here gives a clear error instead
	// of a generic upstream 400. Matches the equivalent check in
	// UpdateTransaction. Review finding H3.
	if in.Cleared != "" {
		switch in.Cleared {
		case "cleared", "uncleared", "reconciled":
		default:
			return nil, CreateTransactionOutput{}, errors.New("cleared must be one of: cleared, uncleared, reconciled")
		}
	}
	// Mutually-exclusive payee fields: allowing both creates undocumented
	// precedence semantics at YNAB. Review finding M5.
	if in.PayeeID != "" && in.PayeeName != "" {
		return nil, CreateTransactionOutput{}, errors.New("at most one of payee_id or payee_name may be set")
	}
	if err := checkAmountBound(in.AmountMilliunits, in.AmountOverrideMilliunits); err != nil {
		return nil, CreateTransactionOutput{}, sanitizedErr(err)
	}

	// Pre-fetch the target account so the elicitation message can show
	// a human-readable name instead of an opaque UUID. The user has to
	// be able to eyeball "is this the right account?" during
	// confirmation. If the account fetch fails (network error, wrong
	// id), fall back to the id and let the user decide. Review finding M9.
	acctPath := "/plans/" + url.PathEscape(in.PlanID) + "/accounts/" + url.PathEscape(in.AccountID)
	var preAcctWire wireSingleAccountResponse
	accountLabel := in.AccountID
	if err := c.doJSON(ctx, acctPath, nil, &preAcctWire); err == nil && preAcctWire.Data.Account.Name != "" {
		accountLabel = fmt.Sprintf("%q (%s)", preAcctWire.Data.Account.Name, in.AccountID)
	}

	// Elicit confirmation with a specific summary. The exact message is
	// what the user sees on their MCP client.
	msg := fmt.Sprintf(
		"Create YNAB transaction: %s on %s to %q (account %s)",
		formatSignedMoney(in.AmountMilliunits),
		orDefault(in.Date, nowUTC().Format("2006-01-02")),
		orDefault(in.PayeeName, in.PayeeID),
		accountLabel,
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
		date = nowUTC().Format("2006-01-02")
	}
	body := wireNewTransactionWrapper{
		Transaction: wireNewTransaction{
			AccountID: in.AccountID,
			Date:      date,
			Amount:    in.AmountMilliunits,
			// Cleared is a *string so the defaulted value is always
			// present in the POST body even if future code removes the
			// "uncleared" default above. Review finding H4.
			Cleared:  &cleared,
			Approved: approved,
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
		// Scrub caller-provided strings from the error surface. If a
		// pathological transport echoes the request body verbatim (or
		// YNAB's own error detail happens to contain the memo), the
		// extra redaction prevents user-submitted content from reaching
		// the MCP client. The environment-level sanitize() handles
		// Bearer/Authorization patterns; this additional scrub handles
		// memo and payee_name.
		return nil, CreateTransactionOutput{}, sanitizedErrWith(err, in.Memo, in.PayeeName)
	}

	// Dedup detection: YNAB may return the transaction either as
	// .data.transaction (fresh insert) or as the first element of
	// .data.transactions (bulk-shape response). On the import_id retry
	// path, .data.transaction is zero-valued and .data.duplicate_import_ids
	// carries the original id. We MUST NOT return a zero-value
	// Transaction{} — that would look like success with ID="". Review
	// finding H2.
	txn := wire.Data.Transaction
	if txn.ID == "" && len(wire.Data.Transactions) > 0 {
		txn = wire.Data.Transactions[0]
	}
	if txn.ID == "" {
		if len(wire.Data.DuplicateImportIDs) > 0 {
			return nil, CreateTransactionOutput{}, fmt.Errorf(
				"create_transaction: YNAB returned duplicate_import_ids (import_id already used); "+
					"the prior transaction with this import_id was not modified. "+
					"To post a new transaction, use a fresh import_id. Duplicates reported: %v",
				wire.Data.DuplicateImportIDs)
		}
		return nil, CreateTransactionOutput{}, errors.New(
			"create_transaction: YNAB response contained no transaction id — refusing to report success")
	}

	out := CreateTransactionOutput{
		Transaction: toTransaction(txn),
		Before:      nil,
	}

	// Fetch the account balance for the "after" snapshot. If this fails,
	// we still return a successful result — the transaction itself was
	// created, and the response's Transaction.ID identifies it.
	// Re-use acctPath from the pre-fetch step above.
	var postAcctWire wireSingleAccountResponse
	if err := c.doJSON(ctx, acctPath, nil, &postAcctWire); err == nil {
		balance := NewMoney(postAcctWire.Data.Account.Balance)
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
	if err := validatePlanID(in.PlanID); err != nil {
		return nil, UpdateCategoryBudgetedOutput{}, err
	}
	if err := validateEntityID("category_id", in.CategoryID); err != nil {
		return nil, UpdateCategoryBudgetedOutput{}, err
	}
	month := in.Month
	if month == "" {
		return nil, UpdateCategoryBudgetedOutput{}, errors.New("month is required (YYYY-MM-01 or 'current')")
	}
	if err := validateYNABMonth(month); err != nil {
		return nil, UpdateCategoryBudgetedOutput{}, errors.New("month " + err.Error())
	}
	// YNAB's PATCH month-category docs enumerate only ISO dates in the
	// URL path — "current" is a GET convenience only. Resolve to a
	// concrete YYYY-MM-01 before building the URL so PATCH gets a
	// documented value. Review finding H4.
	if month == "current" {
		month = nowUTC().Format("2006-01") + "-01"
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
	// sees the exact delta. The category note is omitted from the
	// confirmation summary — notes can contain sensitive or long text
	// and would otherwise land in elicitation logs. The user can verify
	// the category by its name alone. Review finding M10.
	delta := in.NewBudgetedMilliunits - beforeWire.Data.Category.Budgeted

	// Safety cap guards the DELTA, not the target value. Moving $50K
	// between categories can leave both new totals under the $10K target
	// threshold while still shifting $50K of money around — so we must
	// gate on |new - before|. Computed after the before-snapshot fetch
	// because delta depends on YNAB's actual state. Review finding H1.
	if err := checkAmountBound(delta, in.AmountOverrideMilliunits); err != nil {
		return nil, UpdateCategoryBudgetedOutput{}, sanitizedErr(err)
	}
	// Include plan_id and month in the confirmation so a user with
	// multiple plans or mid-year shifts sees exactly which plan/month
	// is being mutated. The category name is truncated to guard against
	// a plan-import that created a multi-kilobyte category name — the
	// elicitation prompt is rendered verbatim by the MCP client and an
	// unbounded name would mangle the UI. Review finding on
	// UpdateCategoryBudgeted elicitation missing plan_id/month and on
	// unbounded display strings in prompts.
	msg := fmt.Sprintf(
		"Update YNAB category budgeted in plan %s, month %s: %q from %s to %s (change: %s)",
		in.PlanID,
		month,
		truncateForDisplay(beforeWire.Data.Category.Name, 80),
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
	if err := validatePlanID(in.PlanID); err != nil {
		return nil, UpdateTransactionOutput{}, err
	}
	if err := validateEntityID("transaction_id", in.TransactionID); err != nil {
		return nil, UpdateTransactionOutput{}, err
	}

	// Validate mutable field subset and collect the non-nil ones for the
	// "fields changed" set. If nothing was specified, reject as a no-op.
	if in.CategoryID == nil && in.PayeeID == nil && in.PayeeName == nil &&
		in.Memo == nil && in.Approved == nil && in.Cleared == nil && in.FlagColor == nil {
		return nil, UpdateTransactionOutput{}, errors.New("at least one field must be specified to update")
	}

	// UUID shape checks on the optional scope-changing fields. PayeeName
	// is a free-form string and is NOT validated here — only id fields
	// are. Review finding on broader UUID validation.
	if in.CategoryID != nil {
		if err := validateEntityID("category_id", *in.CategoryID); err != nil {
			return nil, UpdateTransactionOutput{}, err
		}
	}
	if in.PayeeID != nil {
		if err := validateEntityID("payee_id", *in.PayeeID); err != nil {
			return nil, UpdateTransactionOutput{}, err
		}
	}

	// Length checks operate on runes, not bytes. The docs and the error
	// message both say "200 characters" and "500 characters"; a
	// byte-based len() would reject a legitimate CJK/emoji memo at ~67
	// runes while the limit says 200. Review finding on memo length
	// byte-vs-rune.
	if in.PayeeName != nil && utf8.RuneCountInString(*in.PayeeName) > 200 {
		return nil, UpdateTransactionOutput{}, errors.New("payee_name must be at most 200 characters")
	}
	if in.Memo != nil && utf8.RuneCountInString(*in.Memo) > 500 {
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
	if err := validatePlanID(in.PlanID); err != nil {
		return nil, UpdateTransactionOutput{}, err
	}
	if err := validateEntityID("transaction_id", in.TransactionID); err != nil {
		return nil, UpdateTransactionOutput{}, err
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

	// Collect caller-supplied fields that could echo back through an
	// error surface (a pathological proxy reflecting the request body,
	// YNAB validation-error.detail containing the memo, etc.). Passed to
	// sanitizedErrWith on every error path in this handler so the memo
	// and payee_name scrub symmetrically with CreateTransaction — the
	// prior asymmetry meant UpdateTransaction leaked memos while
	// CreateTransaction scrubbed them. Review finding on sanitizedErrWith
	// asymmetry.
	redactMemo := deref(in.Memo)
	redactPayee := deref(in.PayeeName)

	// Fetch current state for the "before" snapshot.
	var beforeWire wireTransactionResponse
	if err := c.doJSON(ctx, basePath, nil, &beforeWire); err != nil {
		return nil, UpdateTransactionOutput{}, sanitizedErrWith(err, redactMemo, redactPayee)
	}

	// Reject a no-op update and a deleted-transaction update before
	// issuing the PUT. A no-op update would prompt the user, send a
	// PATCH that changes nothing, and audit the non-change — all noise.
	// A deleted-transaction update would trigger a confusing YNAB 400
	// and leave the audit trail implying the tool tried to modify a
	// tombstone. Review finding on no-op / deleted-tx short-circuit.
	if beforeWire.Data.Transaction.Deleted {
		return nil, UpdateTransactionOutput{}, errors.New("transaction is deleted; cannot update a tombstoned transaction")
	}
	if isUpdateNoOp(beforeWire.Data.Transaction, in) {
		return nil, UpdateTransactionOutput{}, errors.New("update is a no-op: every specified field already matches the current value")
	}

	if elicit {
		msg := buildUpdateTransactionElicitMessage(beforeWire.Data.Transaction, in)
		if err := elicitConfirmation(ctx, req.Session, msg); err != nil {
			return nil, UpdateTransactionOutput{}, sanitizedErrWith(err, redactMemo, redactPayee)
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
		return nil, UpdateTransactionOutput{}, sanitizedErrWith(err, redactMemo, redactPayee)
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
	// Display caps. Memos can legally reach 500 chars and payee names can
	// be arbitrarily long on plans imported from other tools; embedding
	// them verbatim would balloon the elicitation prompt the MCP client
	// renders. Truncate to 80 chars per field with an ellipsis suffix so
	// the prompt stays compact while preserving enough text for the user
	// to verify what they are confirming. Review finding on unbounded
	// elicitation strings.
	const shortMax = 80
	const memoMax = 120
	var changes []string
	if in.CategoryID != nil {
		changes = append(changes, fmt.Sprintf("category: %s → (new id=%s)", truncateForDisplay(deref(before.CategoryName), shortMax), *in.CategoryID))
	}
	if in.PayeeID != nil {
		changes = append(changes, fmt.Sprintf("payee: %s → (new id=%s)", truncateForDisplay(deref(before.PayeeName), shortMax), *in.PayeeID))
	} else if in.PayeeName != nil {
		changes = append(changes, fmt.Sprintf("payee_name: %q → %q", truncateForDisplay(deref(before.PayeeName), shortMax), truncateForDisplay(*in.PayeeName, shortMax)))
	}
	if in.Memo != nil {
		changes = append(changes, fmt.Sprintf("memo: %q → %q", truncateForDisplay(deref(before.Memo), memoMax), truncateForDisplay(*in.Memo, memoMax)))
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
		truncateForDisplay(deref(before.PayeeName), shortMax),
		summary,
	)
}

// truncateForDisplay returns s bounded to max runes (NOT bytes), appending
// an ellipsis when truncation occurred. Operates on runes so multi-byte
// UTF-8 sequences (CJK, emoji) are not cut mid-codepoint — the prior
// byte-based len() check would lop off the trailing byte of a character
// and render invalid text to MCP clients. Used for elicitation prompts
// and any other user-facing summary that embeds YNAB-supplied strings.
func truncateForDisplay(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
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

// isUpdateNoOp reports whether every field the caller specified in an
// update request already matches the current YNAB value. When true, the
// tool short-circuits before prompting, issuing the PUT, or writing an
// audit entry — an otherwise wasted round-trip that would also pollute
// the audit log with a zero-delta record. Review finding on
// doUpdateTransaction no-op short-circuit.
func isUpdateNoOp(before wireTransaction, in UpdateTransactionInput) bool {
	if in.CategoryID != nil && deref(before.CategoryID) != *in.CategoryID {
		return false
	}
	if in.PayeeID != nil && deref(before.PayeeID) != *in.PayeeID {
		return false
	}
	if in.PayeeName != nil && deref(before.PayeeName) != *in.PayeeName {
		return false
	}
	if in.Memo != nil && deref(before.Memo) != *in.Memo {
		return false
	}
	if in.Approved != nil && before.Approved != *in.Approved {
		return false
	}
	if in.Cleared != nil && before.Cleared != *in.Cleared {
		return false
	}
	if in.FlagColor != nil && deref(before.FlagColor) != *in.FlagColor {
		return false
	}
	return true
}

// orDefault returns s if non-empty, otherwise def.
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
