# Assumptions

This document lists the non-obvious assumptions `mcp-ynab` makes about the YNAB API and its data conventions. Each entry identifies where in the code the assumption is relied on so a future maintainer can find and update the relevant check when an assumption is broken.

If any of these stops holding, the corresponding feature may misbehave silently. Each entry also describes the failure mode.

## YNAB API is English-only

**Assumption:** YNAB's API returns all field values and category group names in English only. It does not localize responses based on user locale, Accept-Language headers, or any other per-account setting.

**Where relied on:**
- `tools_tasks.go:ynabStatusCreditCardPaymentGroupName = "Credit Card Payments"` — the exact English name of YNAB's auto-managed credit card payment category group. Used by both `ynab_status` and `ynab_weekly_checkin` to exclude credit card payment categories from the "overspent" counts. Credit card payment categories legitimately go negative mid-month as you spend on the card, so counting them as overspent would produce noise.

**Failure mode if broken:** If YNAB ever localizes the "Credit Card Payments" group name (e.g., to "Paiements de carte de crédit" for a French plan), our string match will fail. Credit card payment categories would then appear in `overspent_categories` arrays, inflating the count and confusing the LLM summary.

**Detection:** None automated. A user of a non-English-plan would notice credit card payment categories showing up in the overspent list. The `credit_card_payment_categories_excluded_count` field on `ynab_status` output would also go to zero unexpectedly.

**Why this matters less than it might:** YNAB has been English-only for 15+ years. There is no current signal that localization is planned. The pragmatic choice is a string match with this documented assumption rather than the more complex alternative of cross-referencing category groups to credit card account types at query time. If YNAB ever localizes, this is the single place to update.

**Resolution path:** Add a cross-reference between `Category.category_group_id` and the plan's credit-card-type accounts: a group is a "credit card payment group" iff at least one credit-card-type account has a category named identically. This is more robust but more code; ship if/when needed.

---

## YNAB debt account balances are always negative

**Assumption:** For accounts with types in the debt set (`creditCard`, `lineOfCredit`, `mortgage`, `autoLoan`, `studentLoan`, `personalLoan`, `medicalDebt`, `otherDebt`), the `balance` field is negative when the user owes money on the account.

**Where relied on:**
- `tools_tasks.go:YnabDebtSnapshot` — negates `balance.Milliunits` to produce a positive "amount owed" number for simulation input.
- `tools_tasks.go:YnabStatus` — same sign flip for debt account display.

**Failure mode if broken:** If a debt account has a credit balance (you paid more than you owed, creating a positive credit), the negated value would be negative and the debt snapshot would clamp it to zero. This is already handled: `if owed < 0 { owed = 0 }`.

**Detection:** None automated. Users with credit balances on a debt account would see zero balance in the snapshot, which is probably the correct behavior for "amount owed" purposes.

---

## YNAB `twiceAMonth` frequency does not expose user's anchor days

**Assumption:** YNAB's `ScheduledTransactionSummary.frequency: twiceAMonth` value means "fires on two specific days each month" (e.g., the 1st and 15th, or 5th and 20th). The YNAB API does not expose which two days the user chose when creating the scheduled transaction. Only `date_first` and `date_next` are available.

**Where relied on:**
- `recurrence.go:advanceByFrequency` for `twiceAMonth` — approximated as a 15-day advance. For 7-day windows (the primary use case in `ynab_status.scheduled_next_7_days`) this under-counts by at most ~1 occurrence per window.

**Failure mode if broken:** In `ynab_status.scheduled_next_7_days`, a user with a `twiceAMonth` scheduled transaction whose two days happen to fall within the 7-day window would see one occurrence instead of two, understating their upcoming cash flow by the scheduled amount once per affected window.

**Detection:** None automated. The under-count is small (one per affected window) and only affects the cash-flow total, not the occurrence list visibility.

**Resolution path:** Use both `date_first` and `date_next` to compute the two anchor days (`date_next - date_first` modulo month length gives the offset), then iterate two days per month within the window. ~30 LoC, deferred to v0.3 unless a user reports the issue.

---

## YNAB scheduled-transaction date fields are date-only

**Assumption:** `ScheduledTransactionSummary.date_next` is an ISO date (`YYYY-MM-DD`) with no time component. YNAB models scheduled transactions at day granularity.

**Where relied on:**
- `recurrence.go:FrequencyOccurrences` — normalizes inputs via `dateOnly()` which zeroes time components. If YNAB ever adds hour-granularity scheduling, the occurrence math would need re-examination.

**Failure mode if broken:** Sub-day scheduling precision is currently invisible to the recurrence iterator. Hourly schedules would be treated as daily.

---

## YNAB monthly PATCH only updates `budgeted`

**Assumption:** `PATCH /plans/{id}/months/{month}/categories/{category_id}` ignores any field in the request body other than `budgeted`. YNAB explicitly documents this in the OpenAPI spec: "Only `budgeted` (assigned) amount can be updated and any other fields specified will be ignored."

**Where relied on:**
- `tools_writes.go:UpdateCategoryBudgeted` — sends only `{category: {budgeted: int64}}` in the request body and trusts YNAB to leave other fields alone.

**Failure mode if broken:** If YNAB ever stops ignoring extra fields, our body would cause a validation error — which is a fail-loud failure, not a silent corruption. We would see 400 responses and know to update the handler.

---

## Memo length limit: client-side 200 vs YNAB 500

**Assumption:** We impose a stricter memo length limit (200 characters) than YNAB's API maximum (500 characters).

**Where relied on:**
- `tools_writes.go:CreateTransaction` and `tools_writes.go:UpdateTransaction` — reject input memos longer than 200 characters.

**Rationale:** The v0.2 brief specifies 200 as a deliberate limit. Shorter memos encourage focused annotation and prevent the LLM from dumping large context into memo fields. YNAB's 500 limit is the backstop.

**Failure mode if changed:** Users can still set memos up to 500 characters via the YNAB app; we just don't let our tool create/update with more than 200. Trivially fixable: bump the constant in `tools_writes.go`.

---

## Amount safety cap: 10 million milliunits

**Assumption:** Write-tool amount arguments exceeding `10_000_000` milliunits (= $10,000 USD) require an `amount_override_milliunits` echo-back equal to the original amount.

**Where relied on:**
- `writes.go:checkAmountBound` — universal cap, currency-agnostic.

**Failure mode:** For plans in currencies with very different subunit scales (e.g., JPY where 1 million milliunits = ¥1,000), the cap maps to a different "dollar equivalent" than $10K. For JPY the cap is roughly ~$70 USD equivalent, which is tighter than intended but still fails-safe (users would hit the override more often).

**Resolution path:** Make the cap currency-aware by fetching the plan's `currency_format.iso_code` at handler time and looking up a per-currency threshold. Not in v0.2 scope; deferred unless a non-USD user complains.
