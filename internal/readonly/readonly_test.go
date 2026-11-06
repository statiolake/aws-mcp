package readonly

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/statiolake/aws-mcp/internal/caller"
)

func TestClassify(t *testing.T) {
	allowed := [][2]string{
		{"ec2", "DescribeInstances"}, {"ec2", "describe_instances"}, {"ec2", "describe-instances"},
		{"s3", "ListObjectsV2"}, {"s3api", "list_objects_v2"}, {"s3", "GetObject"}, {"s3", "HeadObject"},
		{"sts", "GetCallerIdentity"}, {"dynamodb", "Scan"}, {"dynamodb", "Query"}, {"dynamodb", "BatchGetItem"},
		{"cloudtrail", "LookupEvents"}, {"s3", "SelectObjectContent"}, {"cloudformation", "ValidateTemplate"},
		{"iam", "SimulatePrincipalPolicy"}, {"logs", "FilterLogEvents"}, {"logs", "StartQuery"},
		{"logs", "StopQuery"}, {"ce", "GetCostAndUsage"}, {"resourcegroupstaggingapi", "GetResources"},
		{"secretsmanager", "GetSecretValue"}, {"kendra", "Query"}, {"rds", "DescribeDBInstances"},
		{"ecs", "DescribeServices"}, {"sagemaker", "Search"}, {"ec2", "GetConsoleOutput"},
	}
	for _, c := range allowed {
		if d := Classify(c[0], c[1]); !d.Allowed {
			t.Errorf("%s %s refused: %s", c[0], c[1], d.Reason)
		}
	}
	refused := [][2]string{
		{"ec2", "TerminateInstances"}, {"ec2", "RunInstances"}, {"ec2", "run_instances"},
		{"s3", "PutObject"}, {"s3", "DeleteBucket"}, {"iam", "CreateUser"}, {"lambda", "Invoke"},
		{"dynamodb", "PutItem"}, {"dynamodb", "BatchWriteItem"}, {"dynamodb", "ExecuteStatement"},
		{"rds-data", "ExecuteStatement"}, {"athena", "StartQueryExecution"}, {"sqs", "ReceiveMessage"},
		{"elasticache", "TestFailover"}, {"apigateway", "TestInvokeMethod"}, {"kms", "Decrypt"},
		{"ssm", "SendCommand"}, {"cognito-identity", "GetId"}, {"ec2", "Getaway"}, {"ec2", "Listen"},
		{"ec2", "ModifyInstanceAttribute"}, {"ec2", ""}, {"stepfunctions", "StartExecution"},
	}
	for _, c := range refused {
		if d := Classify(c[0], c[1]); d.Allowed {
			t.Errorf("%s %s allowed: %s", c[0], c[1], d.Reason)
		}
	}
}

func TestGuard(t *testing.T) {
	calls := 0
	next := caller.Func(func(ctx context.Context, req *caller.Request) (json.RawMessage, error) {
		calls++
		return json.RawMessage(`{}`), nil
	})
	g := Guard{Next: next}
	if _, err := g.Call(context.Background(), &caller.Request{Service: "ec2", Operation: "DescribeVpcs"}); err != nil {
		t.Fatal(err)
	}
	_, err := g.Call(context.Background(), &caller.Request{Service: "ec2", Operation: "terminate_instances"})
	var ae *caller.APIError
	if !errors.As(err, &ae) || ae.Code != caller.CodeReadOnlyRefused || !strings.Contains(ae.Message, "--allow-writes") {
		t.Fatalf("err = %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d", calls)
	}
}
