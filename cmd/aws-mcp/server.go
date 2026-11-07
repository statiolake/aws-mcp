package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/statiolake/aws-mcp/internal/caller"
	"github.com/statiolake/aws-mcp/internal/mcpx"
	"github.com/statiolake/aws-mcp/internal/readonly"
	"github.com/statiolake/aws-mcp/internal/script"
)

// RunScriptInput is the input of run_script.
type RunScriptInput struct {
	Script  string `json:"script" jsonschema:"Starlark source code"`
	Region  string `json:"region,omitempty" jsonschema:"default region for aws.client() in this script"`
	Profile string `json:"profile,omitempty" jsonschema:"default AWS profile for aws.client() in this script"`
}

// CallAWSInput is the input of call_aws.
type CallAWSInput struct {
	Service   string         `json:"service" jsonschema:"boto3 client name, e.g. ec2, s3, iam, logs, rds"`
	Operation string         `json:"operation" jsonschema:"API operation, PascalCase or snake_case: DescribeInstances or describe_instances"`
	Params    map[string]any `json:"params,omitempty" jsonschema:"request parameters with the API's PascalCase names, as in boto3"`
	Region    string         `json:"region,omitempty" jsonschema:"region override"`
	Profile   string         `json:"profile,omitempty" jsonschema:"AWS profile override"`
}

func newServer(cfg *Config, backend caller.Caller) *mcp.Server {
	if backend == nil {
		backend = caller.NewCLI(caller.CLI{
			Path: cfg.AWSCLI, Region: cfg.Region, Profile: cfg.Profile, Timeout: cfg.CallTimeout,
			MaxConcurrent: cfg.Limits.MaxConcurrency, MaxBodyBytes: cfg.MaxBody,
		})
	}
	api := backend
	if cfg.ReadOnly {
		api = readonly.Guard{Next: backend}
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "aws-mcp", Version: buildVersion()}, &mcp.ServerOptions{
		Instructions: serverInstructions(cfg),
	})
	runner := &script.Runner{Caller: api, Limits: cfg.Limits}
	openWorld := true

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "run_script",
		Title:       "Run an AWS script (Starlark)",
		Description: runScriptDescription(cfg),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: cfg.ReadOnly, OpenWorldHint: &openWorld},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in RunScriptInput) (*mcp.CallToolResult, any, error) {
		res := runner.Run(ctx, in.Script, script.Env{Region: in.Region, Profile: in.Profile})
		return textResult(res.Text(), res.Err != nil), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "call_aws",
		Title:       "Call one AWS API operation",
		Description: callAWSDescription(cfg),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: cfg.ReadOnly, OpenWorldHint: &openWorld},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in CallAWSInput) (*mcp.CallToolResult, any, error) {
		var params json.RawMessage
		if in.Params != nil {
			b, err := json.Marshal(in.Params)
			if err != nil {
				return textResult("Error: "+err.Error(), true), nil, nil
			}
			params = b
		}
		if in.Service == "" || in.Operation == "" {
			return textResult("Error: service and operation are required", true), nil, nil
		}
		out, err := api.Call(ctx, &caller.Request{Service: in.Service, Operation: in.Operation, Params: params, Region: in.Region, Profile: in.Profile})
		if err != nil {
			return textResult(errorText(err), true), nil, nil
		}
		return textResult(limitText(string(out), cfg.Limits.MaxOutputBytes), false), nil, nil
	})

	return srv
}

var (
	textResult = mcpx.TextResult
	errorText  = mcpx.ErrorText
	limitText  = mcpx.LimitText
)

func readOnlyNote(cfg *Config) string {
	if !cfg.ReadOnly {
		return "Write operations are ENABLED on this server (--allow-writes); be careful with anything that changes or deletes resources."
	}
	return "This server is in read-only mode: only " + readonly.AllowedVerbs + " operations run; anything else fails with ReadOnlyRefused."
}

func serverInstructions(cfg *Config) string {
	s := "Call AWS APIs. Prefer run_script for anything beyond a single call: it runs a boto3-like Starlark script so you can combine calls, paginate, run calls concurrently and return only what matters. Use call_aws for one simple call."
	return s + " " + readOnlyNote(cfg)
}

