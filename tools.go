// SPDX-License-Identifier: MIT
//
// Tool handlers for the 5 read-only YNAB endpoints this server exposes.
// Each handler is an exported method on *Client so it can be tested directly
// without spinning up the full MCP server. registerTools wires them into an
// mcp.Server via the generic mcp.AddTool, which automatically derives input
// and output JSON schemas from the struct types and validates incoming
// arguments before the handler is called.
//
// All error paths run through sanitizedErr at the tool boundary as
// defense-in-depth. The SDK's SEP-1303 behavior (commit 74d2751) converts a
// returned error into CallToolResult{IsError:true} — a tool-result error,
// not a JSON-RPC protocol error — so the LLM can self-correct.
//
// All tools are marked ReadOnlyHint=true so conformant MCP clients know
// they can run without user confirmation.

package main

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---- inputs / outputs -------------------------------------------------------

// ListPlansInput has no fields; list_plans takes no arguments.
type ListPlansInput struct{}

type ListPlansOutput struct {
	Plans []Plan `json:"plans"`
}

type GetMonthInput struct {
	PlanID string `json:"plan_id" jsonschema:"YNAB plan id (UUID), or 'last-used' / 'default' to use the default plan"`
	Month  string `json:"month,omitempty" jsonschema:"ISO month (YYYY-MM-01) or 'current' for the current calendar month (UTC). Defaults to 'current'."`
}

type ListAccountsInput struct {
	PlanID        string `json:"plan_id" jsonschema:"YNAB plan id (UUID), or 'last-used' / 'default'"`
	IncludeClosed bool   `json:"include_closed,omitempty" jsonschema:"include closed accounts; default false"`
}

type ListAccountsOutput struct {
	Accounts []Account `json:"accounts"`
}

type ListTransactionsInput struct {
	PlanID     string `json:"plan_id" jsonschema:"YNAB plan id (UUID), or 'last-used' / 'default'"`
	SinceDate  string `json:"since_date,omitempty" jsonschema:"only include transactions on or after this ISO date (YYYY-MM-DD). Strongly recommended to avoid enormous responses."`
	Type       string `json:"type,omitempty" jsonschema:"filter: 'uncategorized' or 'unapproved'. Omit for all transactions."`
	Limit      int    `json:"limit,omitempty" jsonschema:"max transactions to return, most recent first. Default 100, max 500."`
	AccountID  string `json:"account_id,omitempty" jsonschema:"only include transactions for this account. At most one of account_id, category_id, or payee_id may be set."`
	CategoryID string `json:"category_id,omitempty" jsonschema:"only include transactions for this category. Split transactions are flattened to subtransaction rows. At most one of account_id, category_id, or payee_id may be set."`
	PayeeID    string `json:"payee_id,omitempty" jsonschema:"only include transactions for this payee. Split transactions are flattened to subtransaction rows. At most one of account_id, category_id, or payee_id may be set."`
}

type ListTransactionsOutput struct {
	Transactions []Transaction `json:"transactions"`
	Truncated    bool          `json:"truncated,omitempty" jsonschema:"true if more transactions were available than the limit returned"`
}

type ListMonthsInput struct {
	PlanID string `json:"plan_id" jsonschema:"YNAB plan id (UUID), or 'last-used' / 'default'"`
	Limit  int    `json:"limit,omitempty" jsonschema:"max months to return, most recent first. Default 6, max 60."`
}

type ListMonthsOutput struct {
	Months []MonthSummary `json:"months"`
}

type ListScheduledTransactionsInput struct {
	PlanID       string `json:"plan_id" jsonschema:"YNAB plan id (UUID), or 'last-used' / 'default'"`
	UpcomingDays int    `json:"upcoming_days,omitempty" jsonschema:"only include scheduled transactions whose next occurrence is within this many days. Default no filter."`
}

type ListScheduledTransactionsOutput struct {
	ScheduledTransactions []ScheduledTransaction `json:"scheduled_transactions"`
}

type ListCategoriesInput struct {
	PlanID        string `json:"plan_id" jsonschema:"YNAB plan id (UUID), or 'last-used' / 'default'"`
	IncludeHidden bool   `json:"include_hidden,omitempty" jsonschema:"include hidden categories; default false"`
}

type ListCategoriesOutput struct {
	Categories []Category `json:"categories"`
}

