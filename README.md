# aws-mcp

A small MCP server that lets LLM agents call **any AWS API** — one call at a
time, or from a sandboxed, Python-like script with a boto3-style client.

It is a lightweight alternative to the Python AWS API MCP server from awslabs,
which uses around 700 MB per process. `aws-mcp` is a single static Go binary
that idles at about **9.4 MB RSS**.

```python
ec2 = aws.client("ec2", region="ap-northeast-1")
r = ec2.describe_instances(Filters=[{"Name": "instance-state-name", "Values": ["running"]}])
[(i["InstanceId"], i["InstanceType"]) for res in r["Reservations"] for i in res["Instances"]]
```

## Why

Agents mostly want to *run a small program* against AWS: list things across
regions, join two APIs, filter, count, and hand back only the answer. Doing
that through one-call-per-tool-invocation wastes context and turns; running
the agent's code in a full Python interpreter is heavy and not hermetic.

`aws-mcp` gives the agent [Starlark](https://github.com/google/starlark-go) —
Python syntax, deterministic, no file or network access — plus an `aws`
module that mirrors boto3 closely enough that code LLMs already know works.

Calls go through the **AWS CLI v2** (`aws <service> <operation>
--cli-input-json ...`). Every CLI operation accepts `--cli-input-json`, so every
operation of every service is reachable, and credentials (profiles, SSO,
environment, IMDS, ...) are resolved by the CLI exactly as on your terminal.

## Footprint

Measured on Linux x86-64 (`/proc/<pid>/status`), Go 1.25, `CGO_ENABLED=0`
build, after `initialize` and `tools/list`:

| | aws-mcp | awslabs AWS API MCP server |
|---|---|---|
| Idle RSS | **9.4 MB** (VmRSS 9596 kB) | ~700 MB |
| After a 100 000-element script | 19.5 MB | – |
| Distribution | one 8.4 MB static binary | Python package + dependencies |

Each AWS call additionally starts a short-lived `aws` CLI process while it
runs (the CLI is itself a bundled Python program); it exits when the call
returns, so it does not add to the idle footprint. Calls run concurrently
up to `--concurrency` (default 8).

## Install

Requirements: the [AWS CLI v2](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html)
on `PATH` (or pass `--aws-cli PATH`) and Go 1.25+ to build (an older `go`
fetches the right toolchain automatically).

```sh
CGO_ENABLED=0 go install github.com/statiolake/aws-mcp/cmd/aws-mcp@latest
```

`CGO_ENABLED=0` produces a fully static binary; without it the binary links
libc dynamically on Linux, which also works.

## Client configuration

### Claude Code

```sh
claude mcp add aws -- aws-mcp --region ap-northeast-1
# with a profile, shared with the project via .mcp.json:
claude mcp add --scope project aws -e AWS_PROFILE=dev -- aws-mcp --region ap-northeast-1
```

or write `.mcp.json` yourself:

```json
{
  "mcpServers": {
    "aws": {
      "type": "stdio",
      "command": "aws-mcp",
      "args": ["--region", "ap-northeast-1"],
      "env": { "AWS_PROFILE": "dev" }
    }
  }
}
```

### Codex

`~/.codex/config.toml`:

```toml
[mcp_servers.aws]
command = "aws-mcp"
args = ["--region", "ap-northeast-1"]
env = { AWS_PROFILE = "dev" }
```

## Tools

### `run_script(script, region?, profile?)`

Runs a Starlark script. The scripting API:

| | |
|---|---|
| `aws.client(service, region=None, profile=None)` | a client; `service` is the boto3 name (`"ec2"`, `"s3"`, `"logs"`, ...) |
| `client.describe_instances(**params)` | any operation in snake_case; keyword arguments are the API's PascalCase parameters, as in boto3; returns the response as dicts/lists |
| `client.call("DescribeInstances", **params)` | the same with the operation name as a string (any spelling) |
| `client.paginate("list_objects_v2", **params)` | list of pages, following `NextToken` / `Marker` / `ContinuationToken` / `LastEvaluatedKey` / ...; `_max_pages=N`, and `_input_token=` / `_output_token=` for unusual APIs |
| `aws.batch([(client, "op", {params}), ...], return_errors=False)` | runs calls concurrently and returns their results in order; with `return_errors=True` failures become `{"Error": {"Code", "Message"}}` |
| `aws.try_call(fn, *args, **kwargs)` | `(result, None)` or `(None, {"Code", "Message", ...})` — Starlark has no `try`/`except` |
| `json`, `math`, `time`, `sum()` | Starlark's standard modules; no `load()`, files or network |

A failed call stops the script with the AWS error code and message
(`InvalidInstanceID.NotFound (ec2.DescribeInstances): ...`) and a backtrace.
The tool returns everything printed plus the value of the final expression
(dicts and lists as compact JSON).

Limits per script (all configurable): 10 000 000 execution steps, 2 minutes of
wall time, 100 000 bytes of output (the rest is cut with a notice), 100 AWS
calls (each page counts).

### `call_aws(service, operation, params?, region?, profile?)`

One call, JSON in and out:
`{"service": "ec2", "operation": "DescribeVpcs", "params": {"VpcIds": ["vpc-1"]}}`.
Returns a single page.

### `rds_query(resource_arn, secret_arn, database, sql, parameters?, region?, profile?)`

Read-only SQL on Aurora / RDS through the
[RDS Data API](https://docs.aws.amazon.com/AmazonRDS/latest/AuroraUserGuide/data-api.html),
authenticating with the Secrets Manager secret you pass — intended for a
read-only database user's secret. Two protections:

1. Only a single read statement (`SELECT`, `WITH`, `SHOW`, `EXPLAIN`,
   `DESCRIBE`) without write keywords is accepted; the check skips literals and
   comments and is applied with both standard and backslash string escaping.
2. The statement runs inside a Data API transaction (made `READ ONLY` where
   the engine supports it, i.e. PostgreSQL) that is **always rolled back**.

`parameters` fills `:name` placeholders: `{"id": 42}`. The result is
`{"columns", "records", "numberOfRecords", "rolledBack"}`. Disable the tool
with `--no-rds`.

## Read-only mode

On by default. `run_script` and `call_aws` only run operations whose name
starts with a read verb — `Describe`, `List`, `Get`, `Head`, `Search`, `Scan`,
`Query`, `BatchGet`, `BatchDescribe`, `Lookup`, `Select`, `Validate`,
`Estimate`, `Preview`, `Test`, `Simulate`, `Filter` — plus an allowlist
(CloudWatch Logs `StartQuery`/`StopQuery`), minus a denylist of operations
whose names lie (`elasticache TestFailover`, `apigateway TestInvokeMethod`,
`cognito-identity GetId`, ...). Everything else fails with `ReadOnlyRefused`
before any process is started. The classifier is a pure function
(`internal/readonly`).

Start the server with `--allow-writes` (or `--read-only=false`) to allow all
operations. Read-only mode is a guard against mistakes, not a security
boundary: `Get*` includes things like `secretsmanager GetSecretValue`. Use IAM
(a read-only role or profile) for real enforcement.

## Configuration

Every flag has an environment variable default.

| Flag | Env | Default | |
|---|---|---|---|
| `--region` | `AWS_MCP_REGION` | CLI's own | default region |
| `--profile` | `AWS_MCP_PROFILE` | CLI's own | default profile |
| `--aws-cli` | `AWS_MCP_AWS_CLI` | `aws` | path to the AWS CLI v2 |
| `--read-only` / `--allow-writes` | `AWS_MCP_READ_ONLY` / `AWS_MCP_ALLOW_WRITES` | read-only | |
| `--no-rds` | `AWS_MCP_NO_RDS` | off | drop `rds_query` |
| `--call-timeout` | `AWS_MCP_CALL_TIMEOUT` | `1m` | per AWS call |
| `--script-timeout` | `AWS_MCP_SCRIPT_TIMEOUT` | `2m` | per script |
| `--max-steps` | `AWS_MCP_MAX_STEPS` | 10000000 | Starlark steps per script |
| `--max-output` | `AWS_MCP_MAX_OUTPUT` | 100000 | bytes per tool result |
| `--max-calls` | `AWS_MCP_MAX_CALLS` | 100 | AWS calls per script |
| `--concurrency` | `AWS_MCP_CONCURRENCY` | 8 | parallel CLI processes |
| `--max-pages` | `AWS_MCP_MAX_PAGES` | 20 | default `paginate` page limit |
| `--max-body` | `AWS_MCP_MAX_BODY` | 1048576 | bytes of an S3 `GetObject` body returned |

Region and profile resolve as: per-client argument > tool argument > server
flag > the CLI's own resolution (`AWS_REGION`, `AWS_PROFILE`, config files).

## Examples

Count running instances per region, all regions at once:

```python
regions = [r["RegionName"] for r in aws.client("ec2").describe_regions()["Regions"]]
rs = aws.batch([(aws.client("ec2", region=r), "describe_instances",
                 {"Filters": [{"Name": "instance-state-name", "Values": ["running"]}]})
                for r in regions], return_errors=True)
{r: sum([len(x["Instances"]) for x in res.get("Reservations", [])]) for r, res in zip(regions, rs)}
```

IAM users without MFA:

```python
iam = aws.client("iam")
users = [u for p in iam.paginate("list_users") for u in p["Users"]]
mfa = aws.batch([(iam, "list_mfa_devices", {"UserName": u["UserName"]}) for u in users])
[u["UserName"] for u, m in zip(users, mfa) if not m["MFADevices"]]
```

Read a small object from S3 (streamed bodies are returned as `Body`):

```python
aws.client("s3").get_object(Bucket="my-bucket", Key="config.json")["Body"]
```

## Limitations

- Each call costs one CLI start-up (~0.3–1 s); use `aws.batch` to overlap them.
- `paginate` recognises continuation tokens by name (botocore's paginator
  models are not visible through the CLI); pass `_input_token`/`_output_token`
  for APIs it does not know.
- Streaming outputs are supported for `s3 GetObject` and `lambda Invoke`; blob
  inputs take raw strings (`--cli-binary-format raw-in-base64-out`);
  event-stream operations are not supported.
- Requires AWS CLI **v2**.

## Development

```sh
go vet ./... && go test ./...
```

Tests never touch AWS: the test binary doubles as a fake `aws` executable
(`internal/fakeaws`) that answers from canned rules and logs each invocation.

Layout:

```
cmd/aws-mcp/        server, flags, tool definitions
internal/caller/    Caller interface + AWS CLI backend
internal/script/    Starlark runtime and the aws module
internal/readonly/  read-only classifier and Caller guard
internal/rdsdata/   rds_query: SQL guard + rolled-back Data API transaction
internal/awsname/   boto3 / API / CLI name conversion
internal/fakeaws/   fake aws executable for tests
```

## License

MIT. This is an independent, clean-room implementation; it contains no code
from awslabs/mcp.
