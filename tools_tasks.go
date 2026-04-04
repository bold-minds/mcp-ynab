// SPDX-License-Identifier: MIT
//
// tools_tasks.go holds the task-shaped tool handlers: composition-level
// tools that take configuration as arguments and answer specific user
// questions in one call (instead of forcing the LLM to chain 3-5
// primitives). Per the v0.2 brief's stateless-MCP architecture, all
// user-specific config (APRs, priority tiers, etc.) is passed in via
// input arguments — the MCP reads no files the user owns.
//
// Currently in this file:
//   - ynab_debt_snapshot (pure simulation, no YNAB calls beyond account balances)
//
// Upcoming in later steps of the v0.2 build sequence:
//   - ynab_spending_check (step 8)
//   - ynab_waterfall_assignment (step 9)
//   - ynab_status (step 10)
//   - ynab_weekly_checkin (step 11)

package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ============================================================================
// ynab_debt_snapshot — avalanche payoff projection
// ============================================================================

// DebtAccountConfig is the per-account configuration the caller (skill)
// passes in. YNAB does not store APRs; the skill owns that data in its
// own config file and forwards it to us on every call.
type DebtAccountConfig struct {
	AccountID                string  `json:"account_id" jsonschema:"YNAB account id (UUID) for a debt-type account"`
	Nickname                 string  `json:"nickname,omitempty" jsonschema:"optional friendly name shown in output; falls back to YNAB account name"`
	APRPercent               float64 `json:"apr_percent" jsonschema:"annual percentage rate as a decimal percent (e.g. 27.15 for 27.15%)"`
	MinimumPaymentMilliunits int64   `json:"minimum_payment_milliunits" jsonschema:"minimum monthly payment in milliunits"`
}

// YnabDebtSnapshotInput asks the server to compute a debt snapshot and
// optional avalanche payoff projection.
type YnabDebtSnapshotInput struct {
	PlanID                 string              `json:"plan_id" jsonschema:"YNAB plan id, or 'last-used' / 'default'"`
	DebtAccountConfig      []DebtAccountConfig `json:"debt_account_config" jsonschema:"debt accounts to include, with APR and minimum payment. Skill owns this config."`
	ExtraPerMonthMilliunits int64              `json:"extra_per_month_milliunits,omitempty" jsonschema:"optional: additional monthly payment beyond minimums, applied to highest-APR debt first (avalanche method). Default 0."`
}

// DebtAccountSnapshot is the per-account view in the response.
type DebtAccountSnapshot struct {
	AccountID       string  `json:"account_id"`
	Nickname        string  `json:"nickname"`
	Balance         Money   `json:"balance"`
	APRPercent      float64 `json:"apr_percent"`
	MinimumPayment  Money   `json:"minimum_payment"`
	MonthlyInterest Money   `json:"monthly_interest"`
}

// DebtPayoffProjection describes one scenario's timeline.
type DebtPayoffProjection struct {
	MonthsToDebtFree int    `json:"months_to_debt_free"`
	TotalInterest    Money  `json:"total_interest_paid"`
	DebtFreeDate     string `json:"debt_free_date" jsonschema:"YYYY-MM of the projected debt-free month"`
	// PayoffOrder is present only when extra_per_month_milliunits > 0,
	// showing the order in which accounts hit zero under the avalanche.
	PayoffOrder []DebtPayoffMilestone `json:"payoff_order,omitempty"`
}

// DebtPayoffMilestone is one step in the avalanche payoff ordering.
type DebtPayoffMilestone struct {
	AccountID      string  `json:"account_id"`
	Nickname       string  `json:"nickname"`
	MonthPaidOff   int     `json:"month_paid_off"`
	BalanceAtStart Money   `json:"balance_at_start"`
	APRPercent     float64 `json:"apr_percent" jsonschema:"APR carried through so the LLM can narrate 'paid off in order of highest APR first'"`
}

// DebtSnapshotWarning identifies an individual account whose minimum
// payment is less than its monthly interest — meaning the account is
// growing month-over-month unless the avalanche eventually reaches it.
// Non-fatal: the projection still converges (avalanche pays down the
// account eventually), but the user should know.
type DebtSnapshotWarning struct {
	AccountID                string `json:"account_id"`
	Nickname                 string `json:"nickname"`
	MinimumPaymentMilliunits int64  `json:"minimum_payment_milliunits"`
	MonthlyInterestMilliunits int64 `json:"monthly_interest_milliunits"`
	ShortfallMilliunits      int64  `json:"shortfall_milliunits" jsonschema:"interest minus minimum; positive means the account is growing under minimum-only payment"`
}

// YnabDebtSnapshotOutput is the happy-path response.
type YnabDebtSnapshotOutput struct {
	AsOfDate             string                 `json:"as_of_date"`
	TotalBalance         Money                  `json:"total_balance"`
	TotalMonthlyInterest Money                  `json:"total_monthly_interest"`
	AnnualInterestCost   Money                  `json:"annual_interest_cost" jsonschema:"simple annualization of current monthly interest; not a simulation"`
	Accounts             []DebtAccountSnapshot  `json:"accounts"`
	ProjectionMinimumsOnly *DebtPayoffProjection `json:"projection_minimums_only,omitempty"`
	ProjectionWithExtra    *DebtPayoffProjection `json:"projection_with_extra,omitempty"`
	Warnings             []DebtSnapshotWarning  `json:"warnings,omitempty"`
}

