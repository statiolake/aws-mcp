package main

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/statiolake/aws-mcp/internal/caller"
	"github.com/statiolake/aws-mcp/internal/mcpx"
	"github.com/statiolake/aws-mcp/internal/rdsdata"
)

// RDSQueryInput is the input of rds_query.
type RDSQueryInput struct {
	ResourceARN string `json:"resource_arn" jsonschema:"ARN of the Aurora/RDS cluster (Data API must be enabled)"`
	SecretARN   string `json:"secret_arn" jsonschema:"ARN of the Secrets Manager secret of a (read-only) database user"`
	Database    string `json:"database" jsonschema:"database name"`
	SQL         string `json:"sql" jsonschema:"one read statement: SELECT, WITH, SHOW, EXPLAIN or DESCRIBE; use :name placeholders"`
	Parameters  any    `json:"parameters,omitempty" jsonschema:"values for :name placeholders as {name: value}, or a raw Data API parameter list"`
	Region      string `json:"region,omitempty" jsonschema:"region override"`
	Profile     string `json:"profile,omitempty" jsonschema:"AWS profile override"`
}

func newServer(cfg *Config, backend caller.Caller) *mcp.Server {
	if backend == nil {
		backend = caller.NewCLI(caller.CLI{Path: cfg.AWSCLI, Region: cfg.Region, Profile: cfg.Profile, Timeout: cfg.CallTimeout})
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "aws-rds-mcp", Version: mcpx.BuildVersion(version)}, &mcp.ServerOptions{
		Instructions: "Use rds_query for read-only SQL on Aurora/RDS clusters through the RDS Data API. Statements always run in a transaction that is rolled back.",
	})
	rds := &rdsdata.Runner{Caller: backend, SetReadOnlyTxn: true}
	openWorld := true
	notDestructive := false
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "rds_query",
		Title:       "Read-only SQL via the RDS Data API",
		Description: rdsQueryDescription,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &notDestructive, OpenWorldHint: &openWorld},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in RDSQueryInput) (*mcp.CallToolResult, any, error) {
		var params json.RawMessage
		if in.Parameters != nil {
			b, err := json.Marshal(in.Parameters)
			if err != nil {
				return mcpx.TextResult("Error: "+err.Error(), true), nil, nil
			}
			params = b
		}
		res, err := rds.Run(ctx, rdsdata.Query{
			ResourceARN: in.ResourceARN, SecretARN: in.SecretARN, Database: in.Database,
			SQL: in.SQL, Parameters: params, Region: in.Region, Profile: in.Profile,
		})
		if err != nil {
			msg := mcpx.ErrorText(err)
			if res != nil && !res.RolledBack && res.RollbackError != "" {
				msg += "\nRollback failed: " + res.RollbackError
			}
			return mcpx.TextResult(msg, true), nil, nil
		}
		b, _ := json.Marshal(res)
		return mcpx.TextResult(mcpx.LimitText(string(b), cfg.MaxOutputBytes), false), nil, nil
	})
	return srv
}

const rdsQueryDescription = `Run one read-only SQL statement on an Aurora / RDS cluster through the RDS Data API (the Data API must be enabled on the cluster). It authenticates with the Secrets Manager secret you pass: use a read-only database user's secret.

Two protections make it read-only:
1. Only a single read statement is accepted: SELECT, WITH, SHOW, EXPLAIN or DESCRIBE, without write keywords (INSERT, UPDATE, DELETE, INTO, CREATE, DROP, ...) and without a second statement; anything else is refused before it reaches the database.
2. The statement runs inside a Data API transaction that is always rolled back (and is set READ ONLY where the engine supports it), so nothing it does is ever committed.

parameters: values for :name placeholders, e.g. sql "SELECT * FROM orders WHERE id = :id" with parameters {"id": 42}.
Returns {"columns": [{"name", "typeName"}], "records": [{column: value}, ...], "numberOfRecords": n, "rolledBack": true}. Add LIMIT to large queries.`
