// SPDX-License-Identifier: MIT
package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// assertMoney is a small helper for asserting both fields of a Money value
// in one line without repeating boilerplate in every test.
func assertMoney(t *testing.T, label string, got Money, wantMilliunits int64, wantDecimal string) {
	t.Helper()
	if got.Milliunits != wantMilliunits || got.Decimal != wantDecimal {
		t.Errorf("%s: got %+v; want {Milliunits:%d Decimal:%q}",
			label, got, wantMilliunits, wantDecimal)
	}
}

// ---- list_plans -------------------------------------------------------------

func TestListPlans_Success(t *testing.T) {
	t.Parallel()
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/plans" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"plans":[
			{"id":"aaa","name":"Personal","first_month":"2020-01-01","last_month":"2026-04-01"},
			{"id":"bbb","name":"Business","first_month":"2022-01-01","last_month":"2026-04-01"}
		]}}`))
	})
	_, out, err := client.ListPlans(context.Background(), nil, ListPlansInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Plans) != 2 {
		t.Fatalf("expected 2 plans, got %d", len(out.Plans))
	}
	// ListPlans sorts by name (deterministic output contract) — "Business"
	// precedes "Personal" alphabetically regardless of YNAB response order.
	if out.Plans[0].Name != "Business" || out.Plans[1].Name != "Personal" {
		t.Errorf("unexpected plans: %+v", out.Plans)
	}
}

func TestListPlans_Error404(t *testing.T) {
	t.Parallel()
	client, _ := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":{"id":"404","name":"not_found","detail":"no plans"}}`))
	})
	_, _, err := client.ListPlans(context.Background(), nil, ListPlansInput{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "not_found") {
		t.Errorf("wrong error: %v", err)
	}
}

// ---- get_month --------------------------------------------------------------

func TestGetMonth_DefaultsToCurrent(t *testing.T) {
	t.Parallel()
	var seenPath string
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"month":{
			"month":"2026-04-01","income":5000000,"budgeted":4500000,
			"activity":-2000000,"to_be_budgeted":500000,"age_of_money":42,
			"categories":[]
		}}}`))
	})
	_, out, err := client.GetMonth(context.Background(), nil, GetMonthInput{PlanID: "plan-123"})
	if err != nil {
		t.Fatal(err)
	}
	if seenPath != "/plans/plan-123/months/current" {
		t.Errorf("wrong path, got %q", seenPath)
	}
	assertMoney(t, "Income", out.Income, 5000000, "5000.000")
	assertMoney(t, "Budgeted", out.Budgeted, 4500000, "4500.000")
	assertMoney(t, "Activity", out.Activity, -2000000, "-2000.000")
	assertMoney(t, "ToBeBudgeted", out.ToBeBudgeted, 500000, "500.000")
	if out.AgeOfMoney == nil || *out.AgeOfMoney != 42 {
		t.Errorf("age of money wrong: %v", out.AgeOfMoney)
	}
}

func TestGetMonth_RequiresPlanID(t *testing.T) {
	t.Parallel()
	client, _ := testClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be called")
	})
	_, _, err := client.GetMonth(context.Background(), nil, GetMonthInput{})
	if err == nil || !strings.Contains(err.Error(), "plan_id is required") {
		t.Errorf("wrong error: %v", err)
	}
}

// ---- list_accounts ----------------------------------------------------------

func TestListAccounts_FiltersDeletedAndClosed(t *testing.T) {
	t.Parallel()
	client, _ := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"accounts":[
			{"id":"a","name":"Checking","type":"checking","on_budget":true,"closed":false,"balance":1500000,"cleared_balance":1500000,"uncleared_balance":0,"deleted":false},
			{"id":"b","name":"Old Savings","type":"savings","on_budget":true,"closed":true,"balance":0,"cleared_balance":0,"uncleared_balance":0,"deleted":false},
			{"id":"c","name":"Ghost","type":"cash","on_budget":false,"closed":false,"balance":0,"cleared_balance":0,"uncleared_balance":0,"deleted":true}
		]}}`))
	})
	_, out, err := client.ListAccounts(context.Background(), nil, ListAccountsInput{PlanID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Accounts) != 1 || out.Accounts[0].Name != "Checking" {
		t.Errorf("default filter wrong: %+v", out.Accounts)
	}
	assertMoney(t, "Balance", out.Accounts[0].Balance, 1500000, "1500.000")
}

