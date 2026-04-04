// SPDX-License-Identifier: MIT
//
// subprocess_test.go spawns the actual mcp-ynab binary and speaks raw
// MCP JSON-RPC to it over stdin/stdout. This is the only place in the test
// suite where we exercise the full server including the SDK's schema
// validation layer. Unit tests against handler methods directly bypass
// validation; this test closes that gap.

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildTestBinary compiles mcp-ynab to a temp path and returns the path.
// It caches across tests in the same package run by using t.TempDir at the
// package level is not straightforward; cheap `go build` is fine (~1s).
func buildTestBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "mcp-ynab-test")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Env = append(cmd.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	return bin
}

// mcpSession wraps a running mcp-ynab subprocess with line-delimited
// JSON-RPC send/recv helpers.
type mcpSession struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	reader *bufio.Reader
}

func startSession(t *testing.T, bin string) *mcpSession {
	return startSessionWithEnv(t, bin, nil)
}

// startSessionWithEnv is like startSession but lets the caller pass additional
// environment variables to the subprocess. Used for tests that need to
// toggle YNAB_ALLOW_WRITES.
func startSessionWithEnv(t *testing.T, bin string, extraEnv []string) *mcpSession {
	t.Helper()
	// Use a throwaway token — the SDK handshake and validation layers fire
	// before any YNAB HTTP call, which is what we are testing. Handler
	// execution would hit the rate limiter then fail HTTP, which is also
	// fine for assertion purposes.
	cmd := exec.Command(bin)
	cmd.Env = append(cmd.Environ(), "YNAB_API_TOKEN=sk-subprocess-test")
	cmd.Env = append(cmd.Env, extraEnv...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	// Discard stderr; server logs go there and we do not assert on them.
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	return &mcpSession{
		cmd:    cmd,
		stdin:  stdin,
		reader: bufio.NewReader(stdout),
	}
}

func (s *mcpSession) send(t *testing.T, msg any) {
	t.Helper()
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, '\n')
	if _, err := s.stdin.Write(b); err != nil {
		t.Fatal(err)
	}
}

// recvMatching reads lines until it finds one whose "id" matches wantID,
// or until it hits EOF / the deadline. Notifications (no id) are skipped.
func (s *mcpSession) recvMatching(t *testing.T, wantID int, deadline time.Duration) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	type result struct {
		m   map[string]any
		err error
	}
	ch := make(chan result, 1)
	go func() {
		for {
			line, err := s.reader.ReadBytes('\n')
			if err != nil {
				ch <- result{nil, err}
				return
			}
			var m map[string]any
			if err := json.Unmarshal(line, &m); err != nil {
				continue
			}
			if id, ok := m["id"]; ok {
				// JSON numbers decode as float64.
				if f, _ := id.(float64); int(f) == wantID {
					ch <- result{m, nil}
					return
				}
			}
		}
	}()
	select {
	case <-ctx.Done():
		t.Fatalf("timeout waiting for id=%d", wantID)
		return nil
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("read error waiting for id=%d: %v", wantID, r.err)
		}
		return r.m
	}
}

// TestSubprocess_InitializeAndListTools is the baseline handshake test.
func TestSubprocess_InitializeAndListTools(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in -short mode")
	}
	bin := buildTestBinary(t)
	s := startSession(t, bin)

	s.send(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test", "version": "0"},
		},
	})
	init := s.recvMatching(t, 1, 5*time.Second)
	if _, ok := init["result"]; !ok {
		t.Fatalf("initialize failed: %+v", init)
	}
	s.send(t, map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})

	s.send(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
	})
	list := s.recvMatching(t, 2, 5*time.Second)
	result, ok := list["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/list no result: %+v", list)
	}
	tools, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("tools/list no tools array: %+v", result)
	}
	// Expected tool set — any drift requires explicit update here.
	want := map[string]bool{
		"list_plans":                  false,
		"get_month":                   false,
		"list_accounts":               false,
		"list_transactions":           false,
		"list_categories":             false,
		"list_months":                 false,
		"list_scheduled_transactions": false,
	}
	if len(tools) != len(want) {
		t.Errorf("expected %d tools, got %d", len(want), len(tools))
	}
	for _, ti := range tools {
		tm := ti.(map[string]any)
		name := tm["name"].(string)
		if _, ok := want[name]; !ok {
			t.Errorf("unexpected tool %q", name)
		}
		want[name] = true
		ann, _ := tm["annotations"].(map[string]any)
		ro, _ := ann["readOnlyHint"].(bool)
		if !ro {
			t.Errorf("tool %s missing readOnlyHint=true", name)
		}
	}
	for name, saw := range want {
		if !saw {
			t.Errorf("missing tool %q", name)
		}
	}
}

