// Package readonly decides whether an AWS API operation is safe to run when
// the server is in read-only mode, and provides a Caller wrapper that refuses
// everything else.
package readonly

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/statiolake/aws-mcp/internal/awsname"
	"github.com/statiolake/aws-mcp/internal/caller"
)

// readPrefixes are the leading words (in snake_case) of operations that only
// read. A prefix must cover whole words: "get" matches GetObject but not a
// hypothetical "Getaway".
var readPrefixes = [][]string{
	{"describe"}, {"list"}, {"get"}, {"head"}, {"search"}, {"scan"}, {"query"},
	{"batch", "get"}, {"batch", "describe"}, {"lookup"}, {"select"}, {"validate"},
	{"estimate"}, {"preview"}, {"test"}, {"simulate"}, {"filter"},
}

// allow lists read-only operations whose names do not start with a read
// prefix. Keys are "<cli-service>:<snake_operation>".
var allow = map[string]bool{
	"sts:get_caller_identity":     true, // also covered by "get"; kept explicit
	"logs:start_query":            true, // CloudWatch Logs Insights query
	"logs:stop_query":             true,
	"s3api:select_object_content": true,
}

// deny lists operations that match a read prefix but have side effects.
var deny = map[string]bool{
	"elasticache:test_failover":                                 true, // really fails over a node group
	"elasticache:test_migration":                                true,
	"apigateway:test_invoke_method":                             true, // invokes the integration
	"apigateway:test_invoke_authorizer":                         true, // invokes the authorizer Lambda
	"iot:test_invoke_authorizer":                                true,
	"cognito-idp:get_user_attribute_verification_code":          true, // sends a message
	"cognito-identity:get_id":                                   true, // creates an identity
	"cognito-identity:get_open_id_token_for_developer_identity": true,
}

// Decision is the result of classifying an operation.
type Decision struct {
	Allowed bool
	Reason  string
}

// Classify reports whether service:operation may run in read-only mode.
// Service may be a boto3 or CLI name; operation may be in any spelling. It is
// a pure function.
func Classify(service, operation string) Decision {
	svc := awsname.CanonicalService(service)
	words := awsname.Words(operation)
	key := svc + ":" + strings.Join(words, "_")
	if deny[key] {
		return Decision{false, "has side effects despite its name (explicit denylist)"}
	}
	if allow[key] {
		return Decision{true, "explicit allowlist"}
	}
	for _, p := range readPrefixes {
		if len(words) >= len(p) && equalWords(words[:len(p)], p) {
			return Decision{true, "read-only prefix " + strings.Join(p, "_")}
		}
	}
	return Decision{false, "operation name does not start with a read-only verb"}
}

func equalWords(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// AllowedVerbs is a human-readable list of what read-only mode permits.
const AllowedVerbs = "Describe*, List*, Get*, Head*, Search*, Scan, Query, BatchGet*, BatchDescribe*, Lookup*, Select*, Validate*, Estimate*, Preview*, Test*, Simulate*, Filter*, plus logs StartQuery/StopQuery"

// RefusalMessage explains a refused operation.
func RefusalMessage(service, operation string) string {
	return fmt.Sprintf("%s %s refused: the server runs in read-only mode and only allows %s (%s). Restart the server with --allow-writes to permit writes.",
		service, awsname.PascalOperation(operation), AllowedVerbs, Classify(service, operation).Reason)
}

// Guard wraps a Caller and refuses non-read-only operations.
type Guard struct {
	Next caller.Caller
}

// Call implements caller.Caller.
func (g Guard) Call(ctx context.Context, req *caller.Request) (json.RawMessage, error) {
	if d := Classify(req.Service, req.Operation); !d.Allowed {
		return nil, &caller.APIError{
			Service:   req.Service,
			Operation: awsname.PascalOperation(req.Operation),
			Code:      caller.CodeReadOnlyRefused,
			Message:   RefusalMessage(req.Service, req.Operation),
		}
	}
	return g.Next.Call(ctx, req)
}