func TestListAccounts_IncludeClosed(t *testing.T) {
	t.Parallel()
	client, _ := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"accounts":[
			{"id":"a","name":"Checking","type":"checking","on_budget":true,"closed":false,"balance":1000000,"cleared_balance":1000000,"uncleared_balance":0,"deleted":false},
			{"id":"b","name":"Old","type":"savings","on_budget":true,"closed":true,"balance":0,"cleared_balance":0,"uncleared_balance":0,"deleted":false}
		]}}`))
	})
	_, out, err := client.ListAccounts(context.Background(), nil, ListAccountsInput{PlanID: "p", IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Accounts) != 2 {
		t.Errorf("expected 2 accounts, got %d", len(out.Accounts))
	}
}

// ---- list_transactions ------------------------------------------------------

func TestListTransactions_SortsDescAndTruncates(t *testing.T) {
	t.Parallel()
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("since_date"); got != "2026-03-01" {
			t.Errorf("since_date not forwarded: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"transactions":[
			{"id":"t1","date":"2026-03-05","amount":-12340,"cleared":"cleared","approved":true,"account_id":"a","account_name":"Checking","deleted":false},
			{"id":"t2","date":"2026-03-10","amount":-56780,"cleared":"cleared","approved":true,"account_id":"a","account_name":"Checking","deleted":false},
			{"id":"t3","date":"2026-03-01","amount":-1000,"cleared":"cleared","approved":true,"account_id":"a","account_name":"Checking","deleted":false},
			{"id":"deleted","date":"2026-03-20","amount":-1,"cleared":"cleared","approved":true,"account_id":"a","account_name":"Checking","deleted":true}
		]}}`))
	})
	_, out, err := client.ListTransactions(context.Background(), nil, ListTransactionsInput{
		PlanID:    "p",
		SinceDate: "2026-03-01",
		Limit:     2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Transactions) != 2 {
		t.Fatalf("expected 2 after truncate, got %d", len(out.Transactions))
	}
	if out.Transactions[0].ID != "t2" || out.Transactions[1].ID != "t1" {
		t.Errorf("wrong order: %+v", out.Transactions)
	}
	if !out.Truncated {
		t.Error("expected truncated=true")
	}
	// t2 amount: -56780 milliunits
	assertMoney(t, "t2 Amount", out.Transactions[0].Amount, -56780, "-56.780")
}

func TestListTransactions_InvalidTypeRejected(t *testing.T) {
	t.Parallel()
	client, _ := testClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be called")
	})
	_, _, err := client.ListTransactions(context.Background(), nil, ListTransactionsInput{
		PlanID: "p",
		Type:   "delete_everything",
	})
	if err == nil || !strings.Contains(err.Error(), "uncategorized") {
		t.Errorf("wrong error: %v", err)
	}
}

