// SPDX-License-Identifier: MIT
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// emptyReq is a CallToolRequest with Session=nil, used to invoke handler
// methods directly in unit tests. elicitConfirmation treats nil Session as
// a test-path skip (see its doc). Writing tests that need real elicitation
// behavior requires wiring up an in-memory MCP session; those cases are
// limited and live in subprocess_test.go.
func emptyReq() *mcp.CallToolRequest {
	return &mcp.CallToolRequest{}
}

// ---- create_transaction ----------------------------------------------------

func TestCreateTransaction_GateOff(t *testing.T) {
	t.Setenv(envAllowWrites, "")
	client, _ := testClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be called when gate is off")
	})
	_, _, err := client.CreateTransaction(context.Background(), emptyReq(), CreateTransactionInput{
		PlanID: "p", AccountID: "a", AmountMilliunits: 1000, PayeeName: "X",
	})
	if err == nil || !strings.Contains(err.Error(), "YNAB_ALLOW_WRITES") {
		t.Errorf("expected YNAB_ALLOW_WRITES error, got %v", err)
	}
}

func TestCreateTransaction_HappyPath(t *testing.T) {
	t.Setenv(envAllowWrites, "1")
	var seenPath, seenMethod string
	var seenBody map[string]any
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/transactions") && r.Method == http.MethodPost:
			seenPath = r.URL.Path
			seenMethod = r.Method
			_ = json.NewDecoder(r.Body).Decode(&seenBody)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"transaction_ids":["txn-42"],"transaction":{
				"id":"txn-42","date":"2026-04-10","amount":-12340,"memo":"Lunch",
				"cleared":"uncleared","approved":true,"account_id":"acct-1","account_name":"Checking",
				"payee_name":"Chipotle","category_name":"Restaurants","deleted":false
			}}}`))
		case strings.Contains(r.URL.Path, "/accounts/acct-1") && r.Method == http.MethodGet:
			// Post-create balance fetch
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"account":{
				"id":"acct-1","name":"Checking","type":"checking","on_budget":true,"closed":false,
				"balance":987660,"cleared_balance":987660,"uncleared_balance":0,"deleted":false
			}}}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	_, out, err := client.CreateTransaction(context.Background(), emptyReq(), CreateTransactionInput{
		PlanID:           "plan-1",
		AccountID:        "acct-1",
		AmountMilliunits: -12340,
		PayeeName:        "Chipotle",
		Memo:             "Lunch",
		Date:             "2026-04-10",
	})
	if err != nil {
		t.Fatal(err)
	}
	if seenMethod != "POST" || seenPath != "/plans/plan-1/transactions" {
		t.Errorf("wrong request: %s %s", seenMethod, seenPath)
	}
	// Verify body structure
	txn, ok := seenBody["transaction"].(map[string]any)
	if !ok {
		t.Fatalf("request body missing transaction object: %+v", seenBody)
	}
	if txn["account_id"] != "acct-1" {
		t.Errorf("wrong account_id in body: %v", txn["account_id"])
	}
	if txn["amount"].(float64) != -12340 {
		t.Errorf("wrong amount in body: %v", txn["amount"])
	}
	if txn["payee_name"] != "Chipotle" {
		t.Errorf("wrong payee_name in body: %v", txn["payee_name"])
	}
	if txn["approved"] != true {
		t.Errorf("expected default approved=true, got %v", txn["approved"])
	}
	if txn["cleared"] != "uncleared" {
		t.Errorf("expected default cleared=uncleared, got %v", txn["cleared"])
	}
	// Verify output
	if out.Transaction.ID != "txn-42" {
		t.Errorf("wrong txn id in output: %+v", out.Transaction)
	}
	assertMoney(t, "out.Transaction.Amount", out.Transaction.Amount, -12340, "-12.340")
	if out.Before != nil {
		t.Errorf("expected before=nil on create, got %+v", out.Before)
	}
	if out.After == nil {
		t.Fatal("expected after account balance snapshot")
	}
	assertMoney(t, "out.After (account balance)", *out.After, 987660, "987.660")
}

func TestCreateTransaction_DefaultDateIsToday(t *testing.T) {
	// Freeze the clock so the asserted default date is deterministic
	// across midnight UTC boundaries. Review finding H8.
	frozen := time.Date(2027, 5, 20, 12, 0, 0, 0, time.UTC)
	setNowUTC(func() time.Time { return frozen })
	t.Cleanup(resetNowUTC)
	t.Setenv(envAllowWrites, "1")
	var seenBody map[string]any
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&seenBody)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"transaction_ids":["t1"],"transaction":{
				"id":"t1","date":"x","amount":0,"cleared":"uncleared","approved":true,
				"account_id":"a","account_name":"C","deleted":false
			}}}`))
		} else {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"account":{"id":"a","name":"C","type":"checking","on_budget":true,"closed":false,"balance":0,"cleared_balance":0,"uncleared_balance":0,"deleted":false}}}`))
		}
	})
	_, _, err := client.CreateTransaction(context.Background(), emptyReq(), CreateTransactionInput{
		PlanID: "p", AccountID: "a", AmountMilliunits: -100, PayeeName: "X",
		// Date is deliberately empty
	})
	if err != nil {
		t.Fatal(err)
	}
	txn := seenBody["transaction"].(map[string]any)
	expected := frozen.Format("2006-01-02")
	if txn["date"] != expected {
		t.Errorf("expected default date %s, got %v", expected, txn["date"])
	}
}

func TestCreateTransaction_RequiresPayee(t *testing.T) {
	t.Setenv(envAllowWrites, "1")
	client, _ := testClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be called")
	})
	_, _, err := client.CreateTransaction(context.Background(), emptyReq(), CreateTransactionInput{
		PlanID: "p", AccountID: "a", AmountMilliunits: 100,
	})
	if err == nil || !strings.Contains(err.Error(), "payee") {
		t.Errorf("expected payee error, got %v", err)
	}
}

func TestCreateTransaction_RequiresPlanAndAccount(t *testing.T) {
	t.Setenv(envAllowWrites, "1")
	client, _ := testClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be called")
	})
	cases := []CreateTransactionInput{
		{AccountID: "a", AmountMilliunits: 100, PayeeName: "X"},     // missing plan_id
		{PlanID: "p", AmountMilliunits: 100, PayeeName: "X"},        // missing account_id
	}
	for _, in := range cases {
		if _, _, err := client.CreateTransaction(context.Background(), emptyReq(), in); err == nil {
			t.Errorf("expected error for %+v", in)
		}
	}
}

func TestCreateTransaction_MemoLengthLimit(t *testing.T) {
	t.Setenv(envAllowWrites, "1")
	client, _ := testClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be called")
	})
	_, _, err := client.CreateTransaction(context.Background(), emptyReq(), CreateTransactionInput{
		PlanID: "p", AccountID: "a", AmountMilliunits: 100, PayeeName: "X",
		Memo: strings.Repeat("a", 201),
	})
	if err == nil || !strings.Contains(err.Error(), "200") {
		t.Errorf("expected memo length error, got %v", err)
	}
}

func TestCreateTransaction_AmountBoundRejectedWithoutOverride(t *testing.T) {
	t.Setenv(envAllowWrites, "1")
	client, _ := testClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be called")
	})
	_, _, err := client.CreateTransaction(context.Background(), emptyReq(), CreateTransactionInput{
		PlanID: "p", AccountID: "a",
		AmountMilliunits: -15_000_000, // $15K outflow
		PayeeName:        "BigSpend",
	})
	if err == nil || !strings.Contains(err.Error(), "amount_override_milliunits") {
		t.Errorf("expected amount bound error, got %v", err)
	}
}

func TestCreateTransaction_AmountBoundAcceptedWithMatchingOverride(t *testing.T) {
	t.Setenv(envAllowWrites, "1")
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"data":{"transaction_ids":["t1"],"transaction":{
				"id":"t1","date":"2026-04-10","amount":-15000000,"cleared":"uncleared","approved":true,
				"account_id":"a","account_name":"C","deleted":false
			}}}`))
		} else {
			_, _ = w.Write([]byte(`{"data":{"account":{"id":"a","name":"C","type":"checking","on_budget":true,"closed":false,"balance":0,"cleared_balance":0,"uncleared_balance":0,"deleted":false}}}`))
		}
	})
	_, _, err := client.CreateTransaction(context.Background(), emptyReq(), CreateTransactionInput{
		PlanID: "p", AccountID: "a",
		AmountMilliunits:         -15_000_000,
		AmountOverrideMilliunits: -15_000_000,
		PayeeName:                "BigSpend",
		Date:                     "2026-04-10",
	})
	if err != nil {
		t.Fatalf("expected success with matching override, got %v", err)
	}
}