// YnabDebtSnapshot computes current debt state and avalanche projections.
// Integer milliunit arithmetic throughout; no floats in the compounding
// loop. See the file-level comment for approximation bounds.
func (c *Client) YnabDebtSnapshot(ctx context.Context, _ *mcp.CallToolRequest, in YnabDebtSnapshotInput) (*mcp.CallToolResult, YnabDebtSnapshotOutput, error) {
	if in.PlanID == "" {
		return nil, YnabDebtSnapshotOutput{}, errors.New("plan_id is required")
	}
	if len(in.DebtAccountConfig) == 0 {
		return nil, YnabDebtSnapshotOutput{}, errors.New("debt_account_config must contain at least one account")
	}
	if in.ExtraPerMonthMilliunits < 0 {
		return nil, YnabDebtSnapshotOutput{}, errors.New("extra_per_month_milliunits must be non-negative")
	}

	// Validate APRs.
	for _, cfg := range in.DebtAccountConfig {
		if cfg.AccountID == "" {
			return nil, YnabDebtSnapshotOutput{}, errors.New("every debt_account_config entry must have account_id")
		}
		if cfg.APRPercent < 0 {
			return nil, YnabDebtSnapshotOutput{}, fmt.Errorf("apr_percent must be non-negative (account %s)", cfg.AccountID)
		}
		if cfg.APRPercent > 1000 {
			return nil, YnabDebtSnapshotOutput{}, fmt.Errorf("apr_percent %v looks wrong (account %s) — expected a percent like 27.15, not a fraction", cfg.APRPercent, cfg.AccountID)
		}
	}

	// Fetch live balances via list_accounts. Delta sync applies if
	// configured. We reuse the existing handler so we get all the
	// security guarantees (rate limiter, host lock, error scrubbing).
	_, accOut, err := c.ListAccounts(ctx, nil, ListAccountsInput{PlanID: in.PlanID, IncludeClosed: false})
	if err != nil {
		return nil, YnabDebtSnapshotOutput{}, sanitizedErr(err)
	}
	balanceByID := make(map[string]Account, len(accOut.Accounts))
	for _, a := range accOut.Accounts {
		balanceByID[a.ID] = a
	}

	// Build per-account snapshots with current state.
	snapshots := make([]DebtAccountSnapshot, 0, len(in.DebtAccountConfig))
	balances := make([]int64, 0, len(in.DebtAccountConfig))
	aprBps := make([]int64, 0, len(in.DebtAccountConfig))
	aprPercents := make([]float64, 0, len(in.DebtAccountConfig))
	minimums := make([]int64, 0, len(in.DebtAccountConfig))
	nicknames := make([]string, 0, len(in.DebtAccountConfig))
	ids := make([]string, 0, len(in.DebtAccountConfig))

	var warnings []DebtSnapshotWarning
	var totalBalanceMu, totalInterestMu int64

	for _, cfg := range in.DebtAccountConfig {
		acct, found := balanceByID[cfg.AccountID]
		if !found {
			return nil, YnabDebtSnapshotOutput{}, fmt.Errorf("account %s not found in YNAB plan", cfg.AccountID)
		}
		// YNAB debt account balances are stored as NEGATIVE milliunits
		// (a -$500 balance means you owe $500). Flip sign for simulation.
		owed := -acct.Balance.Milliunits
		if owed < 0 {
			owed = 0 // credit balance on a debt account; treat as zero owed
		}
		apr := aprToBasisPoints(cfg.APRPercent)
		interest := monthlyInterestMilliunits(owed, apr)

		nickname := cfg.Nickname
		if nickname == "" {
			nickname = acct.Name
		}

		snapshots = append(snapshots, DebtAccountSnapshot{
			AccountID:       cfg.AccountID,
			Nickname:        nickname,
			Balance:         NewMoney(owed),
			APRPercent:      cfg.APRPercent,
			MinimumPayment:  NewMoney(cfg.MinimumPaymentMilliunits),
			MonthlyInterest: NewMoney(interest),
		})

		balances = append(balances, owed)
		aprBps = append(aprBps, apr)
		aprPercents = append(aprPercents, cfg.APRPercent)
		minimums = append(minimums, cfg.MinimumPaymentMilliunits)
		nicknames = append(nicknames, nickname)
		ids = append(ids, cfg.AccountID)

		totalBalanceMu += owed
		totalInterestMu += interest

		// Warning: minimum payment less than monthly interest.
		if cfg.MinimumPaymentMilliunits < interest {
			warnings = append(warnings, DebtSnapshotWarning{
				AccountID:                 cfg.AccountID,
				Nickname:                  nickname,
				MinimumPaymentMilliunits:  cfg.MinimumPaymentMilliunits,
				MonthlyInterestMilliunits: interest,
				ShortfallMilliunits:       interest - cfg.MinimumPaymentMilliunits,
			})
		}
	}

	// Early error: if total minimums <= total interest, we have
	// aggregate negative amortization — debt cannot decrease without
	// additional payment. Return a structured error the LLM can render.
	var totalMinimumsMu int64
	for _, m := range minimums {
		totalMinimumsMu += m
	}
	if in.ExtraPerMonthMilliunits == 0 && totalMinimumsMu <= totalInterestMu {
		return nil, YnabDebtSnapshotOutput{}, fmt.Errorf(
			"negative_amortization: total minimum payments (%s) are less than or equal to total monthly interest (%s); "+
				"shortfall %s milliunits. Payments must exceed interest for debt to decrease — "+
				"either the APRs are wrong, minimums are wrong, or extra_per_month_milliunits must be provided",
			formatMilliunits(totalMinimumsMu),
			formatMilliunits(totalInterestMu),
			formatMilliunits(totalInterestMu-totalMinimumsMu+1),
		)
	}

	out := YnabDebtSnapshotOutput{
		AsOfDate:             time.Now().UTC().Format("2006-01-02"),
		TotalBalance:         NewMoney(totalBalanceMu),
		TotalMonthlyInterest: NewMoney(totalInterestMu),
		AnnualInterestCost:   NewMoney(totalInterestMu * 12),
		Accounts:             snapshots,
		Warnings:             warnings,
	}

	// Run simulations. Both scenarios use the same simulation function
	// with different extraPerMonth values.
	projMin, errMin := simulateAvalanche(balances, aprBps, minimums, aprPercents, ids, nicknames, 0)
	if errMin != nil {
		return nil, YnabDebtSnapshotOutput{}, fmt.Errorf("minimum-only projection: %w", errMin)
	}
	out.ProjectionMinimumsOnly = &projMin

	if in.ExtraPerMonthMilliunits > 0 {
		projExtra, errExtra := simulateAvalanche(balances, aprBps, minimums, aprPercents, ids, nicknames, in.ExtraPerMonthMilliunits)
		if errExtra != nil {
			return nil, YnabDebtSnapshotOutput{}, fmt.Errorf("with-extra projection: %w", errExtra)
		}
		out.ProjectionWithExtra = &projExtra
	}

	return nil, out, nil
}

// aprToBasisPoints converts a float APR percent (e.g. 27.15) to basis
// points (2715). The boundary conversion is the ONLY floating-point
// arithmetic in the debt snapshot path; the compounding loop is int64.
// Precision cap: ~0.01% — aprs with more than 2 decimal places round.
func aprToBasisPoints(apr float64) int64 {
	bps := math.Round(apr * 100)
	if bps < 0 {
		return 0
	}
	return int64(bps)
}

// monthlyInterestMilliunits returns the monthly interest on balance at
// the given basis-point APR, computed in integer arithmetic.
//
// Derivation:
//   monthly_rate = apr_percent / 100 / 12
//                = apr_bps / 10_000 / 12
//                = apr_bps / 120_000
//   interest     = balance * monthly_rate
//                = balance * apr_bps / 120_000
//
// Floor-division error: up to ~1 milliunit per account per month. The
// rounding direction is always "down" (never unbiased), and the error
// compounds through the simulation because each month's interest is
// computed on a balance that already reflects prior-month rounding.
// Cumulative bound for a 600-month × 10-account simulation: ≤6,000
// milliunits = ~$6 of under-reported total interest. Acceptable for a
// "months to debt free" projection — the error is always biased toward
// optimistic (slightly less interest than true), and in no case
// changes the integer number of months to payoff.
func monthlyInterestMilliunits(balanceMu, aprBps int64) int64 {
	if balanceMu <= 0 || aprBps == 0 {
		return 0
	}
	return balanceMu * aprBps / 120_000
}