func TestListTransactions_LimitCapping(t *testing.T) {
	t.Parallel()
	// Build a server response containing 700 rows so the cap logic has
	// something to bite on. Prior to this revision the test asserted only
	// err != nil, which meant a broken cap (e.g. returning all 700 rows
	// unconditionally) would still pass. Assert the actual post-cap
	// lengths instead.
	var body []byte
	body = append(body, []byte(`{"data":{"transactions":[`)...)
	for i := range 700 {
		if i > 0 {
			body = append(body, ',')
		}
		body = append(body, []byte(fmt.Sprintf(
			`{"id":"t%d","date":"2026-03-%02d","amount":-1000,"account_id":"a","approved":true,"cleared":"cleared","deleted":false}`,
			i, 1+(i%28),
		))...)
	}
	body = append(body, []byte(`]}}`)...)

	client, _ := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})

	// Expected lengths after the handler's default/cap/clamp logic:
	//   limit=0   → default 100
	//   limit=-5  → default 100
	//   limit=50  → honored
	//   limit=9999 → clamped to 500
	cases := []struct {
		in   int
		want int
	}{
		{0, 100}, {-5, 100}, {50, 50}, {9999, 500},
	}
	for _, c := range cases {
		_, out, err := client.ListTransactions(context.Background(), nil, ListTransactionsInput{
			PlanID: "p", Limit: c.in, SinceDate: "2020-01-01",
		})
		if err != nil {
			t.Errorf("limit %d: %v", c.in, err)
			continue
		}
		if len(out.Transactions) != c.want {
			t.Errorf("limit %d: expected %d rows, got %d", c.in, c.want, len(out.Transactions))
		}
		if !out.Truncated {
			t.Errorf("limit %d: Truncated should be true (700 rows > cap)", c.in)
		}
	}
}

// ---- list_categories --------------------------------------------------------

func TestListCategories_FlattensAndFilters(t *testing.T) {
	t.Parallel()
	client, _ := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"category_groups":[
			{"id":"g1","name":"Immediate","hidden":false,"deleted":false,"categories":[
				{"id":"c1","name":"Groceries","category_group_id":"g1","category_group_name":"Immediate","hidden":false,"budgeted":400000,"activity":-150000,"balance":250000,"deleted":false},
				{"id":"c2","name":"HiddenCat","category_group_id":"g1","category_group_name":"Immediate","hidden":true,"budgeted":0,"activity":0,"balance":0,"deleted":false}
			]},
			{"id":"g2","name":"GoneGroup","hidden":false,"deleted":true,"categories":[
				{"id":"c3","name":"ShouldSkip","category_group_id":"g2","category_group_name":"GoneGroup","hidden":false,"budgeted":0,"activity":0,"balance":0,"deleted":false}
			]}
		]}}`))
	})
	_, out, err := client.ListCategories(context.Background(), nil, ListCategoriesInput{PlanID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Categories) != 1 || out.Categories[0].Name != "Groceries" {
		t.Errorf("wrong categories: %+v", out.Categories)
	}
	assertMoney(t, "Budgeted", out.Categories[0].Budgeted, 400000, "400.000")
	assertMoney(t, "Activity", out.Categories[0].Activity, -150000, "-150.000")
	assertMoney(t, "Balance", out.Categories[0].Balance, 250000, "250.000")
}

// ---- list_payees -----------------------------------------------------------

func TestListPayees_Success(t *testing.T) {
	t.Parallel()
	var seenPath string
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"payees":[
			{"id":"p1","name":"Chipotle","transfer_account_id":null,"deleted":false},
			{"id":"p2","name":"Whole Foods","transfer_account_id":null,"deleted":false},
			{"id":"p3","name":"Transfer: Savings","transfer_account_id":"acct-s","deleted":false}
		]}}`))
	})
	_, out, err := client.ListPayees(context.Background(), nil, ListPayeesInput{PlanID: "plan-1"})
	if err != nil {
		t.Fatal(err)
	}
	if seenPath != "/plans/plan-1/payees" {
		t.Errorf("wrong path: %s", seenPath)
	}
	if len(out.Payees) != 3 {
		t.Errorf("expected 3 payees, got %d", len(out.Payees))
	}
	// Transfer payee should have transfer_account_id populated.
	var transferPayee *Payee
	for i := range out.Payees {
		if out.Payees[i].ID == "p3" {
			transferPayee = &out.Payees[i]
		}
	}
	if transferPayee == nil || transferPayee.TransferAccountID != "acct-s" {
		t.Errorf("transfer payee missing or wrong: %+v", transferPayee)
	}
}