// TestCreateTransaction_InvalidClearedRejected is the H3 regression. YNAB
// accepts only {cleared, uncleared, reconciled}; passing anything else
// through would reach YNAB as an upstream 400 without a clear local error.
func TestCreateTransaction_InvalidClearedRejected(t *testing.T) {
	t.Setenv(envAllowWrites, "1")
	client, _ := testClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be called for invalid cleared")
	})
	_, _, err := client.CreateTransaction(context.Background(), emptyReq(), CreateTransactionInput{
		PlanID: "p", AccountID: "a", AmountMilliunits: -100, PayeeName: "X",
		Cleared: "half-cleared",
	})
	if err == nil || !strings.Contains(err.Error(), "cleared") {
		t.Errorf("expected cleared enum error, got %v", err)
	}
}

// TestCreateTransaction_PayeeIDAndNameMutuallyExclusive is the M5 regression.
// YNAB's precedence semantics for both-set are undocumented; reject locally.
func TestCreateTransaction_PayeeIDAndNameMutuallyExclusive(t *testing.T) {
	t.Setenv(envAllowWrites, "1")
	client, _ := testClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be called when both payee_id and payee_name set")
	})
	_, _, err := client.CreateTransaction(context.Background(), emptyReq(), CreateTransactionInput{
		PlanID: "p", AccountID: "a", AmountMilliunits: -100,
		PayeeID: "some-id", PayeeName: "Chipotle",
	})
	if err == nil || !strings.Contains(err.Error(), "payee") {
		t.Errorf("expected mutex error, got %v", err)
	}
}