// simulateAvalanche runs a month-by-month integer simulation using the
// avalanche method (pay minimums on all debts, put any extra on the
// highest-APR debt; when that's paid off, roll its minimum + any extra
// to the next-highest-APR debt).
//
// All input slices must be the same length and aligned by account index.
// aprPercents carries the display-friendly APR value through to the
// payoff milestone output so the LLM can narrate ordering by APR.
// The function does NOT mutate its input slices.
//
// Simulation is capped at 600 months (50 years). If the total payment
// capacity cannot cover the compounded interest (aggregate negative
// amortization), the function returns a "cannot converge" error — but
// callers should catch the simpler case at the entry point via the
// total_minimums <= total_interest check.
func simulateAvalanche(
	initialBalances, aprBps, minimums []int64,
	aprPercents []float64,
	ids, nicknames []string,
	extraPerMonth int64,
) (DebtPayoffProjection, error) {
	const maxMonths = 600

	n := len(initialBalances)
	balances := make([]int64, n)
	startingBalances := make([]int64, n)
	copy(balances, initialBalances)
	copy(startingBalances, initialBalances)
	paidOffMonth := make([]int, n)
	for i := range paidOffMonth {
		paidOffMonth[i] = 0
	}

	// Pre-sort account indices by APR descending for avalanche ordering.
	// This ordering never changes during the simulation; an account that
	// is paid off is skipped by the balance check.
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool {
		return aprBps[order[a]] > aprBps[order[b]]
	})

	var totalInterest int64
	allPaidOff := func() bool {
		for _, b := range balances {
			if b > 0 {
				return false
			}
		}
		return true
	}

	month := 0
	for ; month < maxMonths; month++ {
		if allPaidOff() {
			break
		}
		monthOneBased := month + 1

		// Step 1: accrue interest on all accounts.
		for i, b := range balances {
			if b <= 0 {
				continue
			}
			interest := monthlyInterestMilliunits(b, aprBps[i])
			balances[i] += interest
			totalInterest += interest
		}

		// Step 2: pay minimums on every unpaid account.
		remainingExtra := extraPerMonth
		for i, b := range balances {
			if b <= 0 {
				continue
			}
			pay := minimums[i]
			if pay > b {
				// Pay off the remainder, save the leftover for extra pot.
				leftover := pay - b
				balances[i] = 0
				paidOffMonth[i] = monthOneBased
				remainingExtra += leftover
				continue
			}
			balances[i] -= pay
		}

		// Step 3: apply extra payment to the highest-APR unpaid account,
		// rolling overflow to the next-highest, and so on (snowball the
		// avalanche).
		for _, idx := range order {
			if remainingExtra <= 0 {
				break
			}
			if balances[idx] <= 0 {
				// Already paid off; if it was paid off this month, its
				// leftover is already in remainingExtra.
				continue
			}
			if remainingExtra >= balances[idx] {
				remainingExtra -= balances[idx]
				balances[idx] = 0
				paidOffMonth[idx] = monthOneBased
			} else {
				balances[idx] -= remainingExtra
				remainingExtra = 0
			}
		}

		// Safety: if after this month's payments nothing changed, we
		// have aggregate negative amortization even with extra applied.
		// Break out with an error.
		if month > 0 && !progressedThisMonth(balances, initialBalances) && extraPerMonth == 0 {
			// This should have been caught by the entry-point check, but
			// defensive.
			break
		}
	}

	if !allPaidOff() {
		return DebtPayoffProjection{}, fmt.Errorf("simulation did not converge within %d months", maxMonths)
	}

	// Build the payoff-order list (avalanche sequence: by earliest
	// month paid off, ties broken by APR descending). APR is carried
	// through from the input so the tiebreak comparison actually uses
	// APR — fix for review finding B2.
	payoffMilestones := make([]DebtPayoffMilestone, 0, n)
	for i := range balances {
		if paidOffMonth[i] > 0 {
			payoffMilestones = append(payoffMilestones, DebtPayoffMilestone{
				AccountID:      ids[i],
				Nickname:       nicknames[i],
				MonthPaidOff:   paidOffMonth[i],
				BalanceAtStart: NewMoney(startingBalances[i]),
				APRPercent:     aprPercents[i],
			})
		}
	}
	sort.Slice(payoffMilestones, func(i, j int) bool {
		if payoffMilestones[i].MonthPaidOff != payoffMilestones[j].MonthPaidOff {
			return payoffMilestones[i].MonthPaidOff < payoffMilestones[j].MonthPaidOff
		}
		return payoffMilestones[i].APRPercent > payoffMilestones[j].APRPercent
	})

	debtFreeDate := time.Now().UTC().AddDate(0, month, 0).Format("2006-01")
	return DebtPayoffProjection{
		MonthsToDebtFree: month,
		TotalInterest:    NewMoney(totalInterest),
		DebtFreeDate:     debtFreeDate,
		PayoffOrder:      payoffMilestones,
	}, nil
}

// progressedThisMonth reports whether at least one account has a lower
// balance this month than it started with. Used as a last-ditch infinite
// loop guard.
func progressedThisMonth(current, initial []int64) bool {
	for i := range current {
		if current[i] < initial[i] {
			return true
		}
	}
	return false
}

// countUnapprovedExcludingTransferMirrors counts pending-cleanup items
// without double-counting a transfer between on-budget accounts. YNAB
// represents an on-budget-to-on-budget transfer as two mirrored
// transactions (one positive, one negative), each with transfer_account_id
// set. Each unapproved transfer should show up as ONE pending item in the
// user's "approve these" queue, not two.
//
// Implementation: for non-transfer rows, count them all. For transfer
// rows, count only the outflow side (Amount < 0). This is stable
// regardless of which side the user encounters first in the UI.
func countUnapprovedExcludingTransferMirrors(txns []Transaction) int {
	n := 0
	for _, t := range txns {
		if t.TransferAccountID == "" {
			n++
			continue
		}
		// Transfer row: count the outflow side only.
		if t.Amount.Milliunits < 0 {
			n++
		}
	}
	return n
}

// ============================================================================
// ynab_status — one-call dashboard snapshot for the Sunday ritual
// ============================================================================

// ynabStatusCreditCardPaymentGroupName is the hard-coded English name
// YNAB assigns to the auto-managed category group that holds credit
// card payment categories. YNAB's API does not expose a machine-readable
// "is credit card payment group" flag, so we match on this literal.
//
// See docs/ASSUMPTIONS.md — YNAB's API has been English-only for 15+
// years and this name has not changed, so the pragmatic choice is a
// string match with a documented assumption. If YNAB ever localizes,
// this is the single place to update.
const ynabStatusCreditCardPaymentGroupName = "Credit Card Payments"

// debtAccountTypes is the set of YNAB account types that represent
// liabilities (money the user owes). Used by both ynab_status and
// ynab_debt_snapshot to identify debt accounts. Lifted here so that
// any future YNAB type addition is a single-point edit across the
// package.
var debtAccountTypes = map[string]bool{
	"creditCard":   true,
	"lineOfCredit": true,
	"mortgage":     true,
	"autoLoan":     true,
	"studentLoan":  true,
	"personalLoan": true,
	"medicalDebt":  true,
	"otherDebt":    true,
}

// YnabStatusInput takes the debt account config as an optional argument
// so the status view can display APR and monthly interest for debt
// accounts when the skill has that data. If the config is omitted, debt
// accounts still appear in the list but without APR/interest fields.
type YnabStatusInput struct {
	PlanID            string              `json:"plan_id" jsonschema:"YNAB plan id, or 'last-used' / 'default'"`
	DebtAccountConfig []DebtAccountConfig `json:"debt_account_config,omitempty" jsonschema:"optional: debt accounts to enrich with APR and monthly interest. If omitted, debt accounts appear without APR/interest fields."`
}

// DebtStatusEntry is a debt account in the status output. APR and
// monthly interest are nullable — null when the caller did not supply
// debt_account_config.
type DebtStatusEntry struct {
	AccountID       string   `json:"account_id"`
	Nickname        string   `json:"nickname"`
	Balance         Money    `json:"balance"`
	APRPercent      *float64 `json:"apr_percent,omitempty"`
	MonthlyInterest *Money   `json:"monthly_interest,omitempty"`
	MinimumPayment  *Money   `json:"minimum_payment,omitempty"`
}

// OverspentCategoryEntry is a category with a negative balance, excluded
// credit card payment categories (per the Q3 decision in the v0.2 brief).
type OverspentCategoryEntry struct {
	CategoryID string `json:"category_id"`
	Name       string `json:"name"`
	GroupName  string `json:"group_name"`
	Overspend  Money  `json:"overspend" jsonschema:"positive milliunits, the absolute value of the negative balance"`
}

// DaysSinceLastReconciledEntry is one account's "how long since the user
// reconciled this account" datapoint. Null days means the account has
// never been reconciled.
type DaysSinceLastReconciledEntry struct {
	AccountID string `json:"account_id"`
	Name      string `json:"name"`
	Days      *int   `json:"days,omitempty" jsonschema:"days since last_reconciled_at; null if the account has never been reconciled"`
}

// ScheduledOutflowOccurrence is one expanded scheduled-transaction
// occurrence within the next-7-days window.
type ScheduledOutflowOccurrence struct {
	ScheduledID string `json:"scheduled_id"`
	PayeeName   string `json:"payee_name,omitempty"`
	Date        string `json:"date" jsonschema:"YYYY-MM-DD of this specific occurrence"`
	Amount      Money  `json:"amount" jsonschema:"signed amount; negative for outflows"`
}

