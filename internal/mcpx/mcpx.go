// Package mcpx holds the small pieces shared by the aws-mcp and aws-rds-mcp
// commands: tool-result helpers, version reporting and environment-backed
// flag defaults.
package mcpx

import (
	"errors"
	"fmt"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/statiolake/aws-mcp/internal/caller"
)

// TextResult is a tool result with one text item.
func TextResult(s string, isErr bool) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}, IsError: isErr}
}

// ErrorText formats err for a tool result, with the AWS error code when
// there is one.
func ErrorText(err error) string {
	var ae *caller.APIError
	if errors.As(err, &ae) {
		return fmt.Sprintf("Error: %s (%s.%s): %s", ae.Code, ae.Service, ae.Operation, ae.Message)
	}
	return "Error: " + err.Error()
}

// LimitText cuts s to max bytes and says so.
func LimitText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("\n[output truncated: %d bytes, the limit is %d. Narrow the request (Filters, MaxResults) or use run_script to select fields.]", len(s), max)
}

// BuildVersion returns version unless it is "dev", in which case it prefers
// the module version recorded by `go install`.
func BuildVersion(version string) string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return version
}

// Env reads flag defaults from environment variables named Prefix+name.
// Unparsable values fall back to the default.
type Env struct {
	Prefix string
	Getenv func(string) string
}

// String returns the variable or def.
func (e Env) String(name, def string) string {
	if v := e.Getenv(e.Prefix + name); v != "" {
		return v
	}
	return def
}

// Bool returns the variable as a bool, or def.
func (e Env) Bool(name string, def bool) bool {
	if b, err := strconv.ParseBool(e.Getenv(e.Prefix + name)); err == nil {
		return b
	}
	return def
}

// Duration returns the variable as a time.Duration, or def.
func (e Env) Duration(name string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(e.Getenv(e.Prefix + name)); err == nil {
		return d
	}
	return def
}

// Int returns the variable as an int, or def.
func (e Env) Int(name string, def int) int {
	if n, err := strconv.Atoi(e.Getenv(e.Prefix + name)); err == nil {
		return n
	}
	return def
}