func runScriptDescription(cfg *Config) string {
	l := cfg.Limits
	r := strings.NewReplacer(
		"{MAXPAGES}", fmt.Sprint(l.MaxPages),
		"{MAXCALLS}", fmt.Sprint(l.MaxCalls),
		"{TIMEOUT}", l.Timeout.String(),
		"{MAXOUTPUT}", fmt.Sprint(l.MaxOutputBytes),
		"{READONLY}", readOnlyNote(cfg),
	)
	return r.Replace(runScriptTemplate)
}

const runScriptTemplate = `Run a Starlark (Python-like) script that calls AWS with a boto3-style client. Use it to combine calls, filter and aggregate, so only the answer comes back.

API
- aws.client(service, region=None, profile=None) -> client. service is the boto3 name: "ec2", "s3", "iam", "logs", "rds", "lambda", ...
- client.<operation_in_snake_case>(**params) -> dict: the API response as dicts/lists. Params are keyword arguments with the API's PascalCase names, exactly as in boto3. Any operation of any service works.
- client.call("DescribeInstances", **params): the same, with the operation as a string.
- client.paginate("operation", **params) -> list of pages; follows NextToken/Marker/ContinuationToken/... automatically, up to {MAXPAGES} pages (pass _max_pages=N for more).
- aws.batch([(client, "operation", {params}), ...], return_errors=False) -> list of results in order, run concurrently. Use it for many regions/resources: every call costs ~0.5-1 s. With return_errors=True a failed item is {"Error": {"Code": ..., "Message": ...}} instead of failing the script.
- aws.try_call(fn, *args, **kwargs) -> (result, None) or (None, {"Code": ..., "Message": ...}). Starlark has no try/except; a failed call otherwise stops the script with the AWS error code and message.
- Also: json.encode/json.decode/json.indent, math, time, sum(). No files, network, imports or load().

Starlark vs Python: no try/except, classes, f-strings ("%s" % x or "{}".format(x) instead), imports, or generators; top-level for/if/while are fine. Timestamps come back as ISO-8601 strings.

Output: everything print()ed, then the value of the script's final expression (dicts/lists as JSON). Limits per script: {MAXCALLS} API calls (each page counts), {TIMEOUT} wall time, {MAXOUTPUT} bytes of output (the rest is cut, so print selected fields, not whole responses).

{READONLY}

Example: running instances
ec2 = aws.client("ec2", region="ap-northeast-1")
r = ec2.describe_instances(Filters=[{"Name": "instance-state-name", "Values": ["running"]}])
[(i["InstanceId"], i["InstanceType"]) for res in r["Reservations"] for i in res["Instances"]]

Example: several regions at once
regions = ["us-east-1", "eu-west-1", "ap-northeast-1"]
rs = aws.batch([(aws.client("lambda", region=r), "list_functions") for r in regions])
{r: [f["FunctionName"] for f in x["Functions"]] for r, x in zip(regions, rs)}

Example: pagination and error handling
s3 = aws.client("s3")
total = 0
for page in s3.paginate("list_objects_v2", Bucket="my-bucket", Prefix="logs/"):
    total += sum([o["Size"] for o in page.get("Contents", [])])
tags, err = aws.try_call(s3.get_bucket_tagging, Bucket="my-bucket")
{"bytes": total, "tags": tags["TagSet"] if tags else err["Code"]}`

func callAWSDescription(cfg *Config) string {
	return `Call one AWS API operation and return its JSON response.
- service: boto3 client name (ec2, s3, iam, logs, rds, ...)
- operation: API operation name, PascalCase or snake_case (DescribeInstances or describe_instances)
- params: request object with the API's PascalCase member names, as in boto3, e.g. {"InstanceIds": ["i-0123456789abcdef0"]}
Returns a single page (no automatic pagination: pass the returned NextToken/Marker back yourself). For several calls, pagination, filtering or aggregation use run_script instead.
` + readOnlyNote(cfg)
}
