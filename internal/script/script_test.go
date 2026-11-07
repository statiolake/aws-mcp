package script

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/statiolake/aws-mcp/internal/caller"
	"github.com/statiolake/aws-mcp/internal/fakeaws"
)

func TestMain(m *testing.M) {
	fakeaws.MaybeRun()
	os.Exit(m.Run())
}

// recorder is an in-process fake Caller.
type recorder struct {
	mu    sync.Mutex
	reqs  []caller.Request
	reply func(req *caller.Request) (string, error)
}

func (r *recorder) Call(ctx context.Context, req *caller.Request) (json.RawMessage, error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, *req)
	r.mu.Unlock()
	if r.reply == nil {
		return json.RawMessage(`{}`), nil
	}
	s, err := r.reply(req)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(s), nil
}

func runScript(t *testing.T, c caller.Caller, limits Limits, src string) *Result {
	t.Helper()
	r := &Runner{Caller: c, Limits: limits}
	return r.Run(context.Background(), src, Env{Region: "ap-northeast-1"})
}

func mustOK(t *testing.T, res *Result) {
	t.Helper()
	if res.Err != nil {
		t.Fatalf("script failed: %v\noutput:\n%s", res.Err, res.Output)
	}
}

func TestCallMapping(t *testing.T) {
	rec := &recorder{reply: func(req *caller.Request) (string, error) {
		return `{"Reservations":[{"Instances":[{"InstanceId":"i-1","State":{"Name":"running"}}]}]}`, nil
	}}
	res := runScript(t, rec, Limits{}, `
ec2 = aws.client("ec2", profile="dev")
r = ec2.describe_instances(Filters=[{"Name": "instance-state-name", "Values": ["running"]}], MaxResults=100)
print(r["Reservations"][0]["Instances"][0]["InstanceId"])
other = aws.client("s3", region="us-west-2")
other.call("ListBuckets")
[i["InstanceId"] for res in r["Reservations"] for i in res["Instances"]]
`)
	mustOK(t, res)
	if res.Output != "i-1\n[\"i-1\"]\n" {
		t.Fatalf("output = %q", res.Output)
	}
	if len(rec.reqs) != 2 {
		t.Fatalf("reqs = %d", len(rec.reqs))
	}
	r0 := rec.reqs[0]
	if r0.Service != "ec2" || r0.Operation != "describe_instances" || r0.Region != "ap-northeast-1" || r0.Profile != "dev" {
		t.Fatalf("req0 = %+v", r0)
	}
	if string(r0.Params) != `{"Filters":[{"Name":"instance-state-name","Values":["running"]}],"MaxResults":100}` {
		t.Fatalf("params = %s", r0.Params)
	}
	r1 := rec.reqs[1]
	if r1.Service != "s3" || r1.Operation != "ListBuckets" || r1.Region != "us-west-2" || string(r1.Params) != "{}" {
		t.Fatalf("req1 = %+v", r1)
	}
	if res.Calls != 2 {
		t.Fatalf("calls = %d", res.Calls)
	}
}

func TestPositionalArgsRejected(t *testing.T) {
	res := runScript(t, &recorder{}, Limits{}, `aws.client("ec2").describe_instances(["i-1"])`)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "keyword arguments") {
		t.Fatalf("err = %v", res.Err)
	}
}

func TestJSONConversion(t *testing.T) {
	rec := &recorder{reply: func(*caller.Request) (string, error) {
		return `{"z":1,"a":{"y":2.5,"b":[true,false,null]},"big":123456789012345678901234567890,"s":"x"}`, nil
	}}
	res := runScript(t, rec, Limits{}, `
r = aws.client("x").get_thing()
print(list(r.keys()))
print(type(r["a"]["y"]), r["big"] + 1, r["a"]["b"])
print(json.encode(r["a"]))  # the starlark json module sorts keys
r
`)
	mustOK(t, res)
	want := "[\"z\", \"a\", \"big\", \"s\"]\nfloat 123456789012345678901234567891 [True, False, None]\n" +
		`{"b":[true,false,null],"y":2.5}` + "\n" +
		`{"z":1,"a":{"y":2.5,"b":[true,false,null]},"big":123456789012345678901234567890,"s":"x"}` + "\n"
	if res.Output != want {
		t.Fatalf("output =\n%q\nwant\n%q", res.Output, want)
	}
}

