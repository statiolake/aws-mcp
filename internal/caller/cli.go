package caller

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/statiolake/aws-mcp/internal/awsname"
)

// CLI calls AWS through the AWS CLI v2, one subprocess per call:
//
//	aws <service> <operation> --cli-input-json <params> --output json --no-paginate ...
//
// Every CLI operation accepts --cli-input-json, so any API operation of any
// service is reachable, and credentials are resolved by the CLI exactly as
// for interactive use (profiles, SSO, environment, IMDS, ...).
type CLI struct {
	// Path to the aws executable. Default "aws" (looked up in PATH).
	Path string
	// Region and Profile are used when a Request leaves them empty. Empty
	// means the CLI's own resolution (AWS_REGION, AWS_PROFILE, config files).
	Region  string
	Profile string
	// Timeout bounds each call. Zero means no limit beyond the context.
	Timeout time.Duration
	// MaxConcurrent bounds the number of CLI processes running at once across
	// all callers. Zero means 8.
	MaxConcurrent int
	// MaxResponseBytes bounds the CLI's stdout. Zero means 64 MiB.
	MaxResponseBytes int64
	// MaxBodyBytes bounds how much of a streamed response body (S3
	// GetObject, Lambda Invoke) is returned. Zero means 1 MiB.
	MaxBodyBytes int64
	// Env is appended to the inherited environment (tests use it).
	Env []string

	sem chan struct{}
}

// NewCLI returns a CLI caller; zero fields take their documented defaults.
func NewCLI(c CLI) *CLI {
	if c.Path == "" {
		c.Path = "aws"
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 8
	}
	if c.MaxResponseBytes <= 0 {
		c.MaxResponseBytes = 64 << 20
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = 1 << 20
	}
	c.sem = make(chan struct{}, c.MaxConcurrent)
	return &c
}

// streamingOutputs lists operations whose output member is a stream. The CLI
// requires an output file name as a positional argument for these; the file
// is read back into the named member of the result.
var streamingOutputs = map[string]string{
	"s3api get-object":         "Body",
	"s3api get-object-torrent": "Body",
	"lambda invoke":            "Payload",
}

// inlineParamLimit is the largest --cli-input-json passed on the command
// line; larger requests go through a temporary file (Linux limits a single
// argument to 128 KiB).
const inlineParamLimit = 64 << 10

// Args returns the CLI arguments for req, excluding any streaming output file
// name. It is exported for tests and debugging.
func (c *CLI) Args(req *Request, params string) []string {
	args := []string{
		awsname.CLIService(req.Service),
		awsname.CLIOperation(req.Operation),
		"--cli-input-json", params,
		"--output", "json",
		"--no-paginate",
		"--no-cli-pager",
		"--cli-binary-format", "raw-in-base64-out",
	}
	region, profile := req.Region, req.Profile
	if region == "" {
		region = c.Region
	}
	if profile == "" {
		profile = c.Profile
	}
	if region != "" {
		args = append(args, "--region", region)
	}
	if profile != "" {
		args = append(args, "--profile", profile)
	}
	return args
}

// Call implements Caller.
func (c *CLI) Call(ctx context.Context, req *Request) (json.RawMessage, error) {
	service := awsname.CLIService(req.Service)
	op := awsname.CLIOperation(req.Operation)
	apiErr := func(code, msg string) error {
		return &APIError{Service: req.Service, Operation: awsname.PascalOperation(req.Operation), Code: code, Message: msg}
	}
	if !validName(service) || !validName(op) {
		return nil, apiErr(CodeInvalidOperation, fmt.Sprintf("invalid service or operation name %q %q", req.Service, req.Operation))
	}

	params := "{}"
	if len(bytes.TrimSpace(req.Params)) > 0 && string(bytes.TrimSpace(req.Params)) != "null" {
		var buf bytes.Buffer
		if err := json.Compact(&buf, req.Params); err != nil {
			return nil, apiErr(CodeInvalidRequest, "params are not valid JSON: "+err.Error())
		}
		if buf.Len() == 0 || buf.Bytes()[0] != '{' {
			return nil, apiErr(CodeInvalidRequest, "params must be a JSON object")
		}
		params = buf.String()
	}

	var cleanup []string
	defer func() {
		for _, f := range cleanup {
			os.Remove(f)
		}
	}()
	if len(params) > inlineParamLimit {
		f, err := os.CreateTemp("", "aws-mcp-input-*.json")
		if err != nil {
			return nil, apiErr(CodeCLI, err.Error())
		}
		cleanup = append(cleanup, f.Name())
		_, werr := f.WriteString(params)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return nil, apiErr(CodeCLI, werr.Error())
		}
		params = "file://" + f.Name()
	}

	args := c.Args(req, params)
	bodyMember, streaming := streamingOutputs[service+" "+op]
	var outFile string
	if streaming {
		f, err := os.CreateTemp("", "aws-mcp-body-*")
		if err != nil {
			return nil, apiErr(CodeCLI, err.Error())
		}
		f.Close()
		outFile = f.Name()
		cleanup = append(cleanup, outFile)
		args = append(args, outFile)
	}

	stdout, stderr, runErr := c.run(ctx, args, nil)
	if runErr != nil {
		if code, ok := runErr.(localError); ok {
			return nil, apiErr(code.code, code.msg)
		}
		e := ParseCLIError(stderr)
		e.Service = req.Service
		if e.Operation == "" {
			e.Operation = awsname.PascalOperation(req.Operation)
		}
		return nil, e
	}
	if stdout.overflow {
		return nil, apiErr(CodeCLI, fmt.Sprintf("response larger than %d bytes; narrow the request (filters, MaxResults)", c.MaxResponseBytes))
	}

	out := bytes.TrimSpace(stdout.Bytes())
	if len(out) == 0 {
		out = []byte("{}")
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, out); err != nil {
		return nil, apiErr(CodeCLI, "CLI printed invalid JSON: "+truncate(string(out), 500))
	}
	if streaming {
		return c.attachBody(buf.Bytes(), outFile, bodyMember)
	}
	return buf.Bytes(), nil
}