// ScheduledNext7Days is the aggregated next-7-days scheduled transaction
// view, expanded via the recurrence iterator so daily and weekly schedules
// are correctly counted multiple times.
type ScheduledNext7Days struct {
	TotalOutflow Money                        `json:"total_outflow" jsonschema:"sum of the absolute values of negative occurrences (money going out)"`
	TotalInflow  Money                        `json:"total_inflow" jsonschema:"sum of positive occurrences (money coming in)"`
	Occurrences  []ScheduledOutflowOccurrence `json:"occurrences"`
}

// YnabStatusOutput is the dashboard response.
type YnabStatusOutput struct {
	ReadyToAssign       Money                    `json:"ready_to_assign" jsonschema:"current plan's to_be_budgeted amount"`
	OverspentCategories []OverspentCategoryEntry `json:"overspent_categories"`
	// CreditCardPaymentCategoriesExcludedCount tells the LLM whether any
	// categories were filtered out of overspent_categories because they
	// are auto-managed credit card payment categories. Allows honest
	// reporting per the Q3 decision in the v0.2 brief.
	CreditCardPaymentCategoriesExcludedCount int                            `json:"credit_card_payment_categories_excluded_count"`
	DebtAccounts                             []DebtStatusEntry              `json:"debt_accounts"`
	SavingsAccounts                          []Account                      `json:"savings_accounts"`
	DaysSinceLastReconciled                  []DaysSinceLastReconciledEntry `json:"days_since_last_reconciled"`
	UnapprovedTransactionCount               int                            `json:"unapproved_transaction_count"`
	ScheduledNext7Days                       ScheduledNext7Days             `json:"scheduled_next_7_days"`
	// Truncated is true when the unapproved-transaction count exceeded
	// the internal aggregation safety ceiling. The count in that case is
	// a lower bound.
	Truncated bool `json:"truncated,omitempty" jsonschema:"true when unapproved_transaction_count hit the internal 50000-row safety ceiling; the count is a lower bound in that case"`
}

// YnabStatus returns a Sunday-ritual dashboard in one call, composing
// list_accounts + list_categories + list_transactions(type=unapproved) +
// list_scheduled_transactions + get_month(current).
func (c *Client) YnabStatus(ctx context.Context, _ *mcp.CallToolRequest, in YnabStatusInput) (*mcp.CallToolResult, YnabStatusOutput, error) {
	if in.PlanID == "" {
		return nil, YnabStatusOutput{}, errors.New("plan_id is required")
	}

	// 1. Current month for ready_to_assign.
	_, monthOut, err := c.GetMonth(ctx, nil, GetMonthInput{PlanID: in.PlanID, Month: "current"})
	if err != nil {
		return nil, YnabStatusOutput{}, sanitizedErr(err)
	}

	// 2. Accounts — for debt balances, savings_accounts, and
	//    days_since_last_reconciled. Use list_accounts with include_closed
	//    off (we only care about live accounts).
	_, accOut, err := c.ListAccounts(ctx, nil, ListAccountsInput{PlanID: in.PlanID, IncludeClosed: false})
	if err != nil {
		return nil, YnabStatusOutput{}, sanitizedErr(err)
	}

	// 3. Categories for overspent detection. list_categories already
	//    includes group_name on each category.
	_, catOut, err := c.ListCategories(ctx, nil, ListCategoriesInput{PlanID: in.PlanID})
	if err != nil {
		return nil, YnabStatusOutput{}, sanitizedErr(err)
	}

	// 4. Unapproved transaction count — aggregation path so we get the
	// FULL count, not a 500-row trim. truncated is surfaced in the output.
	// Transfers between on-budget accounts appear as two unapproved rows;
	// count one side only (the outflow) so a single unapproved transfer
	// counts as 1 pending item, not 2.
	unapprovedRows, unapprovedTruncated, err := c.fetchTransactionsForAggregation(ctx, txnFetchOpts{
		planID:  in.PlanID,
		txnType: "unapproved",
	})
	if err != nil {
		return nil, YnabStatusOutput{}, sanitizedErr(err)
	}
	unapprovedCount := countUnapprovedExcludingTransferMirrors(unapprovedRows)

	// 5. Scheduled transactions for next 7 days window.
	_, schedOut, err := c.ListScheduledTransactions(ctx, nil, ListScheduledTransactionsInput{
		PlanID: in.PlanID,
	})
	if err != nil {
		return nil, YnabStatusOutput{}, sanitizedErr(err)
	}

	// Compose the output from the fetched data.
	out := YnabStatusOutput{
		ReadyToAssign:              monthOut.ToBeBudgeted,
		UnapprovedTransactionCount: unapprovedCount,
		Truncated:                  unapprovedTruncated,
	}

	// Overspent categories: balance < 0 AND not in the Credit Card
	// Payments auto-managed group. See docs/ASSUMPTIONS.md for the
	// English-name match rationale.
	var ccExcluded int
	for _, cat := range catOut.Categories {
		if cat.Balance.Milliunits >= 0 {
			continue
		}
		if cat.GroupName == ynabStatusCreditCardPaymentGroupName {
			ccExcluded++
			continue
		}
		out.OverspentCategories = append(out.OverspentCategories, OverspentCategoryEntry{
			CategoryID: cat.ID,
			Name:       cat.Name,
			GroupName:  cat.GroupName,
			Overspend:  NewMoney(-cat.Balance.Milliunits),
		})
	}
	out.CreditCardPaymentCategoriesExcludedCount = ccExcluded

	// Debt accounts: YNAB's debt account types per the spec. Uses the
	// package-level debtAccountTypes set shared with ynab_debt_snapshot.
	debtCfgByID := make(map[string]DebtAccountConfig, len(in.DebtAccountConfig))
	for _, cfg := range in.DebtAccountConfig {
		debtCfgByID[cfg.AccountID] = cfg
	}

	// Accounts pass: classify debt and savings.
	for _, a := range accOut.Accounts {
		if debtAccountTypes[a.Type] {
			// YNAB stores debt as negative milliunits (owed). Flip sign
			// for display. Clamp to 0 for credit-balance debt accounts
			// (where the user paid more than they owe, creating a
			// positive credit) — review finding B4. YnabDebtSnapshot
			// already does this; YnabStatus must match.
			owed := -a.Balance.Milliunits
			if owed < 0 {
				owed = 0
			}
			entry := DebtStatusEntry{
				AccountID: a.ID,
				Nickname:  a.Name,
				Balance:   NewMoney(owed),
			}
			if cfg, ok := debtCfgByID[a.ID]; ok {
				if cfg.Nickname != "" {
					entry.Nickname = cfg.Nickname
				}
				aprCopy := cfg.APRPercent
				entry.APRPercent = &aprCopy
				minCopy := NewMoney(cfg.MinimumPaymentMilliunits)
				entry.MinimumPayment = &minCopy
				// Use the clamped owed amount for interest — no interest
				// on a credit balance.
				interest := monthlyInterestMilliunits(owed, aprToBasisPoints(cfg.APRPercent))
				if interest < 0 {
					interest = 0
				}
				interestMoney := NewMoney(interest)
				entry.MonthlyInterest = &interestMoney
			}
			out.DebtAccounts = append(out.DebtAccounts, entry)
		}
		if a.Type == "savings" {
			out.SavingsAccounts = append(out.SavingsAccounts, a)
		}
	}

	// Days since last reconciled. list_accounts uses the output Account
	// type which doesn't include last_reconciled_at — need to read it
	// from the wire via a second call or extend Account. Extend Account
	// to carry the field.
	//
	// For now, compute via a fresh list_accounts call that we re-parse
	// via a direct doJSON path. Simpler: extend the Account output type
	// to include last_reconciled_at (done in this commit).
	today := time.Now().UTC()
	for _, a := range accOut.Accounts {
		entry := DaysSinceLastReconciledEntry{
			AccountID: a.ID,
			Name:      a.Name,
		}
		if a.LastReconciledAt != nil {
			days := int(today.Sub(*a.LastReconciledAt).Hours() / 24)
			if days < 0 {
				days = 0
			}
			entry.Days = &days
		}
		out.DaysSinceLastReconciled = append(out.DaysSinceLastReconciled, entry)
	}

	// Scheduled next 7 days: expand recurrences via the iterator.
	windowStart := dateOnly(today)
	windowEnd := windowStart.AddDate(0, 0, 7)
	var totalOutflowMu, totalInflowMu int64
	for _, s := range schedOut.ScheduledTransactions {
		parsedDate, err := time.Parse("2006-01-02", s.DateNext)
		if err != nil {
			continue // skip malformed date
		}
		occurrences := FrequencyOccurrences(parsedDate, s.Frequency, windowStart, windowEnd)
		for _, occ := range occurrences {
			if s.Amount.Milliunits < 0 {
				totalOutflowMu += -s.Amount.Milliunits
			} else {
				totalInflowMu += s.Amount.Milliunits
			}
			out.ScheduledNext7Days.Occurrences = append(out.ScheduledNext7Days.Occurrences, ScheduledOutflowOccurrence{
				ScheduledID: s.ID,
				PayeeName:   s.PayeeName,
				Date:        occ.Format("2006-01-02"),
				Amount:      s.Amount,
			})
		}
	}
	out.ScheduledNext7Days.TotalOutflow = NewMoney(totalOutflowMu)
	out.ScheduledNext7Days.TotalInflow = NewMoney(totalInflowMu)
	// Sort occurrences by date ascending for predictable output.
	sort.Slice(out.ScheduledNext7Days.Occurrences, func(i, j int) bool {
		return out.ScheduledNext7Days.Occurrences[i].Date < out.ScheduledNext7Days.Occurrences[j].Date
	})

	return nil, out, nil
}