func TestFinalValue(t *testing.T) {
	cases := map[string]string{
		"1 + 2":                          "3\n",
		"x = 1":                          "",
		`"hello"`:                        "hello\n",
		`print("a")`:                     "a\n",
		`{"k": [1, (2, 3)]}`:             "{\"k\":[1,[2,3]]}\n",
		"def f():\n  pass\nf":            "<function f>\n",
		"for i in range(2):\n  print(i)": "0\n1\n",
		"sum([1, 2, 3])":                 "6\n",
		"sum([0.5, 1], 1)":               "2.5\n",
	}
	for src, want := range cases {
		res := runScript(t, &recorder{}, Limits{}, src)
		mustOK(t, res)
		if res.Output != want {
			t.Errorf("%q: output %q, want %q", src, res.Output, want)
		}
	}
}

func TestHermetic(t *testing.T) {
	for _, src := range []string{`load("x.star", "y")`, `open("/etc/passwd")`} {
		res := runScript(t, &recorder{}, Limits{}, src)
		if res.Err == nil {
			t.Errorf("%q succeeded", src)
		}
	}
}

func TestPaginateNextToken(t *testing.T) {
	rec := &recorder{reply: func(req *caller.Request) (string, error) {
		var p map[string]any
		json.Unmarshal(req.Params, &p)
		switch p["NextToken"] {
		case nil:
			return `{"Items":[1,2],"NextToken":"t1"}`, nil
		case "t1":
			return `{"Items":[3],"NextToken":"t2"}`, nil
		case "t2":
			return `{"Items":[4]}`, nil
		}
		return "", fmt.Errorf("bad token %v", p["NextToken"])
	}}
	res := runScript(t, rec, Limits{}, `
pages = aws.client("ec2").paginate("describe_instances", MaxResults=2)
[x for p in pages for x in p["Items"]]
`)
	mustOK(t, res)
	if res.Output != "[1,2,3,4]\n" {
		t.Fatalf("output = %q", res.Output)
	}
	if len(rec.reqs) != 3 || string(rec.reqs[2].Params) != `{"MaxResults":2,"NextToken":"t2"}` {
		t.Fatalf("reqs = %d, last params %s", len(rec.reqs), rec.reqs[len(rec.reqs)-1].Params)
	}
}

func TestPaginateConventions(t *testing.T) {
	cases := []struct {
		name   string
		pages  []string
		in     string // request member carrying the token
		expect int
	}{
		{"s3 v2", []string{`{"Contents":[],"IsTruncated":true,"NextContinuationToken":"c1"}`, `{"Contents":[],"IsTruncated":false}`}, "ContinuationToken", 2},
		{"iam", []string{`{"Users":[],"IsTruncated":true,"Marker":"m1"}`, `{"Users":[],"IsTruncated":false}`}, "Marker", 2},
		{"rds", []string{`{"DBInstances":[],"Marker":"m1"}`, `{"DBInstances":[]}`}, "Marker", 2},
		{"dynamodb", []string{`{"Items":[],"LastEvaluatedKey":{"pk":{"S":"a"}}}`, `{"Items":[]}`}, "ExclusiveStartKey", 2},
		{"lambda", []string{`{"Functions":[],"NextMarker":"n1"}`, `{"Functions":[]}`}, "Marker", 2},
		{"logs", []string{`{"events":[],"nextToken":"n1"}`, `{"events":[]}`}, "nextToken", 2},
		{"repeat", []string{`{"events":[],"nextForwardToken":"f"}`, `{"events":[],"nextForwardToken":"f"}`, `{"x":1}`}, "nextToken", 2},
	}
	for _, tc := range cases {
		n := 0
		rec := &recorder{reply: func(req *caller.Request) (string, error) {
			n++
			if n > 1 && !strings.Contains(string(req.Params), `"`+tc.in+`"`) {
				return "", fmt.Errorf("page %d without %s: %s", n, tc.in, req.Params)
			}
			return tc.pages[n-1], nil
		}}
		res := runScript(t, rec, Limits{}, `len(aws.client("svc").paginate("list_things"))`)
		mustOK(t, res)
		if res.Output != fmt.Sprintf("%d\n", tc.expect) {
			t.Errorf("%s: output %q", tc.name, res.Output)
		}
	}
}