// ---- update_category_budgeted ---------------------------------------------

func TestUpdateCategoryBudgeted_GateOff(t *testing.T) {
	t.Setenv(envAllowWrites, "")
	client, _ := testClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be called when gate is off")
	})
	_, _, err := client.UpdateCategoryBudgeted(context.Background(), emptyReq(), UpdateCategoryBudgetedInput{
		PlanID: "p", Month: "current", CategoryID: "c", NewBudgetedMilliunits: 400000,
	})
	if err == nil || !strings.Contains(err.Error(), "YNAB_ALLOW_WRITES") {
		t.Errorf("expected gate error, got %v", err)
	}
}

func TestUpdateCategoryBudgeted_HappyPathReturnsBeforeAndAfter(t *testing.T) {
	// Freeze the clock so the "current" → YYYY-MM-01 resolution in
	// H4 produces a deterministic URL path for this assertion.
	frozen := time.Date(2027, 5, 10, 12, 0, 0, 0, time.UTC)
	setNowUTC(func() time.Time { return frozen })
	t.Cleanup(resetNowUTC)
	t.Setenv(envAllowWrites, "1")
	var seenPaths []string
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		seenPaths = append(seenPaths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			// Before-state fetch
			_, _ = w.Write([]byte(`{"data":{"category":{
				"id":"cat-1","category_group_id":"g","category_group_name":"Immediate","name":"Groceries",
				"hidden":false,"budgeted":350000,"activity":-150000,"balance":200000,"deleted":false
			}}}`))
		case http.MethodPatch:
			// Update response
			_, _ = w.Write([]byte(`{"data":{"category":{
				"id":"cat-1","category_group_id":"g","category_group_name":"Immediate","name":"Groceries",
				"hidden":false,"budgeted":500000,"activity":-150000,"balance":350000,"deleted":false
			}}}`))
		}
	})
	_, out, err := client.UpdateCategoryBudgeted(context.Background(), emptyReq(), UpdateCategoryBudgetedInput{
		PlanID: "plan-1", Month: "current", CategoryID: "cat-1",
		NewBudgetedMilliunits: 500000,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Verify the two expected requests happened in order. The URL uses
	// the frozen-clock-resolved YYYY-MM-01 form (H4) — "current" is a
	// GET convenience only; PATCH requires an ISO date.
	if len(seenPaths) != 2 {
		t.Fatalf("expected 2 requests (GET then PATCH), got %d: %v", len(seenPaths), seenPaths)
	}
	const wantMonthPath = "/plans/plan-1/months/2027-05-01/categories/cat-1"
	if seenPaths[0] != "GET "+wantMonthPath {
		t.Errorf("first request wrong: %s", seenPaths[0])
	}
	if seenPaths[1] != "PATCH "+wantMonthPath {
		t.Errorf("second request wrong: %s", seenPaths[1])
	}
	// Verify before/after snapshots
	assertMoney(t, "before.Budgeted", out.Before.Budgeted, 350000, "350.000")
	assertMoney(t, "before.Balance", out.Before.Balance, 200000, "200.000")
	assertMoney(t, "after.Budgeted", out.After.Budgeted, 500000, "500.000")
	assertMoney(t, "after.Balance", out.After.Balance, 350000, "350.000")
	if out.Category.Name != "Groceries" {
		t.Errorf("wrong category name: %q", out.Category.Name)
	}
}

func TestUpdateCategoryBudgeted_RequiresFields(t *testing.T) {
	t.Setenv(envAllowWrites, "1")
	client, _ := testClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be called")
	})
	cases := []struct {
		name string
		in   UpdateCategoryBudgetedInput
	}{
		{"missing plan_id", UpdateCategoryBudgetedInput{Month: "current", CategoryID: "c"}},
		{"missing category_id", UpdateCategoryBudgetedInput{PlanID: "p", Month: "current"}},
		{"missing month", UpdateCategoryBudgetedInput{PlanID: "p", CategoryID: "c"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := client.UpdateCategoryBudgeted(context.Background(), emptyReq(), c.in)
			if err == nil {
				t.Errorf("expected error for %s", c.name)
			}
		})
	}
}

