// SPDX-License-Identifier: MIT
//
// mcp-ynab is a read-only Model Context Protocol server for the YNAB
// budgeting API. It exposes 5 tools that an LLM can call to inspect plans,
// accounts, categories, transactions, and monthly summaries.
//
// Subcommands:
//
//	mcp-ynab              — run the MCP server over stdio (default)
//	mcp-ynab store-token  — read a token from stdin and store it in the
//	                        OS keyring for future sessions
//	mcp-ynab version      — print the version and exit
//
// Security posture:
//   - Read-only by default; write tools require YNAB_ALLOW_WRITES=1.
//   - All outbound HTTP is pinned to api.ynab.com by a custom RoundTripper.
//   - Token is wrapped in a redacting type; all formatting paths emit
//     [REDACTED]. It is accessible only via a package-private reveal().
//   - A per-Client rate limiter caps us at ~180 req/hr (YNAB allows
//     200/hr). Since mcp-ynab runs one Client per process with one
//     token, this is effectively per-token.
//   - All error strings are sanitized to strip Bearer tokens.
//   - Stdio-only transport; no inbound network surface.
//
// See README.md for install and configuration.

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Version is the server version reported to MCP clients and embedded in the
// outbound User-Agent header. Overridden at build time via -ldflags.
var Version = "0.1.0"

func main() {
	// CRITICAL: MCP stdio transport uses stdout for JSON-RPC framing. Any
	// stray write to stdout corrupts the protocol. Redirect the standard
	// logger to stderr before anything else runs, and never write to stdout
	// from this program's own code.
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	// Subcommand dispatch. The default (no args) is the MCP server.
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "store-token":
			if err := storeTokenFromStdin(); err != nil {
				log.Printf("store-token: %v", sanitize(err.Error()))
				os.Exit(1)
			}
			log.Printf("token stored in OS keyring")
			return
		case "version":
			fmt.Fprintln(os.Stderr, "mcp-ynab", Version)
			return
		case "-h", "--help", "help":
			printUsage()
			return
		default:
			log.Printf("unknown subcommand: %q", args[0]) // #nosec G706 -- %q escapes the value; no injection risk
			printUsage()
			os.Exit(2)
		}
	}

	if err := run(); err != nil {
		// A clean SIGTERM/SIGINT shutdown cancels the server context and
		// the SDK's StdioTransport.Run returns context.Canceled. That's
		// the normal "the operator asked us to stop" exit — not a fatal
		// error. Exit 0 without a scary "fatal: context canceled" line.
		// Review finding on main.go:76 SIGTERM exit.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			log.Printf("mcp-ynab shutting down: %v", err)
			return
		}
		log.Printf("fatal: %v", sanitize(err.Error()))
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `mcp-ynab — read-only YNAB Model Context Protocol server

Usage:
  mcp-ynab               Run the MCP server on stdio (default)
  mcp-ynab store-token   Read a token from stdin and save it to the OS keyring
  mcp-ynab version       Print version and exit

Token resolution order:
  1. YNAB_API_TOKEN           (environment variable, raw value)
  2. YNAB_API_TOKEN_FILE      (path to file containing the token)
  3. OS keyring               (service "mcp-ynab", user "default")

Example — store a token in the keyring:
  echo -n "your-ynab-personal-access-token" | mcp-ynab store-token`)
}

func run() error {
	token, err := loadToken()
	if err != nil {
		return fmt.Errorf("load token: %w", err)
	}

	client, err := NewClient(token)
	if err != nil {
		return fmt.Errorf("build ynab client: %w", err)
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "mcp-ynab",
		Version: Version,
		Title:   "YNAB (read-only)",
	}, nil)

	registerTools(server, client)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Printf("mcp-ynab %s starting on stdio", Version)
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("server run: %w", err)
	}
	return nil
}