// ============================================================================
// ynab_weekly_checkin — 7-day period math vs prior 7 days
// ============================================================================

// YnabWeeklyCheckinInput asks for a week-over-week comparison ending at
// as_of_date (defaults to today). The tool returns income, outflow,
// unapproved, and newly-overspent categories for the current 7-day
// window plus the prior 7-day window.
type YnabWeeklyCheckinInput struct {
	PlanID    string `json:"plan_id" jsonschema:"YNAB plan id, or 'last-used' / 'default'"`
	AsOfDate  string `json:"as_of_date,omitempty" jsonschema:"end of the period to compare (YYYY-MM-DD); defaults to today (UTC)"`
}

// PeriodRange is a [start, end] inclusive date range in ISO format.
type PeriodRange struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// WeeklyMoneyCompare pairs a metric across two adjacent weeks with the
// delta. All three values use the same sign convention.
type WeeklyMoneyCompare struct {
	ThisPeriod  Money `json:"this_period"`
	PriorPeriod Money `json:"prior_period"`
	Delta       Money `json:"delta"`
}

// CategoryOverspendEntry is a category that became overspent in the
// current period (or was resolved from overspent). Name is included
// for display.
type CategoryOverspendEntry struct {
	CategoryID string `json:"category_id"`
	Name       string `json:"name"`
	Overspend  Money  `json:"overspend,omitempty"`
}

// YnabWeeklyCheckinOutput is the ritual response.
type YnabWeeklyCheckinOutput struct {
	Period         PeriodRange        `json:"period"`
	PriorPeriod    PeriodRange        `json:"prior_period"`
	IncomeReceived WeeklyMoneyCompare `json:"income_received" jsonschema:"positive inflows categorized to Ready-to-Assign in the period"`
	TotalOutflows  WeeklyMoneyCompare `json:"total_outflows" jsonschema:"negative amounts (spent) in the period; reported as positive magnitudes in the three fields"`

	// Month-granular fields — see period_grouping_note.
	CategoriesNewlyOverspentThisMonth []CategoryOverspendEntry `json:"categories_newly_overspent_this_month" jsonschema:"Month-over-month comparison (NOT week-over-week). YNAB's API only exposes category balances at monthly granularity via MonthDetail, so this field uses month scope while other fields in this response use week scope."`
	CategoriesResolvedFromOverspent   []CategoryOverspendEntry `json:"categories_resolved_from_overspent_this_month"`

	UnapprovedCount     int  `json:"unapproved_count" jsonschema:"current total across the plan, not period-scoped"`
	AgeOfMoneyDeltaDays *int `json:"age_of_money_delta_days,omitempty" jsonschema:"month-over-month change in age_of_money; null if not computable"`

	// PeriodGroupingNote is a redundant explanation so the LLM cannot
	// miss the month/week scope mismatch when rendering a summary.
	PeriodGroupingNote string `json:"period_grouping_note"`

	// Truncated is true when the period-scope transaction fetch or the
	// unapproved count fetch hit the internal 50K safety ceiling. The
	// numeric fields are lower bounds in that case.
	Truncated bool `json:"truncated,omitempty" jsonschema:"true when at least one underlying transaction fetch exceeded the 50000-row safety ceiling; weekly deltas and unapproved_count are lower bounds when true"`
}

