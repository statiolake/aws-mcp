# Phase 2: a native backend

Phase 1 runs one AWS CLI v2 process per API call. That gives complete
coverage and the CLI's credential handling for free, but every call pays the
CLI's start-up (roughly 0.3–1 s and a short-lived Python process of a few tens
of MB). Phase 2 replaces the subprocess with an in-process client while keeping
everything above the backend unchanged.

## The seam

Everything talks to `caller.Caller` (`internal/caller/caller.go`):

```go
type Caller interface {
	Call(ctx context.Context, req *Request) (json.RawMessage, error)
}
```

`Request` carries the boto3 service name, the operation in any spelling, the
parameters as a JSON object in the API shape, and region/profile overrides.
The response is the JSON the CLI prints with `--output json`; failures are
`*caller.APIError` with the AWS error code. A native backend must produce the
same JSON (ISO-8601 timestamps, base64 blobs, the API's member names) so that
scripts, `readonly.Guard` and the tools need no change.

## Native client outline

1. **Models.** Use the botocore JSON models (`service-2.json`,
   `paginators-1.json`, `endpoint-rule-set-1.json`; Apache-2.0 data). Do not
   embed all of them (hundreds of MB uncompressed): load a service's model on
   first use from a cache directory (populated from the installed AWS CLI's
   bundled models or downloaded once), parse only the operations used, and keep
   a small LRU so idle memory stays near today's ~10 MB.
2. **Serialisation** per protocol, driven by the model's shapes: `json`
   (awsJson1_0/1_1), `rest-json`, `rest-xml`, `query`, `ec2`, and
   `smithy-rpc-v2-cbor`. Responses are decoded back into the CLI's JSON form.
3. **Signing and credentials.** SigV4 via `aws-sdk-go-v2/aws/signer/v4`;
   credentials via `aws-sdk-go-v2/config` (env, shared config/credentials,
   SSO, process credentials, web identity, ECS/IMDS). Cache credentials per
   profile.
4. **Endpoints.** Evaluate the endpoint rule sets (or start with
   `{endpointPrefix}.{region}.{dnsSuffix}` plus the global-service exceptions
   and S3 virtual-hosted addressing, falling back to the CLI otherwise).
5. **Retries** in standard mode (throttling and transient errors, jittered
   backoff), honouring the context deadline.
6. **Pagination** from `paginators-1.json`, replacing the token-name heuristic
   in `internal/script/paginate.go` (keep the heuristic for unknown cases).

## Rollout

- `--backend=cli|native|auto` (default `auto`): `auto` uses the native client
  for services/protocols it supports and falls back to the CLI for the rest
  (event streams, unusual customisations), so coverage never drops.
- Parity tests: for a corpus of operations, compare the native serialiser's
  HTTP request with the one the CLI sends (`aws --debug`), and decode recorded
  responses through both paths; run against the fake CLI in CI.
- Measure: calls/second in `aws.batch`, idle and peak RSS.
