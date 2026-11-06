package caller

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/statiolake/aws-mcp/internal/fakeaws"
)

func TestMain(m *testing.M) {
	fakeaws.MaybeRun()
	os.Exit(m.Run())
}

func newCLI(f *fakeaws.Fake, mod func(*CLI)) *CLI {
	c := CLI{Path: f.Path, Env: f.Env}
	if mod != nil {
		mod(&c)
	}
	return NewCLI(c)
}

func TestArgs(t *testing.T) {
	c := NewCLI(CLI{Region: "us-east-1", Profile: "dev"})
	got := c.Args(&Request{Service: "s3", Operation: "ListObjectsV2", Region: "ap-northeast-1"}, `{"Bucket":"b"}`)
	want := []string{"s3api", "list-objects-v2", "--cli-input-json", `{"Bucket":"b"}`, "--output", "json",
		"--no-paginate", "--no-cli-pager", "--cli-binary-format", "raw-in-base64-out",
		"--region", "ap-northeast-1", "--profile", "dev"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Args =\n%q\nwant\n%q", got, want)
	}
	c = NewCLI(CLI{})
	got = c.Args(&Request{Service: "ec2", Operation: "describe_instances"}, `{}`)
	if strings.Contains(strings.Join(got, " "), "--region") || strings.Contains(strings.Join(got, " "), "--profile") {
		t.Fatalf("unexpected region/profile: %q", got)
	}
}