func TestListPayees_NameContainsCaseInsensitive(t *testing.T) {
	t.Parallel()
	client, _ := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"payees":[
			{"id":"p1","name":"CHIPOTLE Mexican Grill","deleted":false},
			{"id":"p2","name":"chipotle Downtown","deleted":false},
			{"id":"p3","name":"Whole Foods","deleted":false},
			{"id":"p4","name":"Chick-fil-A","deleted":false}
		]}}`))
	})
	_, out, err := client.ListPayees(context.Background(), nil, ListPayeesInput{
		PlanID:       "p",
		NameContains: "chipotle",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Should match p1 and p2 but not p3 or p4.
	if len(out.Payees) != 2 {
		t.Fatalf("expected 2 matches for 'chipotle', got %d: %+v", len(out.Payees), out.Payees)
	}
	seen := map[string]bool{}
	for _, p := range out.Payees {
		seen[p.ID] = true
	}
	if !seen["p1"] || !seen["p2"] {
		t.Errorf("wrong matches: %+v", out.Payees)
	}
}

func TestListPayees_NameContainsUppercaseInputStillCaseInsensitive(t *testing.T) {
	t.Parallel()
	client, _ := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"payees":[
			{"id":"p1","name":"starbucks","deleted":false}
		]}}`))
	})
	// Caller passes uppercase; should still match lowercase payee name.
	_, out, err := client.ListPayees(context.Background(), nil, ListPayeesInput{
		PlanID: "p", NameContains: "STARBUCKS",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Payees) != 1 {
		t.Errorf("expected 1 match, got %d", len(out.Payees))
	}
}

func TestListPayees_FiltersDeletedByDefault(t *testing.T) {
	t.Parallel()
	client, _ := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"payees":[
			{"id":"p1","name":"Active","deleted":false},
			{"id":"p2","name":"Deleted","deleted":true}
		]}}`))
	})
	_, out, err := client.ListPayees(context.Background(), nil, ListPayeesInput{PlanID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Payees) != 1 || out.Payees[0].Name != "Active" {
		t.Errorf("deleted not filtered: %+v", out.Payees)
	}
}

func TestListPayees_IncludeDeleted(t *testing.T) {
	t.Parallel()
	client, _ := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"payees":[
			{"id":"p1","name":"Active","deleted":false},
			{"id":"p2","name":"Deleted","deleted":true}
		]}}`))
	})
	_, out, err := client.ListPayees(context.Background(), nil, ListPayeesInput{PlanID: "p", IncludeDeleted: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Payees) != 2 {
		t.Errorf("expected 2 with include_deleted, got %d", len(out.Payees))
	}
}

func TestListPayees_RequiresPlanID(t *testing.T) {
	t.Parallel()
	client, _ := testClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be called")
	})
	_, _, err := client.ListPayees(context.Background(), nil, ListPayeesInput{})
	if err == nil || !strings.Contains(err.Error(), "plan_id is required") {
		t.Errorf("wrong error: %v", err)
	}
}

// ---- list_transactions filter routing --------------------------------------