// localError is a failure of run that did not come from the CLI's own exit
// status: a timeout or a CLI that could not be started.
type localError struct{ code, msg string }

func (e localError) Error() string { return e.msg }

// run starts the CLI once with args, bounded by the timeout and the
// concurrency limit. A non-nil error is either a localError or an
// *exec.ExitError, in which case stderr holds the CLI's message.
func (c *CLI) run(ctx context.Context, args, env []string) (*limitedBuffer, string, error) {
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return nil, "", localError{CodeTimeout, "timed out waiting for a free CLI slot: " + ctx.Err().Error()}
	}

	cmd := exec.CommandContext(ctx, c.Path, args...)
	cmd.Env = append(os.Environ(), "AWS_PAGER=", "AWS_CLI_AUTO_PROMPT=off")
	cmd.Env = append(cmd.Env, c.Env...)
	cmd.Env = withEnv(cmd.Env, env)
	cmd.WaitDelay = 2 * time.Second
	stdout := &limitedBuffer{max: c.MaxResponseBytes}
	var stderr limitedBuffer
	stderr.max = 64 << 10
	cmd.Stdout = stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	if ctx.Err() != nil {
		return nil, "", localError{CodeTimeout, "call did not finish in time: " + ctx.Err().Error()}
	}
	if runErr != nil {
		var ee *exec.ExitError
		if !errors.As(runErr, &ee) {
			return nil, "", localError{CodeCLI, fmt.Sprintf("cannot run %s: %v", c.Path, runErr)}
		}
		return stdout, stderr.String(), runErr
	}
	return stdout, stderr.String(), nil
}

// withEnv returns base with the KEY=VALUE entries of set applied; an entry
// with an empty value removes the variable instead of setting it empty.
func withEnv(base, set []string) []string {
	if len(set) == 0 {
		return base
	}
	override := map[string]bool{}
	for _, kv := range set {
		k, _, _ := strings.Cut(kv, "=")
		override[k] = true
	}
	out := make([]string, 0, len(base)+len(set))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if !override[k] {
			out = append(out, kv)
		}
	}
	for _, kv := range set {
		if k, v, _ := strings.Cut(kv, "="); k != "" && v != "" {
			out = append(out, kv)
		}
	}
	return out
}

// attachBody reads a streamed output file into member of the JSON result.
func (c *CLI) attachBody(result []byte, path, member string) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(result, &m); err != nil || m == nil {
		m = map[string]json.RawMessage{}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data := make([]byte, c.MaxBodyBytes+1)
	n, _ := readFull(f, data)
	data = data[:n]
	if int64(n) > c.MaxBodyBytes {
		data = data[:c.MaxBodyBytes]
		m[member+"Truncated"] = json.RawMessage("true")
	}
	var v any = string(data)
	if !utf8.Valid(data) {
		v = base64.StdEncoding.EncodeToString(data)
		m[member+"Encoding"] = json.RawMessage(`"base64"`)
	}
	enc, _ := json.Marshal(v)
	m[member] = enc
	return json.Marshal(m)
}

func readFull(f *os.File, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		k, err := f.Read(buf[n:])
		n += k
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func validName(s string) bool { return nameRE.MatchString(s) }

var awsErrorRE = regexp.MustCompile(`(?s)An error occurred \(([^)]+)\) when calling the (\w+) operation(?: \([^)]*\))?: (.*)`)

// ParseCLIError turns the CLI's stderr into an APIError. Service errors look
// like
//
//	An error occurred (AccessDenied) when calling the ListUsers operation: User ... is not authorized
//
// anything else (credentials, parameter validation, unknown commands) is given
// a local code.
func ParseCLIError(stderr string) *APIError {
	s := strings.TrimSpace(stderr)
	if m := awsErrorRE.FindStringSubmatch(s); m != nil {
		return &APIError{Code: m[1], Operation: m[2], Message: strings.TrimSpace(m[3])}
	}
	code := CodeCLI
	switch {
	case strings.Contains(s, "Unable to locate credentials"),
		strings.Contains(s, "Error loading SSO Token"),
		strings.Contains(s, "SSO session associated with this profile has expired"),
		strings.Contains(s, "Token has expired and refresh failed"),
		strings.Contains(s, "The config profile (") && strings.Contains(s, "could not be found"):
		code = CodeNoCredentials
	case strings.Contains(s, "Parameter validation failed"),
		strings.Contains(s, "Error parsing parameter"),
		strings.Contains(s, "Unknown parameter"),
		strings.Contains(s, "Invalid type for parameter"),
		strings.Contains(s, "Missing required parameter"):
		code = CodeParamValidation
	case strings.Contains(s, "Invalid choice"):
		code = CodeInvalidOperation
		s = "unknown service or operation for the AWS CLI (" + firstLine(lastInvalidChoice(s)) + ")"
	}
	if s == "" {
		s = "the AWS CLI exited with an error and no message"
	}
	return &APIError{Code: code, Message: truncate(s, 4000)}
}

func lastInvalidChoice(s string) string {
	if i := strings.Index(s, "Invalid choice"); i >= 0 {
		// Usage text precedes the message; keep the message and a few choices.
		return truncate(s[i:], 300)
	}
	return s
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// limitedBuffer keeps at most max bytes and remembers whether more arrived.
type limitedBuffer struct {
	bytes.Buffer
	max      int64
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	room := b.max - int64(b.Len())
	if int64(len(p)) > room {
		b.overflow = true
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}