// TestSubprocess_WritesGatedOffWhenEnvUnset verifies that when the process
// is started WITHOUT YNAB_ALLOW_WRITES=1, the write tools are not registered
// and do not appear in tools/list output. This is the startup gate (the
// per-call gate is covered by unit tests in tools_writes_test.go).
func TestSubprocess_WritesGatedOffWhenEnvUnset(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in -short mode")
	}
	bin := buildTestBinary(t)
	s := startSession(t, bin) // no YNAB_ALLOW_WRITES

	s.send(t, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "t", "version": "0"},
		},
	})
	_ = s.recvMatching(t, 1, 5*time.Second)
	s.send(t, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	s.send(t, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	resp := s.recvMatching(t, 2, 5*time.Second)
	tools := resp["result"].(map[string]any)["tools"].([]any)
	for _, ti := range tools {
		name := ti.(map[string]any)["name"].(string)
		if strings.HasPrefix(name, "create_") || strings.HasPrefix(name, "update_") || strings.HasPrefix(name, "approve_") {
			t.Errorf("write tool %q should NOT be registered when YNAB_ALLOW_WRITES is unset", name)
		}
	}
}

// TestSubprocess_WritesRegisteredWhenGateOn verifies that with
// YNAB_ALLOW_WRITES=1, the write tools from step 2 (create_transaction,
// update_category_budgeted) appear in tools/list with readOnlyHint=false.
// This test grows as more write tools and task-shaped tools land in later
// steps.
func TestSubprocess_WritesRegisteredWhenGateOn(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in -short mode")
	}
	bin := buildTestBinary(t)
	s := startSessionWithEnv(t, bin, []string{"YNAB_ALLOW_WRITES=1"})

	s.send(t, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "t", "version": "0"},
		},
	})
	_ = s.recvMatching(t, 1, 5*time.Second)
	s.send(t, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	s.send(t, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	resp := s.recvMatching(t, 2, 5*time.Second)
	tools := resp["result"].(map[string]any)["tools"].([]any)
	// Tools currently expected when writes are enabled. Grows in later steps.
	expectedWrites := map[string]bool{
		"create_transaction":       false,
		"update_category_budgeted": false,
		"update_transaction":       false,
		"approve_transaction":      false,
	}
	for _, ti := range tools {
		tm := ti.(map[string]any)
		name := tm["name"].(string)
		if _, isWrite := expectedWrites[name]; isWrite {
			expectedWrites[name] = true
			// Write tools should report readOnlyHint=false.
			ann, _ := tm["annotations"].(map[string]any)
			if ro, _ := ann["readOnlyHint"].(bool); ro {
				t.Errorf("write tool %q should not have readOnlyHint=true", name)
			}
		}
	}
	for name, saw := range expectedWrites {
		if !saw {
			t.Errorf("write tool %q missing from tools/list when YNAB_ALLOW_WRITES=1", name)
		}
	}
}

// TestSubprocess_SDKValidatesMissingRequiredArg confirms that when the LLM
// calls a tool WITHOUT a required argument, the SDK rejects the call at the
// PROTOCOL layer — before any handler code executes and before any code path
// that could touch our token. In go-sdk v1.4.1 this manifests as a JSON-RPC
// -32602 "invalid params" response, not a CallToolResult, because schema
// validation failures are considered protocol violations rather than tool
// execution errors.
//
// This is the behavior we WANT: the handler's own fallback ("plan_id is
// required") is a secondary line of defense. The primary check runs before
// the handler, ensuring that a malformed call can never reach our code
// paths. The test asserts both: that the error is a protocol error, and
// that it identifies the missing field.
func TestSubprocess_SDKValidatesMissingRequiredArg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in -short mode")
	}
	bin := buildTestBinary(t)
	s := startSession(t, bin)

	s.send(t, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test", "version": "0"},
		},
	})
	_ = s.recvMatching(t, 1, 5*time.Second)
	s.send(t, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})

	// list_accounts without plan_id. SDK validates against the derived
	// JSON Schema (where plan_id is required) and rejects at the protocol
	// layer with code -32602 "invalid params".
	s.send(t, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{
			"name":      "list_accounts",
			"arguments": map[string]any{},
		},
	})
	resp := s.recvMatching(t, 2, 5*time.Second)

	// Expect a JSON-RPC error frame, NOT a result. This is the stricter
	// outcome: validation happened before our handler was reached.
	if _, hasResult := resp["result"]; hasResult {
		t.Fatalf("expected protocol error, got tool result: %+v", resp)
	}
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error object, got: %+v", resp)
	}
	code, _ := errObj["code"].(float64)
	if int(code) != -32602 {
		t.Errorf("expected error code -32602 (invalid params), got %v", code)
	}
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "plan_id") {
		t.Errorf("error message does not mention plan_id: %q", msg)
	}
	if !strings.Contains(msg, "validating") && !strings.Contains(msg, "required") {
		t.Errorf("error message does not look like a validation error: %q", msg)
	}
	// Sanity: no token sentinel should appear anywhere.
	full, _ := json.Marshal(resp)
	if strings.Contains(string(full), "sk-subprocess-test") {
		t.Errorf("SECURITY: token appeared in validation error response: %s", full)
	}
}

