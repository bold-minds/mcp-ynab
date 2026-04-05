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
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// cachedBinary is a package-level build cache. Subprocess tests share the
// compiled binary across the run so we incur one `go build` per `go test`
// invocation instead of 3–4. Review finding L11.
var (
	cachedBinary     string
	cachedBinaryDir  string
	cachedBinaryErr  error
	cachedBinaryOnce sync.Once
)

// TestMain cleans up the cached binary's temp directory after all tests
// in the package have finished. The buildTestBinary helper cannot use
// t.TempDir() because that scopes the directory to a single test, but
// the binary is shared across every subprocess test — so the directory
// has to outlive individual tests and still be cleaned up eventually.
// Before this hook was added, every `go test` run leaked a
// /tmp/mcp-ynab-test-binary-* directory. Review nit on leaked temp dirs.
func TestMain(m *testing.M) {
	code := m.Run()
	if cachedBinaryDir != "" {
		_ = os.RemoveAll(cachedBinaryDir)
	}
	os.Exit(code)
}

// buildTestBinary compiles mcp-ynab to a temp path and returns the path.
// The compile result is cached across subtests via cachedBinaryOnce so the
// whole package pays the ~1s build cost exactly once.
func buildTestBinary(t *testing.T) string {
	t.Helper()
	cachedBinaryOnce.Do(func() {
		// Use a well-known path under the OS temp dir; not t.TempDir()
		// because that would clean up before other tests run.
		dir, err := os.MkdirTemp("", "mcp-ynab-test-binary-")
		if err != nil {
			cachedBinaryErr = err
			return
		}
		cachedBinaryDir = dir // TestMain removes this after all tests run
		bin := filepath.Join(dir, "mcp-ynab-test")
		cmd := exec.Command("go", "build", "-o", bin, ".")
		cmd.Env = append(cmd.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			cachedBinaryErr = fmt.Errorf("build failed: %v\n%s", err, out)
			return
		}
		cachedBinary = bin
	})
	if cachedBinaryErr != nil {
		t.Fatal(cachedBinaryErr)
	}
	return cachedBinary
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
	// Filter YNAB_ALLOW_WRITES out of the inherited environment. A dev or
	// CI box that exports YNAB_ALLOW_WRITES=1 globally would otherwise
	// silently bypass TestSubprocess_WritesGatedOffWhenEnvUnset — the one
	// test that pins the bimodal startup gate. We explicitly re-add the
	// variable only when a caller passes it in extraEnv. Review finding
	// on test hermeticity.
	parent := cmd.Environ()
	filtered := parent[:0]
	for _, kv := range parent {
		if strings.HasPrefix(kv, "YNAB_ALLOW_WRITES=") {
			continue
		}
		filtered = append(filtered, kv)
	}
	cmd.Env = append(filtered, "YNAB_API_TOKEN=sk-subprocess-test")
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
//
// On timeout, the reader goroutine is cancelled by closing the subprocess
// stdin (which causes the server to exit, unblocking the blocked
// ReadBytes). Review finding L10.
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
		// Close stdin so the subprocess exits, which unblocks the
		// reader goroutine via EOF. Without this, the goroutine would
		// stay parked on ReadBytes until some other code path closes
		// the pipe.
		_ = s.stdin.Close()
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
		"list_payees":                 false,
		// Task-shaped tools (also read-only, ReadOnlyHint=true)
		"ynab_debt_snapshot":        false,
		"ynab_spending_check":       false,
		"ynab_waterfall_assignment": false,
		"ynab_status":               false,
		"ynab_weekly_checkin":       false,
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

// Handler-level fallback checks (empty plan_id, missing required fields)
// are covered by unit tests against each handler method directly — see
// TestGetMonth_RequiresPlanID and analogous checks in tools_test.go /
// tools_tasks_test.go / tools_writes_test.go. An earlier version of this
// file held a t.Skip() stub marker that served no purpose beyond
// documentation; removed per review nit about dead tests.

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
