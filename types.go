// SPDX-License-Identifier: MIT
//
// This file defines two layers of types:
//
//   1. Wire types (lowercase, unexported) that mirror YNAB's JSON response
//      shapes as documented at https://api.ynab.com/ — these are what we
//      decode responses into. Monetary fields are int64 milliunits.
//   2. Output types (exported) that are returned to MCP clients. These are
//      deliberately trimmed to the fields an LLM is likely to reason about.
//      Monetary fields are Money (int64 + pre-formatted decimal string) —
//      float64 is never used for currency in this codebase per the security
//      brief.
//
// When in doubt, prefer NOT exposing a field. New fields can be added later;
// removing them is a breaking change.

package main

import "time"

// ============================================================================
// Wire types — internal, match YNAB JSON response shapes.
// ============================================================================

type wirePlanSummaryResponse struct {
	Data struct {
		Plans []wirePlan `json:"plans"`
	} `json:"data"`
}

type wirePlan struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	LastModifiedOn *time.Time `json:"last_modified_on,omitempty"`
	FirstMonth     string     `json:"first_month,omitempty"`
	LastMonth      string     `json:"last_month,omitempty"`
}

type wireAccountsResponse struct {
	Data struct {
		Accounts []wireAccount `json:"accounts"`
	} `json:"data"`
}

type wireAccount struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	Type             string  `json:"type"`
	OnBudget         bool    `json:"on_budget"`
	Closed           bool    `json:"closed"`
	Note             *string `json:"note"`
	Balance          int64   `json:"balance"`
	ClearedBalance   int64   `json:"cleared_balance"`
	UnclearedBalance int64   `json:"uncleared_balance"`
	Deleted          bool    `json:"deleted"`
}

type wireCategoriesResponse struct {
	Data struct {
		CategoryGroups []wireCategoryGroupWithCategories `json:"category_groups"`
	} `json:"data"`
}

type wireCategoryGroupWithCategories struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Hidden     bool           `json:"hidden"`
	Deleted    bool           `json:"deleted"`
	Categories []wireCategory `json:"categories"`
}

type wireCategory struct {
	ID                string  `json:"id"`
	CategoryGroupID   string  `json:"category_group_id"`
	CategoryGroupName string  `json:"category_group_name"`
	Name              string  `json:"name"`
	Hidden            bool    `json:"hidden"`
	Note              *string `json:"note"`
	Budgeted          int64   `json:"budgeted"`
	Activity          int64   `json:"activity"`
	Balance           int64   `json:"balance"`
	GoalType          *string `json:"goal_type"`
	GoalTarget        *int64  `json:"goal_target"`
	GoalTargetDate    *string `json:"goal_target_date"`
	GoalPctComplete   *int    `json:"goal_percentage_complete"`
	Deleted           bool    `json:"deleted"`
}

type wireTransactionsResponse struct {
	Data struct {
		Transactions []wireTransaction `json:"transactions"`
	} `json:"data"`
}

type wireTransaction struct {
	ID           string  `json:"id"`
	Date         string  `json:"date"`
	Amount       int64   `json:"amount"`
	Memo         *string `json:"memo"`
	Cleared      string  `json:"cleared"`
	Approved     bool    `json:"approved"`
	FlagColor    *string `json:"flag_color"`
	AccountID    string  `json:"account_id"`
	AccountName  string  `json:"account_name"`
	PayeeID      *string `json:"payee_id"`
	PayeeName    *string `json:"payee_name"`
	CategoryID   *string `json:"category_id"`
	CategoryName *string `json:"category_name"`
	Deleted      bool    `json:"deleted"`
}

type wireMonthDetailResponse struct {
	Data struct {
		Month wireMonthDetail `json:"month"`
	} `json:"data"`
}

type wireMonthDetail struct {
	Month        string         `json:"month"`
	Note         *string        `json:"note"`
	Income       int64          `json:"income"`
	Budgeted     int64          `json:"budgeted"`
	Activity     int64          `json:"activity"`
	ToBeBudgeted int64          `json:"to_be_budgeted"`
	AgeOfMoney   *int           `json:"age_of_money"`
	Categories   []wireCategory `json:"categories"`
}