// ListPayeesInput supports a case-insensitive substring filter so the LLM
// can resolve "Chipotle" to a concrete payee_id without pulling the full
// payee list on every call. For plans with hundreds of payees this is
// meaningfully cheaper than always fetching everything.
type ListPayeesInput struct {
	PlanID         string `json:"plan_id" jsonschema:"YNAB plan id (UUID), or 'last-used' / 'default'"`
	NameContains   string `json:"name_contains,omitempty" jsonschema:"case-insensitive substring filter on payee name; omit to return all payees. No regex — plain substring only."`
	IncludeDeleted bool   `json:"include_deleted,omitempty" jsonschema:"include deleted payees; default false"`
}

type ListPayeesOutput struct {
	Payees []Payee `json:"payees"`
}

// ---- handler methods --------------------------------------------------------
//
// These are exported as methods on *Client to make them testable directly. The
// generic mcp.AddTool accepts a method value with the required
// (ctx, req, in) → (result, out, err) shape.

func (c *Client) ListPlans(ctx context.Context, _ *mcp.CallToolRequest, _ ListPlansInput) (*mcp.CallToolResult, ListPlansOutput, error) {
	var wire wirePlanSummaryResponse
	if err := c.doJSON(ctx, "/plans", nil, &wire); err != nil {
		return nil, ListPlansOutput{}, sanitizedErr(err)
	}
	plans := make([]Plan, 0, len(wire.Data.Plans))
	for _, p := range wire.Data.Plans {
		plans = append(plans, toPlan(p))
	}
	return nil, ListPlansOutput{Plans: plans}, nil
}

func (c *Client) GetMonth(ctx context.Context, _ *mcp.CallToolRequest, in GetMonthInput) (*mcp.CallToolResult, Month, error) {
	if in.PlanID == "" {
		return nil, Month{}, errors.New("plan_id is required")
	}
	month := in.Month
	if month == "" {
		month = "current"
	}
	path := "/plans/" + url.PathEscape(in.PlanID) + "/months/" + url.PathEscape(month)
	var wire wireMonthDetailResponse
	if err := c.doJSON(ctx, path, nil, &wire); err != nil {
		return nil, Month{}, sanitizedErr(err)
	}
	return nil, toMonth(wire.Data.Month), nil
}

func (c *Client) ListAccounts(ctx context.Context, _ *mcp.CallToolRequest, in ListAccountsInput) (*mcp.CallToolResult, ListAccountsOutput, error) {
	if in.PlanID == "" {
		return nil, ListAccountsOutput{}, errors.New("plan_id is required")
	}
	path := "/plans/" + url.PathEscape(in.PlanID) + "/accounts"
	var wire wireAccountsResponse
	if err := c.doJSON(ctx, path, nil, &wire); err != nil {
		return nil, ListAccountsOutput{}, sanitizedErr(err)
	}
	accounts := make([]Account, 0, len(wire.Data.Accounts))
	for _, a := range wire.Data.Accounts {
		if a.Deleted {
			continue
		}
		if a.Closed && !in.IncludeClosed {
			continue
		}
		accounts = append(accounts, toAccount(a))
	}
	return nil, ListAccountsOutput{Accounts: accounts}, nil
}

