// Command aws-mcp is a small MCP server (stdio) that lets LLM agents call any
// AWS API, either one call at a time or from a sandboxed Starlark script.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/statiolake/aws-mcp/internal/mcpx"
)

var version = "dev"

func main() {
	os.Exit(realMain(os.Args[1:]))
}

func realMain(args []string) int {
	cfg, showVersion, err := parseConfig(args, defaultGetenv, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "aws-mcp:", err)
		return 2
	}
	if showVersion {
		fmt.Println(buildVersion())
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := newServer(cfg, nil)
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "aws-mcp:", err)
		return 1
	}
	return 0
}

func buildVersion() string { return mcpx.BuildVersion(version) }
