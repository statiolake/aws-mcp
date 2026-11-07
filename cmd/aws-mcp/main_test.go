package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/statiolake/aws-mcp/internal/awsdocs"
	"github.com/statiolake/aws-mcp/internal/caller"
	"github.com/statiolake/aws-mcp/internal/fakeaws"
	"github.com/statiolake/aws-mcp/internal/script"
)

func TestMain(m *testing.M) {
	if os.Getenv("AWS_MCP_TEST_SERVE") == "1" {
		// Run as the server; the aws CLI children it starts (this binary
		// again) inherit FAKE_AWS=1 and act as the fake CLI.
		os.Unsetenv("AWS_MCP_TEST_SERVE")
		os.Setenv("FAKE_AWS", "1")
		os.Exit(realMain(strings.Fields(os.Getenv("AWS_MCP_TEST_ARGS"))))
	}
	fakeaws.MaybeRun()
	os.Exit(m.Run())
}

func testConfig(t *testing.T, args ...string) *Config {
	t.Helper()
	cfg, _, err := parseConfig(args, func(string) string { return "" }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestParseConfig(t *testing.T) {
	cfg := testConfig(t)
	if !cfg.ReadOnly || cfg.AWSCLI != "aws" || cfg.Limits.MaxCalls != script.DefaultLimits.MaxCalls {
		t.Fatalf("defaults = %+v", cfg)
	}
	cfg = testConfig(t, "--allow-writes", "--region", "eu-west-1", "--max-calls", "7", "--script-timeout", "5s")
	if cfg.ReadOnly || cfg.Region != "eu-west-1" || cfg.Limits.MaxCalls != 7 || cfg.Limits.Timeout != 5*time.Second {
		t.Fatalf("flags = %+v", cfg)
	}
	env := map[string]string{"AWS_MCP_READ_ONLY": "false", "AWS_MCP_PROFILE": "dev", "AWS_MCP_AWS_CLI": "/opt/aws"}
	cfg, _, err := parseConfig(nil, func(k string) string { return env[k] }, io.Discard)
	if err != nil || cfg.ReadOnly || cfg.Profile != "dev" || cfg.AWSCLI != "/opt/aws" {
		t.Fatalf("env = %+v, %v", cfg, err)
	}
	// Flags override the environment.
	cfg, _, _ = parseConfig([]string{"--read-only"}, func(k string) string { return env[k] }, io.Discard)
	if !cfg.ReadOnly {
		t.Fatal("--read-only did not override env")
	}
	if _, _, err := parseConfig([]string{"--max-calls", "0"}, func(string) string { return "" }, io.Discard); err == nil {
		t.Fatal("zero limit accepted")
	}
}

func connect(t *testing.T, srv *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

var fakeRules = []fakeaws.Rule{
	{Service: "sts", Operation: "get-caller-identity", Stdout: map[string]any{"Account": "123456789012", "Arn": "arn:aws:iam::123456789012:user/me"}},
	{Service: "ec2", Operation: "describe-regions", Echo: true},
	{Service: "ec2", Operation: "terminate-instances", Stdout: map[string]any{"TerminatingInstances": []any{}}},
}

func fakeServer(t *testing.T, args ...string) (*mcp.ClientSession, *fakeaws.Fake) {
	t.Helper()
	f := fakeaws.New(t, fakeRules)
	cfg := testConfig(t, args...)
	backend := caller.NewCLI(caller.CLI{Path: f.Path, Env: f.Env, Region: cfg.Region, Timeout: 10 * time.Second})
	return connect(t, newServer(cfg, backend, nil)), f
}

func TestListTools(t *testing.T) {
	cs, _ := fakeServer(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]*mcp.Tool{}
	for _, tl := range res.Tools {
		names[tl.Name] = tl
	}
	for _, n := range []string{"run_script", "call_aws", "describe_aws_operation", "search_aws_docs", "read_aws_docs"} {
		if names[n] == nil {
			t.Fatalf("tool %s missing; have %v", n, names)
		}
	}
	d := names["run_script"].Description
	for _, s := range []string{"aws.client", "paginate", "aws.batch", "aws.try_call", "read-only mode", "Example"} {
		if !strings.Contains(d, s) {
			t.Errorf("run_script description lacks %q", s)
		}
	}
	if !names["call_aws"].Annotations.ReadOnlyHint {
		t.Error("call_aws not annotated read-only")
	}

	if len(res.Tools) != 5 {
		t.Fatalf("%d tools", len(res.Tools))
	}
	if _, _, err := parseConfig([]string{"--no-rds"}, func(string) string { return "" }, io.Discard); err == nil {
		t.Fatal("--no-rds still accepted")
	}

	cs, _ = fakeServer(t, "--allow-writes")
	res, _ = cs.ListTools(context.Background(), nil)
	for _, tl := range res.Tools {
		if tl.Name != "run_script" && tl.Name != "call_aws" {
			continue // the other tools never change anything
		}
		if strings.Contains(tl.Description, "read-only mode") || tl.Annotations.ReadOnlyHint {
			t.Errorf("%s still claims read-only", tl.Name)
		}
	}
}

func TestRunScriptTool(t *testing.T) {
	cs, f := fakeServer(t, "--region", "us-east-1")
	out, isErr := callTool(t, cs, "run_script", map[string]any{
		"script": `
sts = aws.client("sts")
me = sts.get_caller_identity()
print("account", me["Account"])
r = aws.client("ec2", region="ap-northeast-1").describe_regions(AllRegions=True)
r["input"]
`,
	})
	if isErr || out != "account 123456789012\n{\"AllRegions\":true}\n" {
		t.Fatalf("out = %q (err %v)", out, isErr)
	}
	inv := f.Invocations()
	if len(inv) != 2 || inv[0].Region != "us-east-1" || inv[1].Region != "ap-northeast-1" {
		t.Fatalf("invocations = %+v", inv)
	}

	out, isErr = callTool(t, cs, "run_script", map[string]any{"script": `aws.client("ec2").terminate_instances(InstanceIds=["i-1"])`})
	if !isErr || !strings.Contains(out, "ReadOnlyRefused") {
		t.Fatalf("out = %q", out)
	}
	if len(f.Invocations()) != 2 {
		t.Fatal("refused call reached the CLI")
	}

	out, isErr = callTool(t, cs, "run_script", map[string]any{"script": "x = (", "region": "eu-west-1"})
	if !isErr || !strings.Contains(out, "script.star:1") {
		t.Fatalf("syntax error out = %q", out)
	}
}

func TestCallAWSTool(t *testing.T) {
	cs, f := fakeServer(t)
	out, isErr := callTool(t, cs, "call_aws", map[string]any{"service": "ec2", "operation": "DescribeRegions",
		"params": map[string]any{"RegionNames": []string{"us-east-1"}}, "region": "us-west-2", "profile": "p"})
	var got map[string]any
	if isErr || json.Unmarshal([]byte(out), &got) != nil {
		t.Fatalf("out = %q", out)
	}
	if got["region"] != "us-west-2" || got["profile"] != "p" {
		t.Fatalf("got = %v", got)
	}
	out, isErr = callTool(t, cs, "call_aws", map[string]any{"service": "ec2", "operation": "terminate_instances", "params": map[string]any{"InstanceIds": []string{"i-1"}}})
	if !isErr || !strings.Contains(out, "ReadOnlyRefused") || !strings.Contains(out, "--allow-writes") {
		t.Fatalf("out = %q", out)
	}
	out, isErr = callTool(t, cs, "call_aws", map[string]any{"service": "ec2", "operation": "DescribeNoSuchThing"})
	if !isErr || !strings.Contains(out, "InvalidOperation") {
		t.Fatalf("out = %q", out)
	}
	if n := len(f.Invocations()); n != 2 {
		t.Fatalf("invocations = %d", n)
	}

	cs, _ = fakeServer(t, "--allow-writes")
	out, isErr = callTool(t, cs, "call_aws", map[string]any{"service": "ec2", "operation": "TerminateInstances", "params": map[string]any{"InstanceIds": []string{"i-1"}}})
	if isErr || !strings.Contains(out, "TerminatingInstances") {
		t.Fatalf("allow-writes out = %q", out)
	}
}

// TestUnknownServiceEndToEnd drives a made-up service and operations, which
// appear in no list anywhere in the code, through every layer down to the
// (fake) CLI: names are converted generically and the read-only classifier
// works by verb.
func TestUnknownServiceEndToEnd(t *testing.T) {
	f := fakeaws.New(t, []fakeaws.Rule{
		{Service: "quantum-ledger", Operation: "list-entanglement-proofs", Match: map[string]any{"NextToken": "p2"},
			Stdout: map[string]any{"Proofs": []any{"c"}}},
		{Service: "quantum-ledger", Operation: "list-entanglement-proofs",
			Stdout: map[string]any{"Proofs": []any{"a", "b"}, "NextToken": "p2"}},
		{Service: "quantum-ledger", Operation: "describe-qubit-array", Echo: true,
			Skeleton: map[string]any{"ArrayId": "", "IncludeCalibration": true},
			Help:     "NAME\n       describe-qubit-array -\n\nDESCRIPTION\n       Describes a qubit array.\n"},
		{Service: "quantum-ledger", Operation: "entangle-qubits", Stdout: map[string]any{"ProofId": "x"}},
	})
	cfg := testConfig(t)
	cs := connect(t, newServer(cfg, caller.NewCLI(caller.CLI{Path: f.Path, Env: f.Env, Timeout: 10 * time.Second}), nil))

	out, isErr := callTool(t, cs, "run_script", map[string]any{"script": `
ql = aws.client("quantum-ledger", region="eu-north-1")
pages = ql.paginate("list_entanglement_proofs")
d = ql.describe_qubit_array(ArrayId="qa-1", IncludeCalibration=True)
[p for page in pages for p in page["Proofs"]], d["input"], d["region"]`})
	if isErr || out != `[["a","b","c"],{"ArrayId":"qa-1","IncludeCalibration":true},"eu-north-1"]`+"\n" {
		t.Fatalf("run_script = %q", out)
	}
	out, isErr = callTool(t, cs, "call_aws", map[string]any{"service": "quantum-ledger", "operation": "DescribeQubitArray",
		"params": map[string]any{"ArrayId": "qa-2"}})
	if isErr || !strings.Contains(out, `"ArrayId":"qa-2"`) {
		t.Fatalf("call_aws = %q", out)
	}
	out, isErr = callTool(t, cs, "call_aws", map[string]any{"service": "quantum-ledger", "operation": "EntangleQubits"})
	if !isErr || !strings.Contains(out, "ReadOnlyRefused") {
		t.Fatalf("write in read-only mode = %q", out)
	}
	out, isErr = callTool(t, cs, "describe_aws_operation", map[string]any{"service": "quantum-ledger", "operation": "describe_qubit_array"})
	if isErr || !strings.Contains(out, `"IncludeCalibration": true`) || !strings.Contains(out, "Describes a qubit array.") ||
		!strings.Contains(out, "aws quantum-ledger describe-qubit-array") || !strings.Contains(out, "Read-only mode: allowed") {
		t.Fatalf("describe = %q", out)
	}
	out, _ = callTool(t, cs, "describe_aws_operation", map[string]any{"service": "quantum-ledger", "operation": "EntangleQubits", "skeleton_only": true})
	if !strings.Contains(out, "no skeleton") { // the fake has no skeleton for it; the CLI error is passed on
		t.Fatalf("describe without skeleton = %q", out)
	}

	var ops []string
	for _, inv := range f.Invocations() {
		ops = append(ops, inv.Service+" "+strings.Join(inv.Args[1:2], ""))
	}
	got := strings.Join(ops, ",")
	if strings.Count(got, "quantum-ledger list-entanglement-proofs") != 2 || strings.Count(got, "describe-qubit-array") != 4 ||
		strings.Count(got, "entangle-qubits") != 1 {
		t.Fatalf("invocations = %s", got)
	}

	cs = connect(t, newServer(testConfig(t, "--allow-writes"), caller.NewCLI(caller.CLI{Path: f.Path, Env: f.Env, Timeout: 10 * time.Second}), nil))
	out, isErr = callTool(t, cs, "run_script", map[string]any{"script": `aws.client("quantum-ledger").entangle_qubits(Qubits=[1, 2])["ProofId"]`})
	if isErr || out != "x\n" {
		t.Fatalf("allow-writes = %q", out)
	}
}

type fakeTransport struct{ h http.HandlerFunc }

func (f fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	f.h(rec, r)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

func TestDocsTools(t *testing.T) {
	long := strings.Repeat("<p>Lorem ipsum dolor sit amet.</p>\n", 400)
	docs := awsdocs.New(awsdocs.Options{HTTP: &http.Client{Transport: fakeTransport{func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Host == "proxy.search.docs.aws.com":
			io.WriteString(w, `{"suggestions":[{"textExcerptSuggestion":{"link":"https://docs.aws.amazon.com/w/latest/ug/what-is.html","title":"What is Widgets?","summary":"Widgets hold parts."}}]}`)
		case r.URL.Path == "/w/latest/ug/what-is.html":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<html><head><title>What is Widgets?</title></head><body><nav>menu</nav><div id="main-col-body"><h1>What is Widgets?</h1>`+long+`</div></body></html>`)
		default:
			http.NotFound(w, r)
		}
	}}}})
	cs := connect(t, newServer(testConfig(t), nil, docs))

	out, isErr := callTool(t, cs, "search_aws_docs", map[string]any{"query": "widgets"})
	if isErr || out != "1. What is Widgets?\n   https://docs.aws.amazon.com/w/latest/ug/what-is.html\n   Widgets hold parts.\n" {
		t.Fatalf("search = %q", out)
	}
	out, isErr = callTool(t, cs, "read_aws_docs", map[string]any{"url": "https://docs.aws.amazon.com/w/latest/ug/what-is.html"})
	if isErr || !strings.HasPrefix(out, "# What is Widgets?\n\nLorem ipsum") || strings.Contains(out, "menu") ||
		!strings.Contains(out, "call read_aws_docs again with start_index=") {
		t.Fatalf("read = %q", out)
	}
	i := strings.LastIndex(out, "start_index=")
	var next int
	fmt.Sscanf(out[i:], "start_index=%d", &next)
	out, isErr = callTool(t, cs, "read_aws_docs", map[string]any{"url": "https://docs.aws.amazon.com/w/latest/ug/what-is.html", "start_index": next, "max_length": 100000})
	if isErr || !strings.HasPrefix(out, "[https://docs.aws.amazon.com/w/latest/ug/what-is.html, from character") || strings.Contains(out, "continues") {
		t.Fatalf("read on = %q", out[:200])
	}
	out, isErr = callTool(t, cs, "read_aws_docs", map[string]any{"url": "https://example.com/"})
	if !isErr || !strings.Contains(out, "only https://docs.aws.amazon.com/") {
		t.Fatalf("other host = %q", out)
	}
}

// TestDescriptionExamples runs the examples from the run_script description.
func TestDescriptionExamples(t *testing.T) {
	desc := runScriptDescription(testConfig(t))
	parts := strings.Split(desc, "\nExample: ")[1:]
	if len(parts) != 3 {
		t.Fatalf("%d examples", len(parts))
	}
	backend := caller.Func(func(ctx context.Context, req *caller.Request) (json.RawMessage, error) {
		switch req.Operation {
		case "describe_instances":
			return json.RawMessage(`{"Reservations":[{"Instances":[{"InstanceId":"i-1","InstanceType":"t3.micro"}]}]}`), nil
		case "list_functions":
			return json.RawMessage(`{"Functions":[{"FunctionName":"f-` + req.Region + `"}]}`), nil
		case "list_objects_v2":
			return json.RawMessage(`{"Contents":[{"Key":"a","Size":10},{"Key":"b","Size":5}]}`), nil
		case "get_bucket_tagging":
			return nil, &caller.APIError{Service: "s3", Operation: "GetBucketTagging", Code: "NoSuchTagSet", Message: "The TagSet does not exist"}
		}
		return json.RawMessage(`{}`), nil
	})
	want := []string{
		`[["i-1","t3.micro"]]`,
		`{"us-east-1":["f-us-east-1"],"eu-west-1":["f-eu-west-1"],"ap-northeast-1":["f-ap-northeast-1"]}`,
		`{"bytes":15,"tags":"NoSuchTagSet"}`,
	}
	r := &script.Runner{Caller: backend}
	for i, p := range parts {
		src := p[strings.IndexByte(p, '\n')+1:]
		res := r.Run(context.Background(), src, script.Env{})
		if res.Err != nil || strings.TrimSpace(res.Output) != want[i] {
			t.Errorf("example %d: %q, %v\n%s", i+1, res.Output, res.Err, src)
		}
	}
}

// TestStdio starts the real server binary over stdio.
func TestStdio(t *testing.T) {
	f := fakeaws.New(t, fakeRules)
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "AWS_MCP_TEST_SERVE=1", "AWS_MCP_TEST_ARGS=--aws-cli "+f.Path)
	cmd.Env = append(cmd.Env, f.Env[1:]...) // rules and log; FAKE_AWS=1 is set by the server for its children
	cmd.Stderr = os.Stderr
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 5 {
		t.Fatalf("tools = %v, %v", tools, err)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "run_script", Arguments: map[string]any{
		"script": `aws.client("sts").get_caller_identity()["Account"]`}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || res.Content[0].(*mcp.TextContent).Text != "123456789012\n" {
		t.Fatalf("result = %+v", res.Content[0])
	}
}

// TestReadmeExamples runs the Python blocks of README.md against canned data.
func TestReadmeExamples(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	blocks := strings.Split(string(b), "```python\n")[1:]
	if len(blocks) < 4 {
		t.Fatalf("%d python blocks", len(blocks))
	}
	replies := map[string]string{
		"describe_instances": `{"Reservations":[{"Instances":[{"InstanceId":"i-1","InstanceType":"t3.micro"}]}]}`,
		"describe_regions":   `{"Regions":[{"RegionName":"us-east-1"},{"RegionName":"ap-northeast-1"}]}`,
		"list_users":         `{"Users":[{"UserName":"alice"},{"UserName":"bob"}],"IsTruncated":false}`,
		"get_object":         `{"Body":"{}","ContentType":"application/json"}`,
	}
	backend := caller.Func(func(ctx context.Context, req *caller.Request) (json.RawMessage, error) {
		if req.Operation == "list_mfa_devices" {
			if strings.Contains(string(req.Params), "alice") {
				return json.RawMessage(`{"MFADevices":[{"SerialNumber":"x"}]}`), nil
			}
			return json.RawMessage(`{"MFADevices":[]}`), nil
		}
		if r, ok := replies[req.Operation]; ok {
			return json.RawMessage(r), nil
		}
		return nil, &caller.APIError{Code: "Unexpected", Message: req.Operation}
	})
	want := []string{`[["i-1","t3.micro"]]`, `{"us-east-1":1,"ap-northeast-1":1}`, `["bob"]`, `{}`}
	r := &script.Runner{Caller: backend}
	for i, blk := range blocks[:4] {
		src := blk[:strings.Index(blk, "```")]
		res := r.Run(context.Background(), src, script.Env{})
		if res.Err != nil || strings.TrimSpace(res.Output) != want[i] {
			t.Errorf("README example %d: %q, %v\n%s", i+1, res.Output, res.Err, src)
		}
	}
}
