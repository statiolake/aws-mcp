// Package caller defines the backend that performs a single AWS API call and
// an implementation that shells out to the AWS CLI v2.
//
// Everything above this package (the Starlark runtime, the MCP tools) talks
// only to the Caller interface, so the backend can be swapped (tests use
// caller.Func) without touching the scripting surface.
package caller

import (
	"context"
	"encoding/json"
	"fmt"
)

// Request is one AWS API call.
type Request struct {
	// Service is the boto3 client name ("ec2", "s3", "rds-data"). CLI command
	// names ("s3api", "configservice") are accepted too.
	Service string
	// Operation in any spelling: "DescribeInstances", "describe_instances" or
	// "describe-instances".
	Operation string
	// Params is the request as a JSON object in the API's own shape
	// (PascalCase keys as in boto3). Nil or empty means no parameters.
	Params json.RawMessage
	// Region and Profile override the backend's defaults when non-empty.
	Region  string
	Profile string
}

// Caller performs AWS API calls. Implementations must be safe for concurrent
// use. Call returns the response as a JSON object; for an operation with no
// output it returns "{}".
type Caller interface {
	Call(ctx context.Context, req *Request) (json.RawMessage, error)
}

// Func adapts a function to the Caller interface.
type Func func(ctx context.Context, req *Request) (json.RawMessage, error)

// Call implements Caller.
func (f Func) Call(ctx context.Context, req *Request) (json.RawMessage, error) { return f(ctx, req) }

// APIError is a failed call. Code is the AWS error code when the service
// returned one ("AccessDenied", "InvalidInstanceID.NotFound"); otherwise it is
// one of the local codes below.
type APIError struct {
	Service   string
	Operation string
	Code      string
	Message   string
}

// Local error codes, used when the failure did not come from AWS itself.
const (
	CodeCLI              = "CLIError"          // the CLI failed without an AWS error code
	CodeParamValidation  = "ParamValidation"   // the CLI rejected the parameters
	CodeTimeout          = "Timeout"           // the call exceeded its time limit
	CodeCallLimit        = "CallLimitExceeded" // the script used up its API call budget
	CodeNoCredentials    = "NoCredentials"     // the CLI found no credentials
	CodeInvalidOperation = "InvalidOperation"  // unknown service or operation
	CodeInvalidRequest   = "InvalidRequest"    // malformed request (bad JSON etc.)
)

func (e *APIError) Error() string {
	return fmt.Sprintf("%s (%s.%s): %s", e.Code, e.Service, e.Operation, e.Message)
}