func TestUpdateCategoryBudgeted_AmountBound(t *testing.T) {
	t.Setenv(envAllowWrites, "1")
	client, _ := testClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be called; amount bound should reject before any request")
	})
	_, _, err := client.UpdateCategoryBudgeted(context.Background(), emptyReq(), UpdateCategoryBudgetedInput{
		PlanID: "p", Month: "current", CategoryID: "c",
		NewBudgetedMilliunits: 15_000_000, // $15K, over cap
	})
	if err == nil || !strings.Contains(err.Error(), "amount_override_milliunits") {
		t.Errorf("expected amount bound error, got %v", err)
	}
}

// ---- update_transaction ----------------------------------------------------

func TestUpdateTransaction_GateOff(t *testing.T) {
	t.Setenv(envAllowWrites, "")
	client, _ := testClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be called when gate is off")
	})
	memo := "new memo"
	_, _, err := client.UpdateTransaction(context.Background(), emptyReq(), UpdateTransactionInput{
		PlanID: "p", TransactionID: "t", Memo: &memo,
	})
	if err == nil || !strings.Contains(err.Error(), "YNAB_ALLOW_WRITES") {
		t.Errorf("expected gate error, got %v", err)
	}
}

func TestUpdateTransaction_RequiresAtLeastOneMutableField(t *testing.T) {
	t.Setenv(envAllowWrites, "1")
	client, _ := testClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be called")
	})
	_, _, err := client.UpdateTransaction(context.Background(), emptyReq(), UpdateTransactionInput{
		PlanID: "p", TransactionID: "t",
	})
	if err == nil || !strings.Contains(err.Error(), "at least one field") {
		t.Errorf("expected no-op error, got %v", err)
	}
}