// YnabWeeklyCheckin composes multiple reads to build a week-over-week
// narrative plus a month-over-month overspent comparison.
func (c *Client) YnabWeeklyCheckin(ctx context.Context, _ *mcp.CallToolRequest, in YnabWeeklyCheckinInput) (*mcp.CallToolResult, YnabWeeklyCheckinOutput, error) {
	if in.PlanID == "" {
		return nil, YnabWeeklyCheckinOutput{}, errors.New("plan_id is required")
	}

	asOf := time.Now().UTC()
	if in.AsOfDate != "" {
		parsed, err := time.Parse("2006-01-02", in.AsOfDate)
		if err != nil {
			return nil, YnabWeeklyCheckinOutput{}, errors.New("as_of_date must be YYYY-MM-DD")
		}
		asOf = parsed
	}

	// Define the two 7-day windows. Current = [asOf-6, asOf]; prior =
	// [asOf-13, asOf-7]. Both inclusive on both ends.
	thisEnd := asOf
	thisStart := asOf.AddDate(0, 0, -6)
	priorEnd := asOf.AddDate(0, 0, -7)
	priorStart := asOf.AddDate(0, 0, -13)

	thisStartStr := thisStart.Format("2006-01-02")
	thisEndStr := thisEnd.Format("2006-01-02")
	priorStartStr := priorStart.Format("2006-01-02")
	priorEndStr := priorEnd.Format("2006-01-02")

	out := YnabWeeklyCheckinOutput{
		Period:      PeriodRange{Start: thisStartStr, End: thisEndStr},
		PriorPeriod: PeriodRange{Start: priorStartStr, End: priorEndStr},
		PeriodGroupingNote: "categories_newly_overspent_this_month compares current month to prior month " +
			"(YNAB's API does not expose category balances at day granularity); all other fields are week-granular.",
	}

	// Fetch transactions from priorStart onward in a single call via the
	// aggregation path so the 500-row LLM-context trim doesn't silently
	// drop older transactions. This is critical: the prior week is
	// "older" than this week, so a 500-row trim on the base list would
	// evict the prior week first and leave every prior-period number
	// wrong without any indication.
	txns, periodTruncated, err := c.fetchTransactionsForAggregation(ctx, txnFetchOpts{
		planID:    in.PlanID,
		sinceDate: priorStartStr,
	})
	if err != nil {
		return nil, YnabWeeklyCheckinOutput{}, sanitizedErr(err)
	}
	if periodTruncated {
		out.Truncated = true
	}

	// Transfers between on-budget accounts appear as two mirrored rows
	// (one positive, one negative) and would double-inflate both the
	// inflow and outflow totals if we summed them naively. YNAB marks
	// both sides with transfer_account_id != nil (the account_id the
	// transfer moves TO); filter these rows out so weekly income/outflow
	// reflect real money in/out of the budget.
	var thisInflow, priorInflow int64
	var thisOutflow, priorOutflow int64
	for _, t := range txns {
		if t.Date < priorStartStr || t.Date > thisEndStr {
			continue
		}
		if t.TransferAccountID != "" {
			continue // see transfer-exclusion comment above
		}
		inThis := t.Date >= thisStartStr && t.Date <= thisEndStr
		inPrior := t.Date >= priorStartStr && t.Date <= priorEndStr
		amt := t.Amount.Milliunits
		if inThis {
			if amt > 0 {
				thisInflow += amt
			} else {
				thisOutflow += -amt
			}
		} else if inPrior {
			if amt > 0 {
				priorInflow += amt
			} else {
				priorOutflow += -amt
			}
		}
	}

	out.IncomeReceived = WeeklyMoneyCompare{
		ThisPeriod:  NewMoney(thisInflow),
		PriorPeriod: NewMoney(priorInflow),
		Delta:       NewMoney(thisInflow - priorInflow),
	}
	out.TotalOutflows = WeeklyMoneyCompare{
		ThisPeriod:  NewMoney(thisOutflow),
		PriorPeriod: NewMoney(priorOutflow),
		Delta:       NewMoney(thisOutflow - priorOutflow),
	}

	// Unapproved count across the plan (not period-scoped). Aggregation
	// path so we count the full set, not a 500-row trim. Transfers between
	// on-budget accounts appear as two unapproved rows; count one side
	// only (the outflow) to avoid double-counting.
	unapprovedRows, unapprovedTruncated, err := c.fetchTransactionsForAggregation(ctx, txnFetchOpts{
		planID:  in.PlanID,
		txnType: "unapproved",
	})
	if err != nil {
		return nil, YnabWeeklyCheckinOutput{}, sanitizedErr(err)
	}
	if unapprovedTruncated {
		out.Truncated = true
	}
	out.UnapprovedCount = countUnapprovedExcludingTransferMirrors(unapprovedRows)

	// Month-over-month newly-overspent comparison. Fetch current and
	// prior month, compare category balance signs.
	// YNAB months are always the first of the month in ISO format
	// (YYYY-MM-01). Build explicitly via fmt because "2006-01-01" as a
	// Go time format would be ambiguous (01 is the month reference).
	y, m, _ := asOf.Date()
	currentMonthDate := fmt.Sprintf("%04d-%02d-01", y, int(m))
	prior := asOf.AddDate(0, -1, 0)
	py, pm, _ := prior.Date()
	priorMonthDate := fmt.Sprintf("%04d-%02d-01", py, int(pm))

	_, curMonthOut, err := c.GetMonth(ctx, nil, GetMonthInput{PlanID: in.PlanID, Month: currentMonthDate})
	if err != nil {
		return nil, YnabWeeklyCheckinOutput{}, sanitizedErr(err)
	}
	_, priorMonthOut, err := c.GetMonth(ctx, nil, GetMonthInput{PlanID: in.PlanID, Month: priorMonthDate})
	if err != nil {
		return nil, YnabWeeklyCheckinOutput{}, sanitizedErr(err)
	}
	priorOverspentByID := make(map[string]bool)
	for _, cat := range priorMonthOut.Categories {
		if cat.Balance.Milliunits < 0 && cat.GroupName != ynabStatusCreditCardPaymentGroupName {
			priorOverspentByID[cat.ID] = true
		}
	}
	for _, cat := range curMonthOut.Categories {
		if cat.GroupName == ynabStatusCreditCardPaymentGroupName {
			continue
		}
		currentlyOverspent := cat.Balance.Milliunits < 0
		wasOverspentLastMonth := priorOverspentByID[cat.ID]
		switch {
		case currentlyOverspent && !wasOverspentLastMonth:
			out.CategoriesNewlyOverspentThisMonth = append(out.CategoriesNewlyOverspentThisMonth, CategoryOverspendEntry{
				CategoryID: cat.ID,
				Name:       cat.Name,
				Overspend:  NewMoney(-cat.Balance.Milliunits),
			})
		case !currentlyOverspent && wasOverspentLastMonth:
			out.CategoriesResolvedFromOverspent = append(out.CategoriesResolvedFromOverspent, CategoryOverspendEntry{
				CategoryID: cat.ID,
				Name:       cat.Name,
			})
		}
	}

	// age_of_money delta (month-granular; explicitly null if either
	// month's value isn't available).
	if curMonthOut.AgeOfMoney != 0 && priorMonthOut.AgeOfMoney != 0 {
		delta := curMonthOut.AgeOfMoney - priorMonthOut.AgeOfMoney
		out.AgeOfMoneyDeltaDays = &delta
	}

	return nil, out, nil
}

// ============================================================================
// ynab_spending_check — did the user stay on plan in a date range?
// ============================================================================

// YnabSpendingCheckInput describes a spending check against a budget.
// The eating-plan audit shape: category set + date range + budget amount,
// with an optional "except these payees" escape hatch (e.g. exclude
// Chipotle on date nights).
type YnabSpendingCheckInput struct {
	PlanID           string   `json:"plan_id" jsonschema:"YNAB plan id, or 'last-used' / 'default'"`
	CategoryIDs      []string `json:"category_ids" jsonschema:"one or more category ids to include in the spending total. Pass a single category or a group (Groceries + Restaurants + Coffee) — the tool sums across all of them."`
	StartDate        string   `json:"start_date" jsonschema:"inclusive start of the date range (YYYY-MM-DD)"`
	EndDate          string   `json:"end_date" jsonschema:"inclusive end of the date range (YYYY-MM-DD)"`
	BudgetMilliunits int64    `json:"budget_milliunits" jsonschema:"spending limit for the date range in milliunits (positive)"`
	ExcludedPayeeIDs []string `json:"excluded_payee_ids,omitempty" jsonschema:"optional list of payee ids to exclude from the sum (e.g. 'Chipotle' on date nights). Use list_payees with name_contains to find payee ids."`
}

// YnabSpendingCheckOutput reports the result of the spending check.
//
// OnPlan is a nullable boolean: present when the tool could aggregate the
// full matching transaction set, absent when the aggregation hit the
// internal safety ceiling (Truncated=true). In the truncated case,
// VerdictUnavailableReason explains why the tool refuses to give a
// verdict on incomplete data.
type YnabSpendingCheckOutput struct {
	OnPlan                   *bool         `json:"on_plan,omitempty" jsonschema:"present only when the answer is reliable: true if actual <= budget, false if actual > budget. Absent when Truncated=true."`
	ActualMilliunits         int64         `json:"actual_milliunits" jsonschema:"net spending in the scope (positive = net outflow, negative = net inflow from refunds)"`
	BudgetMilliunits         int64         `json:"budget_milliunits"`
	DeltaMilliunits          int64         `json:"delta_milliunits" jsonschema:"actual - budget. Positive = over budget, negative = under budget. May be unreliable when Truncated=true."`
	TransactionCount         int           `json:"transaction_count"`
	OffendingTransactions    []Transaction `json:"offending_transactions,omitempty" jsonschema:"populated only when actual > budget AND not truncated: the transactions that contributed to the overspend, sorted by amount descending (biggest contributors first)"`
	Truncated                bool          `json:"truncated,omitempty" jsonschema:"true when at least one category fetch exceeded the internal 50000-row safety ceiling. When true, OnPlan is absent and numeric fields are lower bounds."`
	VerdictUnavailableReason string        `json:"verdict_unavailable_reason,omitempty" jsonschema:"populated only when Truncated=true; explains why on_plan cannot be given"`
}