func TestListTransactions_AccountFilterHitsAccountEndpoint(t *testing.T) {
	t.Parallel()
	var seenPath string
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"transactions":[
			{"id":"t1","date":"2026-03-15","amount":-5000,"cleared":"cleared","approved":true,"account_id":"acct-1","account_name":"Checking","deleted":false}
		]}}`))
	})
	_, out, err := client.ListTransactions(context.Background(), nil, ListTransactionsInput{
		PlanID:    "p",
		AccountID: "acct-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if seenPath != "/plans/p/accounts/acct-1/transactions" {
		t.Errorf("wrong path, got %q", seenPath)
	}
	if len(out.Transactions) != 1 || out.Transactions[0].IsSubtransaction {
		t.Errorf("wrong result: %+v", out.Transactions)
	}
}

func TestListTransactions_CategoryFilterHitsHybridEndpointAndFlagsSubtransactions(t *testing.T) {
	t.Parallel()
	var seenPath string
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		// The category endpoint returns HybridTransactions with a "type"
		// discriminator. Split transactions appear as subtransaction rows.
		_, _ = w.Write([]byte(`{"data":{"transactions":[
			{"id":"t1","type":"transaction","parent_transaction_id":null,"date":"2026-03-10","amount":-10000,"cleared":"cleared","approved":true,"account_id":"a","account_name":"Checking","category_name":"Groceries","deleted":false},
			{"id":"t2","type":"subtransaction","parent_transaction_id":"parent-xyz","date":"2026-03-12","amount":-2500,"cleared":"cleared","approved":true,"account_id":"a","account_name":"Checking","category_name":"Groceries","deleted":false}
		]}}`))
	})
	_, out, err := client.ListTransactions(context.Background(), nil, ListTransactionsInput{
		PlanID:     "p",
		CategoryID: "cat-99",
	})
	if err != nil {
		t.Fatal(err)
	}
	if seenPath != "/plans/p/categories/cat-99/transactions" {
		t.Errorf("wrong path, got %q", seenPath)
	}
	if len(out.Transactions) != 2 {
		t.Fatalf("expected 2, got %d", len(out.Transactions))
	}
	// Sorted descending by date — t2 is 2026-03-12, t1 is 2026-03-10.
	if out.Transactions[0].ID != "t2" || !out.Transactions[0].IsSubtransaction {
		t.Errorf("expected t2 (subtransaction) first, got %+v", out.Transactions[0])
	}
	if out.Transactions[1].ID != "t1" || out.Transactions[1].IsSubtransaction {
		t.Errorf("expected t1 (not subtransaction) second, got %+v", out.Transactions[1])
	}
}

func TestListTransactions_PayeeFilterHitsPayeeEndpoint(t *testing.T) {
	t.Parallel()
	var seenPath string
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"transactions":[]}}`))
	})
	_, _, err := client.ListTransactions(context.Background(), nil, ListTransactionsInput{
		PlanID:  "p",
		PayeeID: "payee-7",
	})
	if err != nil {
		t.Fatal(err)
	}
	if seenPath != "/plans/p/payees/payee-7/transactions" {
		t.Errorf("wrong path, got %q", seenPath)
	}
}

func TestListTransactions_RejectsMultipleScopeFilters(t *testing.T) {
	t.Parallel()
	client, _ := testClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be called")
	})
	cases := []ListTransactionsInput{
		{PlanID: "p", AccountID: "a", CategoryID: "c"},
		{PlanID: "p", AccountID: "a", PayeeID: "y"},
		{PlanID: "p", CategoryID: "c", PayeeID: "y"},
		{PlanID: "p", AccountID: "a", CategoryID: "c", PayeeID: "y"},
	}
	for _, in := range cases {
		_, _, err := client.ListTransactions(context.Background(), nil, in)
		if err == nil || !strings.Contains(err.Error(), "at most one") {
			t.Errorf("expected rejection for %+v, got %v", in, err)
		}
	}
}