func TestUpdateTransaction_InputHasNoAmountField(t *testing.T) {
	// This is the structural regression test for "amount updates are not
	// permitted through this tool". Use reflection to confirm the input
	// struct has no field named Amount or AmountMilliunits.
	t.Parallel()
	typ := reflect.TypeOf(UpdateTransactionInput{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name := strings.ToLower(f.Name)
		if strings.Contains(name, "amount") {
			t.Errorf("UpdateTransactionInput has disallowed field %q — amount changes are not supported via update_transaction", f.Name)
		}
	}
}

func TestUpdateTransaction_HappyPathSingleField(t *testing.T) {
	t.Setenv(envAllowWrites, "1")
	var putBody map[string]any
	var seenMethods []string
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		seenMethods = append(seenMethods, r.Method)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			// Before-state fetch
			_, _ = w.Write([]byte(`{"data":{"transaction":{
				"id":"t1","date":"2026-04-05","amount":-5000,
				"memo":"old memo","cleared":"uncleared","approved":false,
				"account_id":"a","account_name":"Checking",
				"payee_name":"Target","category_name":"Shopping","deleted":false
			}}}`))
		case http.MethodPut:
			_ = json.NewDecoder(r.Body).Decode(&putBody)
			// After-state from server
			_, _ = w.Write([]byte(`{"data":{"transaction":{
				"id":"t1","date":"2026-04-05","amount":-5000,
				"memo":"new memo","cleared":"uncleared","approved":false,
				"account_id":"a","account_name":"Checking",
				"payee_name":"Target","category_name":"Shopping","deleted":false
			}}}`))
		}
	})
	newMemo := "new memo"
	_, out, err := client.UpdateTransaction(context.Background(), emptyReq(), UpdateTransactionInput{
		PlanID: "plan-1", TransactionID: "t1", Memo: &newMemo,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seenMethods) != 2 || seenMethods[0] != "GET" || seenMethods[1] != "PUT" {
		t.Errorf("expected GET then PUT, got %v", seenMethods)
	}
	// PUT body should have ONLY the memo field inside transaction, no amount.
	txn, _ := putBody["transaction"].(map[string]any)
	if _, hasAmount := txn["amount"]; hasAmount {
		t.Error("PUT body must NOT include amount field")
	}
	if txn["memo"] != "new memo" {
		t.Errorf("expected memo in PUT body, got %+v", txn)
	}
	// Before/after snapshots should contain ONLY the memo field.
	if out.Before.Memo == nil || *out.Before.Memo != "old memo" {
		t.Errorf("wrong before memo: %+v", out.Before)
	}
	if out.After.Memo == nil || *out.After.Memo != "new memo" {
		t.Errorf("wrong after memo: %+v", out.After)
	}
	// Other fields not touched should be nil in the snapshots.
	if out.Before.CategoryID != nil || out.Before.Approved != nil {
		t.Errorf("snapshot should only contain touched fields, got before=%+v", out.Before)
	}
}