// YnabSpendingCheck aggregates spending across one or more categories
// over a date range and compares to a budget. Returns an on_plan boolean
// for "yes/no" LLM answers, plus the full offending-transactions list
// when over budget so the LLM can narrate which transactions pushed
// the user over.
//
// Implementation: calls the shared fetchTransactionsForAggregation helper
// once per category_id with the category scope filter. The category and
// payee scoped YNAB endpoints return HybridTransactions which flatten
// split transactions to subtransaction rows — each row has a unique id
// so de-duping by Transaction.ID across category scopes is correct.
//
// If any per-category fetch exceeds aggregationCeiling (50K rows), the
// output sets Truncated=true, drops OnPlan entirely (the tool refuses to
// give a verdict on incomplete data), and sets VerdictUnavailableReason
// for the LLM to relay.
func (c *Client) YnabSpendingCheck(ctx context.Context, _ *mcp.CallToolRequest, in YnabSpendingCheckInput) (*mcp.CallToolResult, YnabSpendingCheckOutput, error) {
	if in.PlanID == "" {
		return nil, YnabSpendingCheckOutput{}, errors.New("plan_id is required")
	}
	if len(in.CategoryIDs) == 0 {
		return nil, YnabSpendingCheckOutput{}, errors.New("category_ids must contain at least one category")
	}
	if in.StartDate == "" || in.EndDate == "" {
		return nil, YnabSpendingCheckOutput{}, errors.New("start_date and end_date are required (YYYY-MM-DD)")
	}
	if _, err := time.Parse("2006-01-02", in.StartDate); err != nil {
		return nil, YnabSpendingCheckOutput{}, errors.New("start_date must be YYYY-MM-DD")
	}
	if _, err := time.Parse("2006-01-02", in.EndDate); err != nil {
		return nil, YnabSpendingCheckOutput{}, errors.New("end_date must be YYYY-MM-DD")
	}
	if in.EndDate < in.StartDate { // ISO lex-sortable
		return nil, YnabSpendingCheckOutput{}, errors.New("end_date must be on or after start_date")
	}
	if in.BudgetMilliunits < 0 {
		return nil, YnabSpendingCheckOutput{}, errors.New("budget_milliunits must be non-negative")
	}

	excluded := make(map[string]struct{}, len(in.ExcludedPayeeIDs))
	for _, id := range in.ExcludedPayeeIDs {
		excluded[id] = struct{}{}
	}

	// Collect transactions across all requested categories via the
	// aggregation helper (NOT the user-facing ListTransactions, which
	// truncates at 500 rows for LLM-context reasons). De-dupe by
	// transaction id — a split transaction may contribute rows under
	// multiple requested categories but each hybrid row has a unique id.
	seen := make(map[string]Transaction)
	var anyTruncated bool
	for _, catID := range in.CategoryIDs {
		rows, truncated, err := c.fetchTransactionsForAggregation(ctx, txnFetchOpts{
			planID:     in.PlanID,
			sinceDate:  in.StartDate,
			categoryID: catID,
		})
		if err != nil {
			return nil, YnabSpendingCheckOutput{}, sanitizedErr(err)
		}
		if truncated {
			anyTruncated = true
		}
		for _, t := range rows {
			// Date range filter: fetchTransactions applies since_date
			// but not end_date, so we filter end_date here.
			if t.Date > in.EndDate {
				continue
			}
			if t.Date < in.StartDate {
				continue // defensive; since_date should have handled this
			}
			// Payee exclusion.
			if _, ex := excluded[payeeIDForTransaction(t)]; ex {
				continue
			}
			seen[t.ID] = t
		}
	}

	// Compute net outflow = -sum(amount) over the filtered set. YNAB
	// stores outflows as negative milliunits so negating gives us a
	// positive "spent" number when outflows dominate.
	var netOutflowMu int64
	txns := make([]Transaction, 0, len(seen))
	for _, t := range seen {
		netOutflowMu -= t.Amount.Milliunits
		txns = append(txns, t)
	}

	delta := netOutflowMu - in.BudgetMilliunits
	out := YnabSpendingCheckOutput{
		ActualMilliunits: netOutflowMu,
		BudgetMilliunits: in.BudgetMilliunits,
		DeltaMilliunits:  delta,
		TransactionCount: len(txns),
	}

	if anyTruncated {
		out.Truncated = true
		out.VerdictUnavailableReason = fmt.Sprintf(
			"at least one category fetch exceeded the internal safety ceiling of %d transactions; "+
				"refusing to give an on_plan verdict on incomplete data. Narrow the date range with start_date "+
				"or query fewer categories at a time.",
			aggregationCeiling,
		)
	} else {
		onPlan := delta <= 0
		out.OnPlan = &onPlan
		// Only surface offending transactions when over budget and we
		// have the complete set. Sort biggest absolute amount first so
		// the LLM can narrate "the Whole Foods $145 and Costco $200
		// trips put you over".
		if !onPlan {
			sort.Slice(txns, func(i, j int) bool {
				ai := txns[i].Amount.Milliunits
				if ai < 0 {
					ai = -ai
				}
				aj := txns[j].Amount.Milliunits
				if aj < 0 {
					aj = -aj
				}
				return ai > aj
			})
			out.OffendingTransactions = txns
		}
	}
	return nil, out, nil
}

// payeeIDForTransaction returns the payee id for a Transaction output
// type. Our Transaction struct doesn't carry payee_id directly (only
// payee_name for display), so the excluded_payee_ids feature currently
// has no way to match on the output shape. Track this as a known
// limitation: the feature works at the WIRE level but our Transaction
// type strips payee_id for leanness. Returning empty string means no
// exclusion match; the LLM workaround is to use list_payees to resolve
// names and pass IDs, but the match happens against the empty string
// which never matches.
//
// RESOLUTION: we need payee_id on the Transaction output type. This is
// fixed in the same commit — see the types.go change adding PayeeID to
// Transaction and the corresponding update in toTransaction.
func payeeIDForTransaction(t Transaction) string {
	return t.PayeeID
}

// ============================================================================
// ynab_waterfall_assignment — advisory priority allocation (no writes)
// ============================================================================

// WaterfallTier is one level of the user's assignment priority waterfall.
// The skill computes `need_milliunits` per category from its own config
// (goal targets, user rules, last month's budgeted, etc.) and passes
// the result in — the MCP performs pure allocation math with no
// inference about what "need" means.
type WaterfallTier struct {
	Name            string               `json:"name" jsonschema:"tier name for display, e.g. 'Immediate Obligations'"`
	StopIfUnfunded  bool                 `json:"stop_if_unfunded,omitempty" jsonschema:"if true, the waterfall stops allocating to subsequent tiers when this tier's needs cannot be fully funded. Default false: underfunded tiers are partially allocated and the waterfall continues."`
	Categories      []WaterfallCategory  `json:"categories" jsonschema:"the categories in this tier with their individual need amounts"`
}

// WaterfallCategory is one category in a tier, with the amount the
// skill says this category needs allocated in the current cycle.
type WaterfallCategory struct {
	CategoryID     string `json:"category_id" jsonschema:"YNAB category id (UUID)"`
	NeedMilliunits int64  `json:"need_milliunits" jsonschema:"amount the skill says this category needs allocated in this waterfall pass (must be non-negative)"`
}