func TestListTransactions_DeletedHybridRowsFiltered(t *testing.T) {
	t.Parallel()
	client, _ := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"transactions":[
			{"id":"live","type":"transaction","date":"2026-03-01","amount":-1000,"cleared":"cleared","approved":true,"account_id":"a","account_name":"Checking","deleted":false},
			{"id":"gone","type":"transaction","date":"2026-03-02","amount":-1,"cleared":"cleared","approved":true,"account_id":"a","account_name":"Checking","deleted":true}
		]}}`))
	})
	_, out, err := client.ListTransactions(context.Background(), nil, ListTransactionsInput{
		PlanID:     "p",
		CategoryID: "cat",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Transactions) != 1 || out.Transactions[0].ID != "live" {
		t.Errorf("deleted hybrid row not filtered: %+v", out.Transactions)
	}
}

// ---- list_months ------------------------------------------------------------

func TestListMonths_SortsDescAndFiltersDeleted(t *testing.T) {
	t.Parallel()
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/plans/p/months" {
			t.Errorf("wrong path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"months":[
			{"month":"2026-01-01","income":5000000,"budgeted":4800000,"activity":-2100000,"to_be_budgeted":0,"age_of_money":50,"deleted":false},
			{"month":"2026-03-01","income":5200000,"budgeted":5000000,"activity":-2300000,"to_be_budgeted":100000,"age_of_money":55,"deleted":false},
			{"month":"2025-12-01","income":4800000,"budgeted":4600000,"activity":-2000000,"to_be_budgeted":0,"age_of_money":48,"deleted":false},
			{"month":"2026-02-01","income":5100000,"budgeted":4900000,"activity":-2200000,"to_be_budgeted":0,"age_of_money":52,"deleted":false},
			{"month":"2020-01-01","income":0,"budgeted":0,"activity":0,"to_be_budgeted":0,"deleted":true}
		]}}`))
	})
	_, out, err := client.ListMonths(context.Background(), nil, ListMonthsInput{PlanID: "p", Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Months) != 3 {
		t.Fatalf("expected 3 months, got %d", len(out.Months))
	}
	// Most recent first, deleted (2020-01-01) excluded.
	want := []string{"2026-03-01", "2026-02-01", "2026-01-01"}
	for i, w := range want {
		if out.Months[i].Month != w {
			t.Errorf("pos %d: got %q, want %q", i, out.Months[i].Month, w)
		}
	}
	assertMoney(t, "income 2026-03-01", out.Months[0].Income, 5200000, "5200.000")
}

func TestListMonths_LimitDefaultAndCap(t *testing.T) {
	t.Parallel()
	// Build a response with 80 months to exercise the default (6) and cap (60).
	var body strings.Builder
	body.WriteString(`{"data":{"months":[`)
	for i := 0; i < 80; i++ {
		if i > 0 {
			body.WriteString(",")
		}
		// Synthesize a unique month per row for sort stability.
		year := 2020 + i/12
		month := (i % 12) + 1
		fmt.Fprintf(&body, `{"month":"%04d-%02d-01","income":0,"budgeted":0,"activity":0,"to_be_budgeted":0,"deleted":false}`, year, month)
	}
	body.WriteString(`]}}`)
	client, _ := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body.String()))
	})
	// Default limit = 6.
	_, out, err := client.ListMonths(context.Background(), nil, ListMonthsInput{PlanID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Months) != 6 {
		t.Errorf("default limit: expected 6, got %d", len(out.Months))
	}
	// Requested 9999 → capped to 60.
	_, out, err = client.ListMonths(context.Background(), nil, ListMonthsInput{PlanID: "p", Limit: 9999})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Months) != 60 {
		t.Errorf("cap: expected 60, got %d", len(out.Months))
	}
}

func TestListMonths_RequiresPlanID(t *testing.T) {
	t.Parallel()
	client, _ := testClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be called")
	})
	_, _, err := client.ListMonths(context.Background(), nil, ListMonthsInput{})
	if err == nil || !strings.Contains(err.Error(), "plan_id is required") {
		t.Errorf("wrong error: %v", err)
	}
}

// ---- list_scheduled_transactions --------------------------------------------