func TestPaginateMaxPagesAndOverride(t *testing.T) {
	n := 0
	rec := &recorder{reply: func(req *caller.Request) (string, error) {
		n++
		return fmt.Sprintf(`{"Cursor":"c%d"}`, n), nil
	}}
	res := runScript(t, rec, Limits{}, `len(aws.client("svc").paginate("list_things", _input_token="After", _output_token="Cursor", _max_pages=3))`)
	mustOK(t, res)
	if !strings.HasPrefix(res.Output, "3\n") || !strings.Contains(res.Output, "stopped after 3 pages") {
		t.Fatalf("output = %q", res.Output)
	}
	if string(rec.reqs[2].Params) != `{"After":"c2"}` {
		t.Fatalf("params = %s", rec.reqs[2].Params)
	}
}

func TestErrors(t *testing.T) {
	rec := &recorder{reply: func(req *caller.Request) (string, error) {
		if req.Operation == "describe_instances" {
			return "", &caller.APIError{Service: "ec2", Operation: "DescribeInstances", Code: "InvalidInstanceID.NotFound", Message: "The instance ID 'i-1' does not exist"}
		}
		return `{"ok":true}`, nil
	}}
	res := runScript(t, rec, Limits{}, `
print("before")
aws.client("ec2").describe_instances(InstanceIds=["i-1"])
print("after")
`)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "InvalidInstanceID.NotFound") ||
		!strings.Contains(res.Err.Error(), "does not exist") || !strings.Contains(res.Err.Error(), "script.star:3") {
		t.Fatalf("err = %v", res.Err)
	}
	if res.Output != "before\n" || !strings.Contains(res.Text(), "Error: ") {
		t.Fatalf("output = %q", res.Output)
	}

	res = runScript(t, rec, Limits{}, `
ec2 = aws.client("ec2")
r, err = aws.try_call(ec2.describe_instances, InstanceIds=["i-1"])
print(r, err["Code"])
r, err = aws.try_call(lambda: ec2.describe_vpcs())
print(r, err)
`)
	mustOK(t, res)
	if res.Output != "None InvalidInstanceID.NotFound\n{\"ok\": True} None\n" {
		t.Fatalf("output = %q", res.Output)
	}

	// try_call does not swallow non-AWS errors.
	res = runScript(t, rec, Limits{}, `aws.try_call(lambda: 1 // 0)`)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "division by zero") {
		t.Fatalf("err = %v", res.Err)
	}
}

func TestLimitSteps(t *testing.T) {
	res := runScript(t, &recorder{}, Limits{MaxSteps: 10000}, "x = 0\nwhile True:\n  x += 1\n")
	if res.Err == nil || !strings.Contains(res.Err.Error(), "execution steps") {
		t.Fatalf("err = %v", res.Err)
	}
}

func TestLimitTime(t *testing.T) {
	start := time.Now()
	res := runScript(t, &recorder{}, Limits{MaxSteps: 1 << 62, Timeout: 200 * time.Millisecond}, "while True:\n  pass\n")
	if res.Err == nil || !strings.Contains(res.Err.Error(), "wall-time limit") {
		t.Fatalf("err = %v", res.Err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %v", d)
	}

	// A slow call is cancelled through the context.
	blocking := caller.Func(func(ctx context.Context, req *caller.Request) (json.RawMessage, error) {
		<-ctx.Done()
		return nil, &caller.APIError{Code: caller.CodeTimeout, Message: ctx.Err().Error()}
	})
	res = runScript(t, blocking, Limits{Timeout: 200 * time.Millisecond}, `aws.client("ec2").describe_vpcs()`)
	if res.Err == nil {
		t.Fatal("no error")
	}
}

func TestLimitOutput(t *testing.T) {
	res := runScript(t, &recorder{}, Limits{MaxOutputBytes: 100}, "for i in range(100):\n  print('0123456789')\n")
	mustOK(t, res)
	if !res.Truncated || !strings.HasPrefix(res.Output, strings.Repeat("0123456789\n", 9)) ||
		!strings.Contains(res.Output, "[output truncated: the script produced 1100 bytes, the limit is 100") {
		t.Fatalf("output = %q", res.Output)
	}
}

func TestLimitCalls(t *testing.T) {
	rec := &recorder{}
	res := runScript(t, rec, Limits{MaxCalls: 3}, `
ec2 = aws.client("ec2")
for i in range(10):
    aws.try_call(ec2.describe_vpcs)
`)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "limit of 3 AWS API calls") {
		t.Fatalf("err = %v", res.Err)
	}
	if len(rec.reqs) != 3 {
		t.Fatalf("backend calls = %d", len(rec.reqs))
	}
	// A batch that would exceed the budget is refused before any call.
	rec = &recorder{}
	res = runScript(t, rec, Limits{MaxCalls: 3}, `aws.batch([(aws.client("ec2"), "describe_vpcs")] * 4)`)
	if res.Err == nil || len(rec.reqs) != 0 {
		t.Fatalf("err = %v, calls = %d", res.Err, len(rec.reqs))
	}
}

