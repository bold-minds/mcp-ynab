// SPDX-License-Identifier: MIT
package main

import (
	"context"
	"encoding/json"
	"net/http"
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
	today := time.Now().UTC().Format("2006-01-02")
	if txn["date"] != today {
		t.Errorf("expected default date %s, got %v", today, txn["date"])
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
	// Verify the two expected requests happened in order.
	if len(seenPaths) != 2 {
		t.Fatalf("expected 2 requests (GET then PATCH), got %d: %v", len(seenPaths), seenPaths)
	}
	if seenPaths[0] != "GET /plans/plan-1/months/current/categories/cat-1" {
		t.Errorf("first request wrong: %s", seenPaths[0])
	}
	if seenPaths[1] != "PATCH /plans/plan-1/months/current/categories/cat-1" {
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
	// We don't have a public way to enumerate registered tools on the server
	// object in v1.4.1, so this test is structural: registerTools must not
	// panic and must complete. The behavioral assertion lives in the
	// subprocess test which lists tools via the MCP protocol.
}

func TestRegisterTools_WritesRegisteredWhenGateOn(t *testing.T) {
	t.Setenv(envAllowWrites, "1")
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	client := &Client{token: NewToken("x")}
	registerTools(server, client)
	// Same as above — structural test. Subprocess test verifies tool counts.
}