func TestListScheduledTransactions_Success(t *testing.T) {
	t.Parallel()
	var seenPath string
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"scheduled_transactions":[
			{"id":"s1","date_first":"2026-04-01","date_next":"2026-05-01","frequency":"monthly","amount":-150000,"memo":"Rent","account_id":"a","account_name":"Checking","payee_name":"Landlord","category_name":"Rent","deleted":false},
			{"id":"s2","date_first":"2026-04-15","date_next":"2026-04-15","frequency":"monthly","amount":-50000,"account_id":"a","account_name":"Checking","payee_name":"Netflix","category_name":"Subscriptions","deleted":false},
			{"id":"gone","date_first":"2020-01-01","date_next":"2020-01-01","frequency":"never","amount":0,"account_id":"a","account_name":"Checking","deleted":true}
		]}}`))
	})
	_, out, err := client.ListScheduledTransactions(context.Background(), nil, ListScheduledTransactionsInput{PlanID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if seenPath != "/plans/p/scheduled_transactions" {
		t.Errorf("wrong path: %q", seenPath)
	}
	if len(out.ScheduledTransactions) != 2 {
		t.Fatalf("expected 2, got %d", len(out.ScheduledTransactions))
	}
	// Sorted ascending by date_next (soonest first).
	if out.ScheduledTransactions[0].ID != "s2" || out.ScheduledTransactions[1].ID != "s1" {
		t.Errorf("wrong order: %+v", out.ScheduledTransactions)
	}
	assertMoney(t, "rent amount", out.ScheduledTransactions[1].Amount, -150000, "-150.000")
}

func TestListScheduledTransactions_UpcomingDaysFilter(t *testing.T) {
	// Clock override is atomic-safe; don't t.Parallel() the leaf so the
	// asserted cutoff value stays deterministic for this test's body.
	// Review findings M8 and L4.
	frozen := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	setNowUTC(func() time.Time { return frozen })
	t.Cleanup(resetNowUTC)
	today := frozen.Format("2006-01-02")
	farFuture := frozen.AddDate(1, 0, 0).Format("2006-01-02") // +1 year
	body := fmt.Sprintf(`{"data":{"scheduled_transactions":[
		{"id":"soon","date_first":"%s","date_next":"%s","frequency":"monthly","amount":-1000,"account_id":"a","account_name":"Checking","deleted":false},
		{"id":"later","date_first":"%s","date_next":"%s","frequency":"yearly","amount":-1000,"account_id":"a","account_name":"Checking","deleted":false}
	]}}`, today, today, farFuture, farFuture)
	client, _ := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	// upcoming_days=1 should exclude the far-future one.
	_, out, err := client.ListScheduledTransactions(context.Background(), nil, ListScheduledTransactionsInput{
		PlanID:       "p",
		UpcomingDays: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.ScheduledTransactions) != 1 || out.ScheduledTransactions[0].ID != "soon" {
		t.Errorf("upcoming_days filter wrong: %+v", out.ScheduledTransactions)
	}
}

func TestListScheduledTransactions_RejectsInvalidUpcomingDays(t *testing.T) {
	t.Parallel()
	client, _ := testClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be called")
	})
	for _, d := range []int{-1, -100, 366, 9999} {
		_, _, err := client.ListScheduledTransactions(context.Background(), nil, ListScheduledTransactionsInput{
			PlanID:       "p",
			UpcomingDays: d,
		})
		if err == nil {
			t.Errorf("expected error for upcoming_days=%d", d)
		}
	}
}

func TestListCategories_IncludeHidden(t *testing.T) {
	t.Parallel()
	client, _ := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"category_groups":[
			{"id":"g1","name":"G","hidden":false,"deleted":false,"categories":[
				{"id":"c1","name":"Visible","category_group_id":"g1","category_group_name":"G","hidden":false,"budgeted":0,"activity":0,"balance":0,"deleted":false},
				{"id":"c2","name":"Hidden","category_group_id":"g1","category_group_name":"G","hidden":true,"budgeted":0,"activity":0,"balance":0,"deleted":false}
			]}
		]}}`))
	})
	_, out, err := client.ListCategories(context.Background(), nil, ListCategoriesInput{PlanID: "p", IncludeHidden: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Categories) != 2 {
		t.Errorf("expected 2, got %d", len(out.Categories))
	}
}