func (c *Client) ListTransactions(ctx context.Context, _ *mcp.CallToolRequest, in ListTransactionsInput) (*mcp.CallToolResult, ListTransactionsOutput, error) {
	if in.PlanID == "" {
		return nil, ListTransactionsOutput{}, errors.New("plan_id is required")
	}
	// At most one scope filter. YNAB has separate endpoints per scope and
	// combining them would require ambiguous client-side logic — cleaner
	// to make the LLM pick one and compose client-side if needed.
	scopeCount := 0
	if in.AccountID != "" {
		scopeCount++
	}
	if in.CategoryID != "" {
		scopeCount++
	}
	if in.PayeeID != "" {
		scopeCount++
	}
	if scopeCount > 1 {
		return nil, ListTransactionsOutput{}, errors.New("at most one of account_id, category_id, or payee_id may be set")
	}

	limit := in.Limit
	switch {
	case limit <= 0:
		limit = 100
	case limit > 500:
		limit = 500
	}
	q := url.Values{}
	if in.SinceDate != "" {
		q.Set("since_date", in.SinceDate)
	}
	if in.Type != "" {
		if in.Type != "uncategorized" && in.Type != "unapproved" {
			return nil, ListTransactionsOutput{}, errors.New("type must be 'uncategorized' or 'unapproved'")
		}
		q.Set("type", in.Type)
	}

	planPath := "/plans/" + url.PathEscape(in.PlanID)

	// The account endpoint returns TransactionsResponse (same shape as the
	// main endpoint). The category and payee endpoints return
	// HybridTransactionsResponse, where split transactions are flattened
	// to subtransaction rows so the scope filter works correctly.
	var rawRows []Transaction
	switch {
	case in.CategoryID != "":
		path := planPath + "/categories/" + url.PathEscape(in.CategoryID) + "/transactions"
		var wire wireHybridTransactionsResponse
		if err := c.doJSON(ctx, path, q, &wire); err != nil {
			return nil, ListTransactionsOutput{}, sanitizedErr(err)
		}
		rawRows = hybridToTransactions(wire.Data.Transactions)
	case in.PayeeID != "":
		path := planPath + "/payees/" + url.PathEscape(in.PayeeID) + "/transactions"
		var wire wireHybridTransactionsResponse
		if err := c.doJSON(ctx, path, q, &wire); err != nil {
			return nil, ListTransactionsOutput{}, sanitizedErr(err)
		}
		rawRows = hybridToTransactions(wire.Data.Transactions)
	case in.AccountID != "":
		path := planPath + "/accounts/" + url.PathEscape(in.AccountID) + "/transactions"
		var wire wireTransactionsResponse
		if err := c.doJSON(ctx, path, q, &wire); err != nil {
			return nil, ListTransactionsOutput{}, sanitizedErr(err)
		}
		rawRows = plainToTransactions(wire.Data.Transactions)
	default:
		path := planPath + "/transactions"
		var wire wireTransactionsResponse
		if err := c.doJSON(ctx, path, q, &wire); err != nil {
			return nil, ListTransactionsOutput{}, sanitizedErr(err)
		}
		rawRows = plainToTransactions(wire.Data.Transactions)
	}

	sort.Slice(rawRows, func(i, j int) bool {
		return rawRows[i].Date > rawRows[j].Date
	})
	truncated := len(rawRows) > limit
	if truncated {
		rawRows = rawRows[:limit]
	}
	return nil, ListTransactionsOutput{
		Transactions: rawRows,
		Truncated:    truncated,
	}, nil
}

// plainToTransactions converts a slice of wireTransaction (from the main or
// account-scoped endpoint), filtering out deleted rows.
func plainToTransactions(in []wireTransaction) []Transaction {
	out := make([]Transaction, 0, len(in))
	for _, t := range in {
		if t.Deleted {
			continue
		}
		out = append(out, toTransaction(t))
	}
	return out
}

// hybridToTransactions converts a slice of wireHybridTransaction (from the
// category- or payee-scoped endpoint), filtering out deleted rows and
// tagging flattened subtransaction lines.
func hybridToTransactions(in []wireHybridTransaction) []Transaction {
	out := make([]Transaction, 0, len(in))
	for _, t := range in {
		if t.Deleted {
			continue
		}
		out = append(out, toTransactionFromHybrid(t))
	}
	return out
}

func (c *Client) ListMonths(ctx context.Context, _ *mcp.CallToolRequest, in ListMonthsInput) (*mcp.CallToolResult, ListMonthsOutput, error) {
	if in.PlanID == "" {
		return nil, ListMonthsOutput{}, errors.New("plan_id is required")
	}
	limit := in.Limit
	switch {
	case limit <= 0:
		limit = 6
	case limit > 60:
		limit = 60
	}
	path := "/plans/" + url.PathEscape(in.PlanID) + "/months"
	var wire wireMonthSummariesResponse
	if err := c.doJSON(ctx, path, nil, &wire); err != nil {
		return nil, ListMonthsOutput{}, sanitizedErr(err)
	}
	// Filter deleted, sort descending by month (ISO dates sort lex).
	filtered := make([]wireMonthSummary, 0, len(wire.Data.Months))
	for _, m := range wire.Data.Months {
		if m.Deleted {
			continue
		}
		filtered = append(filtered, m)
	}
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].Month > filtered[j].Month
	})
	if len(filtered) > limit {
		filtered = filtered[:limit]
	}
	out := ListMonthsOutput{Months: make([]MonthSummary, 0, len(filtered))}
	for _, m := range filtered {
		out.Months = append(out.Months, toMonthSummary(m))
	}
	return nil, out, nil
}