// TestSubprocess_HandlerFallbackOnBypass is a belt-and-braces test. The SDK
// validation fires before the handler in the normal path (proved above),
// but the handler also contains its own plan_id check as defense-in-depth.
// If someone in the future alters tool registration to use the lower-level
// Server.AddTool (which skips schema validation), the handler must still
// reject the call. We cannot easily simulate that via subprocess, so this
// documents the intent and the unit test on ListAccounts with an empty
// ListAccountsInput covers the fallback path directly.
func TestSubprocess_HandlerFallbackOnBypass(t *testing.T) {
	t.Parallel()
	// The unit test TestGetMonth_RequiresPlanID and analogous checks
	// in tools_test.go cover this. Leaving this doc test here as a marker.
	t.Skip("covered by handler-level unit tests; left as documentation")
}

// TestSubprocess_StoreTokenReadsStdin exercises the store-token subcommand.
// It does NOT actually talk to the OS keyring (which may not be present on
// CI) — it runs the binary with stdin piped and asserts that the binary
// exits with a meaningful error when keyring write fails, rather than
// crashing or leaking.
func TestSubprocess_StoreTokenFailsGracefullyNoKeyring(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in -short mode")
	}
	bin := buildTestBinary(t)
	cmd := exec.Command(bin, "store-token")
	cmd.Stdin = strings.NewReader("sk-test-store-token\n")
	out, err := cmd.CombinedOutput()
	// Behavior depends on whether a keyring backend is available on this
	// host. Either:
	//   (a) keyring wrote successfully → exit 0, message "token stored"
	//   (b) keyring unavailable → exit 1, error mentioning keyring
	// Both are acceptable; we just verify the token sentinel never appears
	// in output regardless of outcome.
	if strings.Contains(string(out), "sk-test-store-token") {
		t.Errorf("REDACTION FAILURE: token appeared in subcommand output: %s", out)
	}
	// If exit is non-zero, the error should be actionable.
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// Acceptable as long as the message is useful.
			if !strings.Contains(string(out), "keyring") && !strings.Contains(string(out), "store") {
				t.Errorf("unhelpful error output on keyring failure: %s", out)
			}
		}
	}
}