func TestCallPassesParams(t *testing.T) {
	f := fakeaws.New(t, []fakeaws.Rule{{Service: "ec2", Operation: "describe-instances", Echo: true}})
	c := newCLI(f, func(c *CLI) { c.Region = "eu-west-1" })
	params := `{"Filters":[{"Name":"instance-state-name","Values":["running"]}],"MaxResults":5}`
	out, err := c.Call(context.Background(), &Request{Service: "ec2", Operation: "DescribeInstances", Params: json.RawMessage(params), Profile: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Input   map[string]any
		Region  string
		Profile string
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Region != "eu-west-1" || got.Profile != "p1" {
		t.Fatalf("region/profile = %q/%q", got.Region, got.Profile)
	}
	if got.Input["MaxResults"] != float64(5) {
		t.Fatalf("input = %v", got.Input)
	}
	// Output is compacted.
	if strings.Contains(string(out), "\n") {
		t.Fatalf("output not compact: %s", out)
	}
}

func TestCallLargeParamsUseFile(t *testing.T) {
	f := fakeaws.New(t, []fakeaws.Rule{{Service: "sqs", Operation: "send-message", Echo: true}})
	c := newCLI(f, nil)
	big := strings.Repeat("x", inlineParamLimit+10)
	p, _ := json.Marshal(map[string]string{"MessageBody": big})
	if _, err := c.Call(context.Background(), &Request{Service: "sqs", Operation: "SendMessage", Params: p}); err != nil {
		t.Fatal(err)
	}
	inv := f.Invocations()
	if len(inv) != 1 || inv[0].Input["MessageBody"] != big {
		t.Fatalf("large input not delivered")
	}
	for i, a := range inv[0].Args {
		if a == "--cli-input-json" && !strings.HasPrefix(inv[0].Args[i+1], "file://") {
			t.Fatalf("large input passed inline")
		}
	}
}

func TestCallEmptyOutput(t *testing.T) {
	f := fakeaws.New(t, []fakeaws.Rule{{Service: "s3api", Operation: "delete-object"}})
	out, err := newCLI(f, nil).Call(context.Background(), &Request{Service: "s3", Operation: "delete_object", Params: json.RawMessage(`{"Bucket":"b","Key":"k"}`)})
	if err != nil || string(out) != "{}" {
		t.Fatalf("out=%s err=%v", out, err)
	}
}

func TestCallErrors(t *testing.T) {
	f := fakeaws.New(t, []fakeaws.Rule{
		{Service: "ec2", Operation: "describe-instances", Exit: 254,
			Stderr: "\nAn error occurred (InvalidInstanceID.NotFound) when calling the DescribeInstances operation: The instance ID 'i-1' does not exist\n"},
		{Service: "iam", Operation: "list-users", Exit: 253, Stderr: "\nUnable to locate credentials. You can configure credentials by running \"aws configure\".\n"},
		{Service: "sts", Operation: "get-caller-identity", Exit: 254,
			Stderr: "\nAn error occurred (Throttling) when calling the GetCallerIdentity operation (reached max retries: 2): Rate exceeded\n"},
		{Service: "ec2", Operation: "describe-vpcs", Exit: 252, Stderr: "\nParameter validation failed:\nUnknown parameter in input: \"Foo\"\n"},
	})
	c := newCLI(f, nil)
	cases := []struct {
		svc, op, code, msg string
	}{
		{"ec2", "DescribeInstances", "InvalidInstanceID.NotFound", "The instance ID 'i-1' does not exist"},
		{"iam", "ListUsers", CodeNoCredentials, "Unable to locate credentials"},
		{"sts", "GetCallerIdentity", "Throttling", "Rate exceeded"},
		{"ec2", "DescribeVpcs", CodeParamValidation, "Unknown parameter"},
		{"ec2", "NoSuchThing", CodeInvalidOperation, "unknown service or operation"},
		{"ec2", "bad name!", CodeInvalidOperation, "invalid service or operation"},
	}
	for _, tc := range cases {
		_, err := c.Call(context.Background(), &Request{Service: tc.svc, Operation: tc.op})
		var ae *APIError
		if !errors.As(err, &ae) {
			t.Fatalf("%s: err = %v", tc.op, err)
		}
		if ae.Code != tc.code || !strings.Contains(ae.Message, tc.msg) || ae.Service != tc.svc {
			t.Errorf("%s: got %+v", tc.op, ae)
		}
	}
	_, err := c.Call(context.Background(), &Request{Service: "ec2", Operation: "DescribeInstances", Params: json.RawMessage(`[1]`)})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != CodeInvalidRequest {
		t.Errorf("array params: %v", err)
	}
}

func TestCallTimeout(t *testing.T) {
	f := fakeaws.New(t, []fakeaws.Rule{{Service: "ec2", Operation: "describe-regions", SleepMS: 5000, Stdout: map[string]any{}}})
	c := newCLI(f, func(c *CLI) { c.Timeout = 200 * time.Millisecond })
	start := time.Now()
	_, err := c.Call(context.Background(), &Request{Service: "ec2", Operation: "DescribeRegions"})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != CodeTimeout {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("timeout took %v", time.Since(start))
	}
}

func TestCallStreamingBody(t *testing.T) {
	f := fakeaws.New(t, []fakeaws.Rule{{Service: "s3api", Operation: "get-object",
		Stdout: map[string]any{"ContentLength": 11, "ContentType": "text/plain"}, Body: "hello world"}})
	out, err := newCLI(f, nil).Call(context.Background(), &Request{Service: "s3", Operation: "get_object", Params: json.RawMessage(`{"Bucket":"b","Key":"k"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["Body"] != "hello world" || m["ContentType"] != "text/plain" {
		t.Fatalf("out = %s", out)
	}

	c := newCLI(f, func(c *CLI) { c.MaxBodyBytes = 5 })
	out, _ = c.Call(context.Background(), &Request{Service: "s3", Operation: "get_object", Params: json.RawMessage(`{"Bucket":"b","Key":"k"}`)})
	json.Unmarshal(out, &m)
	if m["Body"] != "hello" || m["BodyTruncated"] != true {
		t.Fatalf("truncated out = %s", out)
	}
}

func TestParseCLIError(t *testing.T) {
	e := ParseCLIError("\nAn error occurred (AccessDenied) when calling the ListBuckets operation: Access Denied\n")
	if e.Code != "AccessDenied" || e.Operation != "ListBuckets" || e.Message != "Access Denied" {
		t.Fatalf("%+v", e)
	}
	e = ParseCLIError("something odd")
	if e.Code != CodeCLI || e.Message != "something odd" {
		t.Fatalf("%+v", e)
	}
}