// wireHybridTransactionsResponse is returned by the category-scoped and
// payee-scoped transaction endpoints. It differs from wireTransactionsResponse
// in that split transactions are flattened to subtransaction rows so the
// scope-filter works correctly.
type wireHybridTransactionsResponse struct {
	Data struct {
		Transactions []wireHybridTransaction `json:"transactions"`
	} `json:"data"`
}

// wireHybridTransaction extends wireTransaction with two fields that
// identify flattened subtransaction rows.
type wireHybridTransaction struct {
	wireTransaction
	Type                string  `json:"type"` // "transaction" | "subtransaction"
	ParentTransactionID *string `json:"parent_transaction_id"`
}

type wireMonthSummariesResponse struct {
	Data struct {
		Months []wireMonthSummary `json:"months"`
	} `json:"data"`
}

type wireMonthSummary struct {
	Month        string  `json:"month"`
	Note         *string `json:"note"`
	Income       int64   `json:"income"`
	Budgeted     int64   `json:"budgeted"`
	Activity     int64   `json:"activity"`
	ToBeBudgeted int64   `json:"to_be_budgeted"`
	AgeOfMoney   *int    `json:"age_of_money"`
	Deleted      bool    `json:"deleted"`
}

type wireScheduledTransactionsResponse struct {
	Data struct {
		ScheduledTransactions []wireScheduledTransaction `json:"scheduled_transactions"`
	} `json:"data"`
}

type wireScheduledTransaction struct {
	ID           string  `json:"id"`
	DateFirst    string  `json:"date_first"`
	DateNext     string  `json:"date_next"`
	Frequency    string  `json:"frequency"`
	Amount       int64   `json:"amount"`
	Memo         *string `json:"memo"`
	FlagColor    *string `json:"flag_color"`
	AccountID    string  `json:"account_id"`
	AccountName  string  `json:"account_name"`
	PayeeID      *string `json:"payee_id"`
	PayeeName    *string `json:"payee_name"`
	CategoryID   *string `json:"category_id"`
	CategoryName *string `json:"category_name"`
	Deleted      bool    `json:"deleted"`
}

// ============================================================================
// Output types — exposed to MCP clients. All currency is Money (no float).
// ============================================================================

// Plan is the summary of a YNAB plan (formerly called a "budget" in the
// YNAB UI and older API versions).
type Plan struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	LastModifiedOn *time.Time `json:"last_modified_on,omitempty" jsonschema:"last time the plan was changed"`
	FirstMonth     string     `json:"first_month,omitempty" jsonschema:"earliest month covered by this plan (YYYY-MM-01)"`
	LastMonth      string     `json:"last_month,omitempty" jsonschema:"latest month covered by this plan (YYYY-MM-01)"`
}

// Account is a YNAB account within a plan.
type Account struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Type             string `json:"type" jsonschema:"checking|savings|cash|creditCard|...|mortgage"`
	OnBudget         bool   `json:"on_budget"`
	Closed           bool   `json:"closed"`
	Note             string `json:"note,omitempty"`
	Balance          Money  `json:"balance"`
	ClearedBalance   Money  `json:"cleared_balance"`
	UnclearedBalance Money  `json:"uncleared_balance"`
}

// Category is a YNAB category with amounts for its current plan month.
type Category struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	GroupName      string `json:"group_name"`
	Hidden         bool   `json:"hidden,omitempty"`
	Note           string `json:"note,omitempty"`
	Budgeted       Money  `json:"budgeted" jsonschema:"assigned amount for the month"`
	Activity       Money  `json:"activity" jsonschema:"spending/income in the category this month"`
	Balance        Money  `json:"balance" jsonschema:"available balance remaining"`
	GoalType       string `json:"goal_type,omitempty" jsonschema:"TB|TBD|MF|NEED|DEBT when a goal is set"`
	GoalTarget     *Money `json:"goal_target,omitempty"`
	GoalTargetDate string `json:"goal_target_date,omitempty"`
	GoalPercent    int    `json:"goal_percent_complete,omitempty"`
}