// YnabWaterfallAssignmentInput asks the MCP to walk a priority waterfall
// and propose per-category allocations given an incoming amount. This
// tool issues NO writes — it is purely advisory. The LLM presents the
// output to the user; if approved, the LLM then issues
// update_category_budgeted calls.
type YnabWaterfallAssignmentInput struct {
	PlanID                  string          `json:"plan_id" jsonschema:"YNAB plan id, or 'last-used' / 'default'"`
	IncomingAmountMilliunits int64          `json:"incoming_amount_milliunits" jsonschema:"amount to allocate across the waterfall (milliunits, non-negative)"`
	PriorityTiers           []WaterfallTier `json:"priority_tiers" jsonschema:"tiers in priority order: the first tier is funded first, then the second, etc."`
	Month                   string          `json:"month,omitempty" jsonschema:"month to read current budgeted/balance from for the output. ISO month (YYYY-MM-01) or 'current'. Defaults to 'current'."`
}

// WaterfallAllocation is one proposed per-category assignment in the
// output. new_budgeted is the value the skill should pass to
// update_category_budgeted if the user approves.
type WaterfallAllocation struct {
	TierName             string `json:"tier_name"`
	CategoryID           string `json:"category_id"`
	CategoryName         string `json:"category_name"`
	CurrentBudgeted      Money  `json:"current_budgeted"`
	CurrentBalance       Money  `json:"current_balance"`
	AdditionalAssignment Money  `json:"additional_assignment" jsonschema:"the increment to add to current_budgeted"`
	NewBudgeted          Money  `json:"new_budgeted" jsonschema:"current_budgeted + additional_assignment; pass this to update_category_budgeted"`
}

// WaterfallTierSummary is a per-tier rollup of needs and allocations.
type WaterfallTierSummary struct {
	TierName  string `json:"tier_name"`
	Needed    Money  `json:"needed"`
	Allocated Money  `json:"allocated"`
	ShortBy   Money  `json:"short_by" jsonschema:"needed - allocated; zero when the tier was fully funded"`
}

// YnabWaterfallAssignmentOutput is the proposed plan for the skill/LLM
// to present and (optionally) execute.
type YnabWaterfallAssignmentOutput struct {
	Incoming             Money                  `json:"incoming"`
	ProposedAllocations  []WaterfallAllocation  `json:"proposed_allocations"`
	TierSummary          []WaterfallTierSummary `json:"tier_summary"`
	Remainder            Money                  `json:"remainder" jsonschema:"amount left over after the waterfall completed; the skill decides whether to park it in Ready-to-Assign or on a further tier"`
}

// YnabWaterfallAssignment walks the waterfall and returns proposed
// allocations. Zero writes to YNAB; the skill calls update_category_budgeted
// separately if the user approves.
func (c *Client) YnabWaterfallAssignment(ctx context.Context, _ *mcp.CallToolRequest, in YnabWaterfallAssignmentInput) (*mcp.CallToolResult, YnabWaterfallAssignmentOutput, error) {
	if in.PlanID == "" {
		return nil, YnabWaterfallAssignmentOutput{}, errors.New("plan_id is required")
	}
	if in.IncomingAmountMilliunits < 0 {
		return nil, YnabWaterfallAssignmentOutput{}, errors.New("incoming_amount_milliunits must be non-negative")
	}
	if len(in.PriorityTiers) == 0 {
		return nil, YnabWaterfallAssignmentOutput{}, errors.New("priority_tiers must contain at least one tier")
	}
	for i, tier := range in.PriorityTiers {
		if tier.Name == "" {
			return nil, YnabWaterfallAssignmentOutput{}, fmt.Errorf("priority_tiers[%d].name is required", i)
		}
		if len(tier.Categories) == 0 {
			return nil, YnabWaterfallAssignmentOutput{}, fmt.Errorf("priority_tiers[%d] (%q) must contain at least one category", i, tier.Name)
		}
		for j, cat := range tier.Categories {
			if cat.CategoryID == "" {
				return nil, YnabWaterfallAssignmentOutput{}, fmt.Errorf("priority_tiers[%d].categories[%d].category_id is required", i, j)
			}
			if cat.NeedMilliunits < 0 {
				return nil, YnabWaterfallAssignmentOutput{}, fmt.Errorf("priority_tiers[%d].categories[%d].need_milliunits must be non-negative", i, j)
			}
		}
	}

	// Fetch current category state so the output carries current_budgeted
	// and current_balance for display. We use the month-specific category
	// list via get_month, which returns Category values with current-month
	// budgeted and balance.
	month := in.Month
	if month == "" {
		month = "current"
	}
	_, monthOut, err := c.GetMonth(ctx, nil, GetMonthInput{PlanID: in.PlanID, Month: month})
	if err != nil {
		return nil, YnabWaterfallAssignmentOutput{}, sanitizedErr(err)
	}
	byID := make(map[string]Category, len(monthOut.Categories))
	for _, c := range monthOut.Categories {
		byID[c.ID] = c
	}

	// Walk the waterfall. For each tier in order, iterate its categories
	// and allocate min(need, remaining). Track tier totals. Respect
	// stop_if_unfunded.
	remaining := in.IncomingAmountMilliunits
	allocations := make([]WaterfallAllocation, 0)
	tierSummaries := make([]WaterfallTierSummary, 0, len(in.PriorityTiers))

	stopAllocatingRemainingTiers := false
	for _, tier := range in.PriorityTiers {
		var tierNeeded, tierAllocated int64
		for _, cat := range tier.Categories {
			tierNeeded += cat.NeedMilliunits
			if stopAllocatingRemainingTiers {
				// Subsequent tiers get a record with zero allocation so
				// the LLM can see what was skipped, but no money is moved.
				current, ok := byID[cat.CategoryID]
				if !ok {
					current = Category{ID: cat.CategoryID}
				}
				allocations = append(allocations, WaterfallAllocation{
					TierName:             tier.Name,
					CategoryID:           cat.CategoryID,
					CategoryName:         current.Name,
					CurrentBudgeted:      current.Budgeted,
					CurrentBalance:       current.Balance,
					AdditionalAssignment: NewMoney(0),
					NewBudgeted:          current.Budgeted,
				})
				continue
			}
			alloc := cat.NeedMilliunits
			if alloc > remaining {
				alloc = remaining
			}
			if alloc < 0 {
				alloc = 0
			}
			tierAllocated += alloc
			remaining -= alloc

			current, ok := byID[cat.CategoryID]
			if !ok {
				// Category not found in current month. Still record the
				// allocation with zero current values; the skill should
				// log the mismatch.
				current = Category{ID: cat.CategoryID}
			}
			newBudgetedMu := current.Budgeted.Milliunits + alloc
			allocations = append(allocations, WaterfallAllocation{
				TierName:             tier.Name,
				CategoryID:           cat.CategoryID,
				CategoryName:         current.Name,
				CurrentBudgeted:      current.Budgeted,
				CurrentBalance:       current.Balance,
				AdditionalAssignment: NewMoney(alloc),
				NewBudgeted:          NewMoney(newBudgetedMu),
			})
		}
		tierSummaries = append(tierSummaries, WaterfallTierSummary{
			TierName:  tier.Name,
			Needed:    NewMoney(tierNeeded),
			Allocated: NewMoney(tierAllocated),
			ShortBy:   NewMoney(tierNeeded - tierAllocated),
		})
		// stop_if_unfunded: halt allocating to subsequent tiers if this
		// tier could not be fully funded.
		if tier.StopIfUnfunded && tierAllocated < tierNeeded {
			stopAllocatingRemainingTiers = true
		}
	}

	return nil, YnabWaterfallAssignmentOutput{
		Incoming:            NewMoney(in.IncomingAmountMilliunits),
		ProposedAllocations: allocations,
		TierSummary:         tierSummaries,
		Remainder:           NewMoney(remaining),
	}, nil
}