func (c *Client) ListScheduledTransactions(ctx context.Context, _ *mcp.CallToolRequest, in ListScheduledTransactionsInput) (*mcp.CallToolResult, ListScheduledTransactionsOutput, error) {
	if in.PlanID == "" {
		return nil, ListScheduledTransactionsOutput{}, errors.New("plan_id is required")
	}
	if in.UpcomingDays < 0 {
		return nil, ListScheduledTransactionsOutput{}, errors.New("upcoming_days must be non-negative")
	}
	if in.UpcomingDays > 365 {
		return nil, ListScheduledTransactionsOutput{}, errors.New("upcoming_days must be <= 365")
	}
	path := "/plans/" + url.PathEscape(in.PlanID) + "/scheduled_transactions"
	var wire wireScheduledTransactionsResponse
	if err := c.doJSON(ctx, path, nil, &wire); err != nil {
		return nil, ListScheduledTransactionsOutput{}, sanitizedErr(err)
	}
	// Filter deleted. Optional upcoming_days filter is applied via lexical
	// ISO-date comparison because the YNAB date format (YYYY-MM-DD) sorts
	// correctly as a string.
	var cutoff string
	if in.UpcomingDays > 0 {
		cutoff = time.Now().UTC().AddDate(0, 0, in.UpcomingDays).Format("2006-01-02")
	}
	out := ListScheduledTransactionsOutput{
		ScheduledTransactions: make([]ScheduledTransaction, 0),
	}
	for _, s := range wire.Data.ScheduledTransactions {
		if s.Deleted {
			continue
		}
		if cutoff != "" && s.DateNext > cutoff {
			continue
		}
		out.ScheduledTransactions = append(out.ScheduledTransactions, toScheduledTransaction(s))
	}
	// Sort ascending by date_next so "soonest first" is the natural order.
	sort.Slice(out.ScheduledTransactions, func(i, j int) bool {
		return out.ScheduledTransactions[i].DateNext < out.ScheduledTransactions[j].DateNext
	})
	return nil, out, nil
}

func (c *Client) ListPayees(ctx context.Context, _ *mcp.CallToolRequest, in ListPayeesInput) (*mcp.CallToolResult, ListPayeesOutput, error) {
	if in.PlanID == "" {
		return nil, ListPayeesOutput{}, errors.New("plan_id is required")
	}
	path := "/plans/" + url.PathEscape(in.PlanID) + "/payees"
	var wire wirePayeesResponse
	if err := c.doJSON(ctx, path, nil, &wire); err != nil {
		return nil, ListPayeesOutput{}, sanitizedErr(err)
	}
	needle := strings.ToLower(in.NameContains)
	payees := make([]Payee, 0, len(wire.Data.Payees))
	for _, p := range wire.Data.Payees {
		if p.Deleted && !in.IncludeDeleted {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(p.Name), needle) {
			continue
		}
		payees = append(payees, toPayee(p))
	}
	return nil, ListPayeesOutput{Payees: payees}, nil
}

func (c *Client) ListCategories(ctx context.Context, _ *mcp.CallToolRequest, in ListCategoriesInput) (*mcp.CallToolResult, ListCategoriesOutput, error) {
	if in.PlanID == "" {
		return nil, ListCategoriesOutput{}, errors.New("plan_id is required")
	}
	path := "/plans/" + url.PathEscape(in.PlanID) + "/categories"
	var wire wireCategoriesResponse
	if err := c.doJSON(ctx, path, nil, &wire); err != nil {
		return nil, ListCategoriesOutput{}, sanitizedErr(err)
	}
	categories := make([]Category, 0)
	for _, g := range wire.Data.CategoryGroups {
		if g.Deleted {
			continue
		}
		for _, cat := range g.Categories {
			if cat.Deleted {
				continue
			}
			if cat.Hidden && !in.IncludeHidden {
				continue
			}
			if cat.CategoryGroupName == "" {
				cat.CategoryGroupName = g.Name
			}
			categories = append(categories, toCategory(cat))
		}
	}
	return nil, ListCategoriesOutput{Categories: categories}, nil
}

// ---- registration -----------------------------------------------------------