// Transaction is a YNAB transaction as returned by list_transactions.
//
// When list_transactions is filtered by category_id or payee_id, split
// transactions are flattened so each category line appears as its own
// Transaction with IsSubtransaction=true. In that case, Amount is the
// subtransaction amount (not the parent total) and CategoryName is the
// subtransaction's own category. When filtering by account or with no
// filter, split transactions appear as a single Transaction with
// IsSubtransaction=false.
type Transaction struct {
	ID               string `json:"id"`
	Date             string `json:"date" jsonschema:"ISO date (YYYY-MM-DD)"`
	Amount           Money  `json:"amount" jsonschema:"signed amount; negative for outflows"`
	Memo             string `json:"memo,omitempty"`
	Cleared          string `json:"cleared" jsonschema:"cleared|uncleared|reconciled"`
	Approved         bool   `json:"approved"`
	FlagColor        string `json:"flag_color,omitempty"`
	AccountID        string `json:"account_id"`
	AccountName      string `json:"account_name"`
	PayeeName        string `json:"payee_name,omitempty"`
	CategoryName     string `json:"category_name,omitempty"`
	IsSubtransaction bool   `json:"is_subtransaction,omitempty" jsonschema:"true when this row is a flattened split-transaction line from a category or payee filter"`
}

// MonthSummary is a lean per-month rollup used by list_months. It intentionally
// omits the per-category breakdown — use get_month when you need that level.
type MonthSummary struct {
	Month        string `json:"month" jsonschema:"the month in YYYY-MM-01 format"`
	Note         string `json:"note,omitempty"`
	Income       Money  `json:"income"`
	Budgeted     Money  `json:"budgeted"`
	Activity     Money  `json:"activity"`
	ToBeBudgeted Money  `json:"to_be_budgeted"`
	AgeOfMoney   int    `json:"age_of_money,omitempty"`
}

// ScheduledTransaction is a recurring / future-dated transaction as returned
// by list_scheduled_transactions.
type ScheduledTransaction struct {
	ID           string `json:"id"`
	DateFirst    string `json:"date_first" jsonschema:"ISO date of the first scheduled occurrence"`
	DateNext     string `json:"date_next" jsonschema:"ISO date of the next scheduled occurrence"`
	Frequency    string `json:"frequency" jsonschema:"never|daily|weekly|everyOtherWeek|twiceAMonth|every4Weeks|monthly|everyOtherMonth|every3Months|every4Months|twiceAYear|yearly|everyOtherYear"`
	Amount       Money  `json:"amount"`
	Memo         string `json:"memo,omitempty"`
	FlagColor    string `json:"flag_color,omitempty"`
	AccountID    string `json:"account_id"`
	AccountName  string `json:"account_name"`
	PayeeName    string `json:"payee_name,omitempty"`
	CategoryName string `json:"category_name,omitempty"`
}

// Month is the detail view of a plan month.
type Month struct {
	Month        string     `json:"month" jsonschema:"the month in YYYY-MM-01 format"`
	Note         string     `json:"note,omitempty"`
	Income       Money      `json:"income" jsonschema:"total Ready-to-Assign inflows for the month"`
	Budgeted     Money      `json:"budgeted" jsonschema:"total amount assigned in the month"`
	Activity     Money      `json:"activity" jsonschema:"total transaction activity excluding Ready-to-Assign inflows"`
	ToBeBudgeted Money      `json:"to_be_budgeted" jsonschema:"amount still available for Ready-to-Assign"`
	AgeOfMoney   int        `json:"age_of_money,omitempty"`
	Categories   []Category `json:"categories"`
}

// ============================================================================
// Wire → output conversions.
// ============================================================================

// toPlan converts a wire plan into the exported Plan type. The explicit
// field list is intentional — keeping wire and output types decoupled means
// a future divergence (e.g. formatting a derived field) does not silently
// break callers. Do not replace with Plan(w).
func toPlan(w wirePlan) Plan {
	//lint:ignore S1016 explicit conversion is a deliberate wire/output decoupling
	return Plan{
		ID:             w.ID,
		Name:           w.Name,
		LastModifiedOn: w.LastModifiedOn,
		FirstMonth:     w.FirstMonth,
		LastMonth:      w.LastMonth,
	}
}