// TestUpdateTransaction_PUTBodyOmitsUntouchedFields is the M11 regression.
// update_transaction relies on YNAB's documented "omitted JSON field =
// leave unchanged" semantics. This test asserts that a single-field
// update produces a PUT body with ONLY that field in the transaction
// object — nothing else. Any drift where we started sending nil as JSON
// null (which YNAB would interpret as "clear the field") would show up
// here as a silent data-corruption regression.
func TestUpdateTransaction_PUTBodyOmitsUntouchedFields(t *testing.T) {
	t.Setenv(envAllowWrites, "1")
	var putBody map[string]any
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"data":{"transaction":{
				"id":"t1","date":"2026-04-05","amount":-5000,
				"memo":"original memo","cleared":"cleared","approved":true,
				"flag_color":"red","account_id":"a","account_name":"Checking",
				"payee_id":"pay-1","payee_name":"Target",
				"category_id":"cat-1","category_name":"Shopping","deleted":false
			}}}`))
		case http.MethodPut:
			_ = json.NewDecoder(r.Body).Decode(&putBody)
			_, _ = w.Write([]byte(`{"data":{"transaction":{
				"id":"t1","date":"2026-04-05","amount":-5000,
				"memo":"new memo","cleared":"cleared","approved":true,
				"flag_color":"red","account_id":"a","account_name":"Checking",
				"payee_id":"pay-1","payee_name":"Target",
				"category_id":"cat-1","category_name":"Shopping","deleted":false
			}}}`))
		}
	})
	newMemo := "new memo"
	if _, _, err := client.UpdateTransaction(context.Background(), emptyReq(), UpdateTransactionInput{
		PlanID: "plan-1", TransactionID: "t1", Memo: &newMemo,
	}); err != nil {
		t.Fatal(err)
	}
	txn, ok := putBody["transaction"].(map[string]any)
	if !ok {
		t.Fatalf("PUT body missing transaction object: %+v", putBody)
	}
	// Only the memo field must be present. Anything else means we'd be
	// overwriting untouched fields on the YNAB side.
	if len(txn) != 1 {
		t.Errorf("PUT transaction body must contain exactly 1 field (memo); got %d: %+v", len(txn), txn)
	}
	if txn["memo"] != "new memo" {
		t.Errorf("memo not set correctly: %+v", txn)
	}
	// Exhaustive absence check — these are the fields that are mutable
	// via update_transaction, so they must not appear unless the caller
	// asked to change them.
	for _, forbidden := range []string{"category_id", "payee_id", "payee_name", "approved", "cleared", "flag_color", "amount", "account_id", "date"} {
		if _, present := txn[forbidden]; present {
			t.Errorf("PUT body must not include %q when caller did not touch it, got %+v", forbidden, txn)
		}
	}
}

func TestUpdateTransaction_LengthAndEnumValidation(t *testing.T) {
	t.Setenv(envAllowWrites, "1")
	client, _ := testClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be called")
	})
	tooLong := strings.Repeat("x", 201)
	tooLongMemo := strings.Repeat("x", 501)
	badCleared := "done"
	badFlag := "chartreuse"

	cases := []struct {
		name string
		in   UpdateTransactionInput
		want string
	}{
		{"payee_name > 200", UpdateTransactionInput{PlanID: "p", TransactionID: "t", PayeeName: &tooLong}, "200"},
		{"memo > 500", UpdateTransactionInput{PlanID: "p", TransactionID: "t", Memo: &tooLongMemo}, "500"},
		{"bad cleared", UpdateTransactionInput{PlanID: "p", TransactionID: "t", Cleared: &badCleared}, "cleared"},
		{"bad flag", UpdateTransactionInput{PlanID: "p", TransactionID: "t", FlagColor: &badFlag}, "flag_color"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := client.UpdateTransaction(context.Background(), emptyReq(), c.in)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("expected error containing %q, got %v", c.want, err)
			}
		})
	}
}

// ---- approve_transaction ---------------------------------------------------

func TestApproveTransaction_HappyPathNoElicit(t *testing.T) {
	t.Setenv(envAllowWrites, "1")
	var putBody map[string]any
	var methods []string
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"data":{"transaction":{
				"id":"t1","date":"2026-04-05","amount":-1000,"memo":null,
				"cleared":"uncleared","approved":false,
				"account_id":"a","account_name":"Checking","deleted":false
			}}}`))
		case http.MethodPut:
			_ = json.NewDecoder(r.Body).Decode(&putBody)
			_, _ = w.Write([]byte(`{"data":{"transaction":{
				"id":"t1","date":"2026-04-05","amount":-1000,"memo":null,
				"cleared":"uncleared","approved":true,
				"account_id":"a","account_name":"Checking","deleted":false
			}}}`))
		}
	})
	_, out, err := client.ApproveTransaction(context.Background(), emptyReq(), ApproveTransactionInput{
		PlanID: "plan-1", TransactionID: "t1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(methods) != 2 || methods[1] != "PUT" {
		t.Errorf("wrong requests: %v", methods)
	}
	// Body must contain approved=true and NO other fields (except what our
	// omitempty marshaling decides on nil pointers).
	txn := putBody["transaction"].(map[string]any)
	if txn["approved"] != true {
		t.Errorf("expected approved=true in PUT body, got %+v", txn)
	}
	// Should not include any other write fields.
	for _, k := range []string{"memo", "category_id", "payee_id", "payee_name", "cleared", "flag_color"} {
		if _, present := txn[k]; present {
			t.Errorf("PUT body should not include %q, got %+v", k, txn)
		}
	}
	// Before/after should show only the approved flip.
	if out.Before.Approved == nil || *out.Before.Approved != false {
		t.Errorf("before approved should be false, got %+v", out.Before)
	}
	if out.After.Approved == nil || *out.After.Approved != true {
		t.Errorf("after approved should be true, got %+v", out.After)
	}
	if out.Before.Memo != nil || out.After.Memo != nil {
		t.Errorf("unchanged fields should be nil in snapshots")
	}
}

