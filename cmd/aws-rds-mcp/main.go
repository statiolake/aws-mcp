// Command aws-rds-mcp is a small MCP server (stdio) that runs read-only SQL
// on Aurora / RDS through the RDS Data API, using the AWS CLI v2.
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
	cfg, showVersion, err := parseConfig(args, os.Getenv, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "aws-rds-mcp:", err)
		return 2
	}
	if showVersion {
		fmt.Println(mcpx.BuildVersion(version))
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := newServer(cfg, nil)
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "aws-rds-mcp:", err)
		return 1
	}
	return 0
}
