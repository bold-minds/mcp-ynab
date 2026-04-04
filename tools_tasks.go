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
	AccountID      string `json:"account_id"`
	Nickname       string `json:"nickname"`
	MonthPaidOff   int    `json:"month_paid_off"`
	BalanceAtStart Money  `json:"balance_at_start"`
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
	projMin, errMin := simulateAvalanche(balances, aprBps, minimums, ids, nicknames, 0)
	if errMin != nil {
		return nil, YnabDebtSnapshotOutput{}, fmt.Errorf("minimum-only projection: %w", errMin)
	}
	out.ProjectionMinimumsOnly = &projMin

	if in.ExtraPerMonthMilliunits > 0 {
		projExtra, errExtra := simulateAvalanche(balances, aprBps, minimums, ids, nicknames, in.ExtraPerMonthMilliunits)
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
// Floor-division error: at most 1 milliunit per call. Over a 60-month
// × 10-account simulation that's ≤600 milliunits = $0.60, acceptable
// for a "months to debt free" projection.
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
// The function does NOT mutate its input slices.
//
// Simulation is capped at 600 months (50 years). If the total payment
// capacity cannot cover the compounded interest (aggregate negative
// amortization), the function returns a "cannot converge" error — but
// callers should catch the simpler case at the entry point via the
// total_minimums <= total_interest check.
func simulateAvalanche(
	initialBalances, aprBps, minimums []int64,
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
	// month paid off, ties broken by APR descending).
	payoffMilestones := make([]DebtPayoffMilestone, 0, n)
	for i := range balances {
		if paidOffMonth[i] > 0 {
			payoffMilestones = append(payoffMilestones, DebtPayoffMilestone{
				AccountID:      ids[i],
				Nickname:       nicknames[i],
				MonthPaidOff:   paidOffMonth[i],
				BalanceAtStart: NewMoney(startingBalances[i]),
			})
		}
	}
	sort.Slice(payoffMilestones, func(i, j int) bool {
		if payoffMilestones[i].MonthPaidOff != payoffMilestones[j].MonthPaidOff {
			return payoffMilestones[i].MonthPaidOff < payoffMilestones[j].MonthPaidOff
		}
		// Tiebreak: higher APR first (more important to highlight).
		return payoffMilestones[i].BalanceAtStart.Milliunits > payoffMilestones[j].BalanceAtStart.Milliunits
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