// registerTools wires all 5 read-only tools into the server. All tools are
// marked ReadOnlyHint=true so conformant MCP clients can surface them as safe
// operations that need no user confirmation.
func registerTools(server *mcp.Server, c *Client) {
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_plans",
		Title:       "List YNAB plans",
		Description: "List all YNAB plans (called 'budgets' in the YNAB UI) owned by the authenticated user. Returns plan ids and names for use with other tools.",
		Annotations: readOnly,
	}, c.ListPlans)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_month",
		Title:       "Get plan month",
		Description: "Get a plan month with per-category assigned/activity/balance amounts. Useful for 'how am I doing this month' questions.",
		Annotations: readOnly,
	}, c.GetMonth)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_accounts",
		Title:       "List accounts",
		Description: "List accounts in a YNAB plan with current balances. Closed accounts are excluded unless include_closed is set.",
		Annotations: readOnly,
	}, c.ListAccounts)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_transactions",
		Title:       "List transactions",
		Description: "List transactions for a plan, most recent first. Filter by date range (since_date), approval state (type), or scope (account_id OR category_id OR payee_id — pick one). Category/payee scoping flattens split transactions so each subtransaction line appears separately with is_subtransaction=true.",
		Annotations: readOnly,
	}, c.ListTransactions)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_categories",
		Title:       "List categories",
		Description: "List all categories in a plan with this month's assigned/activity/balance amounts and goal details.",
		Annotations: readOnly,
	}, c.ListCategories)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_months",
		Title:       "List months",
		Description: "List monthly rollup summaries (income, budgeted, activity, to_be_budgeted, age_of_money) for recent months, most recent first. Use for month-over-month trend questions. Use get_month for per-category detail within a single month.",
		Annotations: readOnly,
	}, c.ListMonths)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_scheduled_transactions",
		Title:       "List scheduled transactions",
		Description: "List recurring and future-dated scheduled transactions in date_next order (soonest first). Optionally limit to those scheduled within the next N days via upcoming_days.",
		Annotations: readOnly,
	}, c.ListScheduledTransactions)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_payees",
		Title:       "List payees",
		Description: "List all payees in a plan. Optional name_contains performs a case-insensitive substring match on payee names — use this to find specific payee IDs before calling list_transactions with payee_id or ynab_spending_check with excluded_payee_ids. Deleted payees are excluded by default.",
		Annotations: readOnly,
	}, c.ListPayees)

	// Write tools — registered ONLY when YNAB_ALLOW_WRITES=1 at startup.
	// When the environment variable is unset, these tools do not appear
	// in tools/list output and the LLM cannot call them at all. Every
	// handler also performs a per-call re-check as defense-in-depth.
	if writeAllowed() {
		destructive := false
		idempotent := true
		mutating := &mcp.ToolAnnotations{
			ReadOnlyHint:    false,
			DestructiveHint: &destructive, // creates new entities, doesn't destroy
			IdempotentHint:  idempotent,   // import_id-based dedup at YNAB
		}
		mcp.AddTool(server, &mcp.Tool{
			Name:        "create_transaction",
			Title:       "Create transaction",
			Description: "Create a new transaction in YNAB. Requires YNAB_ALLOW_WRITES=1. Asks the MCP client to confirm before executing. Amounts >$10K require an echo-back amount_override_milliunits acknowledgment. Provide an import_id to dedupe idempotently on retry.",
			Annotations: mutating,
		}, c.CreateTransaction)

		destructiveBudget := false
		mutatingBudget := &mcp.ToolAnnotations{
			ReadOnlyHint:    false,
			DestructiveHint: &destructiveBudget, // updates assigned amount, not destructive
			IdempotentHint:  true,               // same budgeted twice == same final state
		}
		mcp.AddTool(server, &mcp.Tool{
			Name:        "update_category_budgeted",
			Title:       "Update category assigned amount",
			Description: "Change the assigned (budgeted) amount on a single category for a single plan month. Primitive for Rule 3 money moves during the Sunday ritual. Requires YNAB_ALLOW_WRITES=1. Asks the MCP client to confirm before executing, showing the before/after delta. Returns before and after snapshots of budgeted and balance.",
			Annotations: mutatingBudget,
		}, c.UpdateCategoryBudgeted)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "update_transaction",
			Title:       "Update transaction",
			Description: "Update a partial subset of fields on an existing transaction: category, payee, memo, approved state, cleared state, flag color. Amount changes are NOT supported — users who need to change an amount should delete the transaction in the YNAB app and create a new one. At least one mutable field must be specified. Requires YNAB_ALLOW_WRITES=1 and asks the MCP client to confirm before executing.",
			Annotations: mutatingBudget,
		}, c.UpdateTransaction)

		// approve_transaction deliberately skips elicitation (see its doc
		// comment in tools_writes.go) to support batch pending-cleanup
		// workflows. Mark idempotent — approving an already-approved
		// transaction is a no-op at YNAB's end.
		mcp.AddTool(server, &mcp.Tool{
			Name:        "approve_transaction",
			Title:       "Approve transaction",
			Description: "Mark an existing transaction as approved. Convenience wrapper over update_transaction. Does NOT prompt for per-call confirmation (unlike other write tools) to support batch daily-cleanup workflows — the YNAB_ALLOW_WRITES=1 env var remains the primary defense. Returns before/after snapshot of the approved field.",
			Annotations: mutatingBudget,
		}, c.ApproveTransaction)
	}
}

// sanitizedErr runs a final sanitize pass on an error's string form before it
// leaves a tool handler. The primary guarantee is still that no code path in
// this package formats a token or Authorization header into an error — this
// is belt-and-braces.
func sanitizedErr(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(sanitize(err.Error()))
}