func TestApproveTransaction_GateOff(t *testing.T) {
	t.Setenv(envAllowWrites, "")
	client, _ := testClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be called when gate is off")
	})
	_, _, err := client.ApproveTransaction(context.Background(), emptyReq(), ApproveTransactionInput{
		PlanID: "p", TransactionID: "t",
	})
	if err == nil || !strings.Contains(err.Error(), "YNAB_ALLOW_WRITES") {
		t.Errorf("expected gate error, got %v", err)
	}
}

// ---- registerTools gating --------------------------------------------------

// TestRegisterTools_WritesNotRegisteredWhenGateOff is covered at the
// subprocess level by the existing TestSubprocess_InitializeAndListTools,
// but that test currently hard-codes the v0.1.0 tool set. We update it in
// Step 12 when we add the other v0.2 tools. For now, this in-process test
// verifies the registerTools function respects the env var.
func TestRegisterTools_WritesGatedOffByEnvVar(t *testing.T) {
	t.Setenv(envAllowWrites, "")
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	client := &Client{token: NewToken("x")}
	registerTools(server, client)
	// Structural check only: registerTools must not panic under the
	// gate-off branch. The go-sdk Server does not expose a public
	// tool-enumeration API in v1.4.1, so behavioral verification that
	// writes are NOT listed lives in TestSubprocess_WritesGatedOffWhenEnvUnset
	// which speaks the MCP tools/list protocol against a real subprocess.
	// Review finding L7.
}

func TestRegisterTools_WritesRegisteredWhenGateOn(t *testing.T) {
	t.Setenv(envAllowWrites, "1")
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	client := &Client{token: NewToken("x")}
	registerTools(server, client)
	// Structural check only. Behavioral verification that every write
	// tool IS listed lives in TestSubprocess_WritesRegisteredWhenGateOn,
	// which asserts the exact expected set of 4 write tools via MCP
	// tools/list. Review finding L7.
}
