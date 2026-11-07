package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/statiolake/aws-mcp/internal/caller"
	"github.com/statiolake/aws-mcp/internal/fakeaws"
)

func TestMain(m *testing.M) {
	if os.Getenv("AWS_RDS_MCP_TEST_SERVE") == "1" {
		os.Unsetenv("AWS_RDS_MCP_TEST_SERVE")
		os.Setenv("FAKE_AWS", "1")
		os.Exit(realMain(strings.Fields(os.Getenv("AWS_RDS_MCP_TEST_ARGS"))))
	}
	fakeaws.MaybeRun()
	os.Exit(m.Run())
}

var fakeRules = []fakeaws.Rule{
	{Service: "rds-data", Operation: "begin-transaction", Stdout: map[string]any{"transactionId": "tx"}},
	{Service: "rds-data", Operation: "execute-statement", Match: map[string]any{"sql": "SET TRANSACTION READ ONLY"}, Stdout: map[string]any{}},
	{Service: "rds-data", Operation: "execute-statement", Stdout: map[string]any{
		"columnMetadata": []any{map[string]any{"label": "one", "typeName": "int4"}}, "formattedRecords": `[{"one":1}]`}},
	{Service: "rds-data", Operation: "rollback-transaction", Stdout: map[string]any{"transactionStatus": "Rollback Complete"}},
}

func TestParseConfig(t *testing.T) {
	cfg, _, err := parseConfig(nil, func(string) string { return "" }, io.Discard)
	if err != nil || cfg.AWSCLI != "aws" || cfg.MaxOutputBytes != 100000 {
		t.Fatalf("defaults = %+v, %v", cfg, err)
	}
	env := map[string]string{"AWS_RDS_MCP_REGION": "eu-west-1", "AWS_RDS_MCP_CALL_TIMEOUT": "5s"}
	cfg, _, err = parseConfig([]string{"--profile", "ro"}, func(k string) string { return env[k] }, io.Discard)
	if err != nil || cfg.Region != "eu-west-1" || cfg.Profile != "ro" || cfg.CallTimeout != 5*time.Second {
		t.Fatalf("cfg = %+v, %v", cfg, err)
	}
	if _, _, err := parseConfig([]string{"--no-rds"}, func(string) string { return "" }, io.Discard); err == nil {
		t.Fatal("unknown flag accepted")
	}
}

func TestRDSQueryTool(t *testing.T) {
	f := fakeaws.New(t, fakeRules)
	cfg, _, _ := parseConfig(nil, func(string) string { return "" }, io.Discard)
	backend := caller.NewCLI(caller.CLI{Path: f.Path, Env: f.Env, Timeout: 10 * time.Second})
	srv := newServer(cfg, backend)
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != "rds_query" {
		t.Fatalf("tools = %+v, %v", tools, err)
	}
	d := tools.Tools[0].Description
	if !strings.Contains(d, "rolled back") || !strings.Contains(d, "single read statement") {
		t.Errorf("rds_query description lacks the protections")
	}

	call := func(args map[string]any) (string, bool) {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "rds_query", Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return res.Content[0].(*mcp.TextContent).Text, res.IsError
	}
	args := map[string]any{"resource_arn": "arn:aws:rds:us-east-1:1:cluster:c", "secret_arn": "arn:aws:secretsmanager:us-east-1:1:secret:ro",
		"database": "app", "sql": "SELECT 1 AS one"}
	out, isErr := call(args)
	if isErr || !strings.Contains(out, `"records":[{"one":1}]`) || !strings.Contains(out, `"rolledBack":true`) {
		t.Fatalf("out = %q", out)
	}
	if inv := f.Invocations(); len(inv) != 4 || inv[3].Operation != "rollback-transaction" {
		t.Fatalf("invocations = %+v", inv)
	}
	args["sql"] = "DELETE FROM users"
	out, isErr = call(args)
	if !isErr || !strings.Contains(out, "refused") {
		t.Fatalf("out = %q", out)
	}
	if len(f.Invocations()) != 4 {
		t.Fatal("refused SQL reached the CLI")
	}
}

// TestStdio starts the real server binary over stdio.
func TestStdio(t *testing.T) {
	f := fakeaws.New(t, fakeRules)
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "AWS_RDS_MCP_TEST_SERVE=1", "AWS_RDS_MCP_TEST_ARGS=--aws-cli "+f.Path)
	cmd.Env = append(cmd.Env, f.Env[1:]...)
	cmd.Stderr = os.Stderr
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "rds_query", Arguments: map[string]any{
		"resource_arn": "arn:c", "secret_arn": "arn:s", "database": "app", "sql": "SELECT 1"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, `"rolledBack":true`) {
		t.Fatalf("result = %+v", res.Content[0])
	}
}