func TestBatchConcurrency(t *testing.T) {
	var inflight, peak atomic.Int32
	slow := caller.Func(func(ctx context.Context, req *caller.Request) (json.RawMessage, error) {
		n := inflight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(150 * time.Millisecond)
		inflight.Add(-1)
		return json.RawMessage(fmt.Sprintf(`{"Region":%q}`, req.Region)), nil
	})
	start := time.Now()
	res := runScript(t, slow, Limits{MaxConcurrency: 4}, `
regions = ["r%d" % i for i in range(8)]
rs = aws.batch([(aws.client("ec2", region=r), "describe_vpcs", {"MaxResults": 5}) for r in regions])
[r["Region"] for r in rs]
`)
	mustOK(t, res)
	d := time.Since(start)
	if res.Output != `["r0","r1","r2","r3","r4","r5","r6","r7"]`+"\n" {
		t.Fatalf("output = %q", res.Output)
	}
	if peak.Load() != 4 {
		t.Fatalf("peak concurrency = %d", peak.Load())
	}
	if d > 900*time.Millisecond {
		t.Fatalf("batch took %v; not concurrent", d)
	}
}

// TestWithFakeCLI runs a script end to end through the CLI backend.
func TestWithFakeCLI(t *testing.T) {
	f := fakeaws.New(t, []fakeaws.Rule{
		{Service: "ec2", Operation: "describe-instances", Match: map[string]any{"NextToken": "p2"},
			Stdout: map[string]any{"Reservations": []any{map[string]any{"Instances": []any{map[string]any{"InstanceId": "i-2"}}}}}},
		{Service: "ec2", Operation: "describe-instances", Absent: []string{"NextToken"},
			Stdout: map[string]any{"Reservations": []any{map[string]any{"Instances": []any{map[string]any{"InstanceId": "i-1"}}}}, "NextToken": "p2"}},
		{Service: "s3api", Operation: "list-buckets", SleepMS: 300, Stdout: map[string]any{"Buckets": []any{}}},
		{Service: "sts", Operation: "get-caller-identity", SleepMS: 300, Stdout: map[string]any{"Account": "123456789012"}},
		{Service: "iam", Operation: "get-role", Exit: 254,
			Stderr: "\nAn error occurred (NoSuchEntity) when calling the GetRole operation: The role with name x cannot be found.\n"},
	})
	cli := caller.NewCLI(caller.CLI{Path: f.Path, Env: f.Env})
	r := &Runner{Caller: cli}
	res := r.Run(context.Background(), `
ec2 = aws.client("ec2")
ids = [i["InstanceId"] for p in ec2.paginate("describe_instances", MaxResults=5) for res in p["Reservations"] for i in res["Instances"]]
b, ident = aws.batch([(aws.client("s3"), "list_buckets"), (aws.client("sts"), "get_caller_identity")])
_, err = aws.try_call(aws.client("iam").get_role, RoleName="x")
{"ids": ids, "account": ident["Account"], "err": err["Code"]}
`, Env{Region: "eu-central-1"})
	mustOK(t, res)
	if res.Output != `{"ids":["i-1","i-2"],"account":"123456789012","err":"NoSuchEntity"}`+"\n" {
		t.Fatalf("output = %q", res.Output)
	}
	inv := f.Invocations()
	if len(inv) != 5 {
		t.Fatalf("invocations = %d", len(inv))
	}
	var s3, sts *fakeaws.Invocation
	for i := range inv {
		if inv[i].Region != "eu-central-1" {
			t.Errorf("region = %q", inv[i].Region)
		}
		switch inv[i].Service {
		case "s3api":
			s3 = &inv[i]
		case "sts":
			sts = &inv[i]
		}
	}
	if s3 == nil || sts == nil || s3.Start > sts.End || sts.Start > s3.End {
		t.Fatalf("batch calls did not overlap: %+v %+v", s3, sts)
	}
}