func toAccount(w wireAccount) Account {
	return Account{
		ID:               w.ID,
		Name:             w.Name,
		Type:             w.Type,
		OnBudget:         w.OnBudget,
		Closed:           w.Closed,
		Note:             deref(w.Note),
		Balance:          NewMoney(w.Balance),
		ClearedBalance:   NewMoney(w.ClearedBalance),
		UnclearedBalance: NewMoney(w.UnclearedBalance),
	}
}

func toCategory(w wireCategory) Category {
	out := Category{
		ID:        w.ID,
		Name:      w.Name,
		GroupName: w.CategoryGroupName,
		Hidden:    w.Hidden,
		Note:      deref(w.Note),
		Budgeted:  NewMoney(w.Budgeted),
		Activity:  NewMoney(w.Activity),
		Balance:   NewMoney(w.Balance),
	}
	if w.GoalType != nil {
		out.GoalType = *w.GoalType
	}
	if w.GoalTarget != nil {
		m := NewMoney(*w.GoalTarget)
		out.GoalTarget = &m
	}
	if w.GoalTargetDate != nil {
		out.GoalTargetDate = *w.GoalTargetDate
	}
	if w.GoalPctComplete != nil {
		out.GoalPercent = *w.GoalPctComplete
	}
	return out
}

func toTransaction(w wireTransaction) Transaction {
	return Transaction{
		ID:           w.ID,
		Date:         w.Date,
		Amount:       NewMoney(w.Amount),
		Memo:         deref(w.Memo),
		Cleared:      w.Cleared,
		Approved:     w.Approved,
		FlagColor:    deref(w.FlagColor),
		AccountID:    w.AccountID,
		AccountName:  w.AccountName,
		PayeeName:    deref(w.PayeeName),
		CategoryName: deref(w.CategoryName),
	}
}

// toTransactionFromHybrid converts a wireHybridTransaction (returned by the
// category- and payee-scoped endpoints) into a Transaction. When the hybrid
// row is a flattened subtransaction line, IsSubtransaction is set so the
// LLM can distinguish split lines from whole transactions.
func toTransactionFromHybrid(w wireHybridTransaction) Transaction {
	t := toTransaction(w.wireTransaction)
	if w.Type == "subtransaction" {
		t.IsSubtransaction = true
	}
	return t
}

func toMonthSummary(w wireMonthSummary) MonthSummary {
	out := MonthSummary{
		Month:        w.Month,
		Note:         deref(w.Note),
		Income:       NewMoney(w.Income),
		Budgeted:     NewMoney(w.Budgeted),
		Activity:     NewMoney(w.Activity),
		ToBeBudgeted: NewMoney(w.ToBeBudgeted),
	}
	if w.AgeOfMoney != nil {
		out.AgeOfMoney = *w.AgeOfMoney
	}
	return out
}

func toScheduledTransaction(w wireScheduledTransaction) ScheduledTransaction {
	return ScheduledTransaction{
		ID:           w.ID,
		DateFirst:    w.DateFirst,
		DateNext:     w.DateNext,
		Frequency:    w.Frequency,
		Amount:       NewMoney(w.Amount),
		Memo:         deref(w.Memo),
		FlagColor:    deref(w.FlagColor),
		AccountID:    w.AccountID,
		AccountName:  w.AccountName,
		PayeeName:    deref(w.PayeeName),
		CategoryName: deref(w.CategoryName),
	}
}

func toMonth(w wireMonthDetail) Month {
	cats := make([]Category, 0, len(w.Categories))
	for _, c := range w.Categories {
		if c.Deleted {
			continue
		}
		cats = append(cats, toCategory(c))
	}
	out := Month{
		Month:        w.Month,
		Note:         deref(w.Note),
		Income:       NewMoney(w.Income),
		Budgeted:     NewMoney(w.Budgeted),
		Activity:     NewMoney(w.Activity),
		ToBeBudgeted: NewMoney(w.ToBeBudgeted),
		Categories:   cats,
	}
	if w.AgeOfMoney != nil {
		out.AgeOfMoney = *w.AgeOfMoney
	}
	return out
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
