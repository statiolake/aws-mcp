// Package script runs Starlark scripts that call AWS through a boto3-like
// module:
//
//	ec2 = aws.client("ec2", region="ap-northeast-1")
//	r = ec2.describe_instances(Filters=[{"Name": "instance-state-name", "Values": ["running"]}])
//
// Scripts are hermetic: the only way out of the interpreter is the aws module,
// which goes through a caller.Caller (and so through read-only mode, when the
// server wraps the Caller in readonly.Guard).
package script

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.starlark.net/lib/json"
	"go.starlark.net/lib/math"
	startime "go.starlark.net/lib/time"
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"

	"github.com/statiolake/aws-mcp/internal/caller"
)

// Limits bound what one script may do. Zero fields take the defaults below.
type Limits struct {
	MaxSteps       uint64        // Starlark execution steps
	Timeout        time.Duration // wall time for the whole script
	MaxOutputBytes int           // printed output plus final value
	MaxCalls       int           // AWS API calls (each page counts)
	MaxConcurrency int           // parallel calls inside aws.batch
	MaxPages       int           // default page limit for paginate
}

// DefaultLimits are used for zero fields of Limits.
var DefaultLimits = Limits{
	MaxSteps:       10_000_000,
	Timeout:        120 * time.Second,
	MaxOutputBytes: 100_000,
	MaxCalls:       100,
	MaxConcurrency: 8,
	MaxPages:       20,
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits
	if l.MaxSteps == 0 {
		l.MaxSteps = d.MaxSteps
	}
	if l.Timeout == 0 {
		l.Timeout = d.Timeout
	}
	if l.MaxOutputBytes == 0 {
		l.MaxOutputBytes = d.MaxOutputBytes
	}
	if l.MaxCalls == 0 {
		l.MaxCalls = d.MaxCalls
	}
	if l.MaxConcurrency == 0 {
		l.MaxConcurrency = d.MaxConcurrency
	}
	if l.MaxPages == 0 {
		l.MaxPages = d.MaxPages
	}
	return l
}

// Runner executes scripts against a Caller.
type Runner struct {
	Caller caller.Caller
	Limits Limits
}

// Env is per-run context: the default region and profile for aws.client.
type Env struct {
	Region  string
	Profile string
}

// Result is the outcome of one script.
type Result struct {
	Output    string // printed text, then the final value, possibly truncated
	Truncated bool
	Calls     int   // AWS API calls made
	Err       error // nil on success
}

// Text renders the result for the LLM.
func (r *Result) Text() string {
	var b strings.Builder
	b.WriteString(r.Output)
	if r.Err != nil {
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
			b.WriteByte('\n')
		}
		b.WriteString("Error: ")
		b.WriteString(r.Err.Error())
	}
	if b.Len() == 0 {
		b.WriteString("(no output; print() values or end the script with an expression)")
	}
	return b.String()
}

var fileOptions = &syntax.FileOptions{
	Set:             true,
	While:           true,
	TopLevelControl: true,
	GlobalReassign:  true,
	Recursion:       true,
}

const resultVar = "__result__"

// run holds the state of one script execution.
type run struct {
	ctx    context.Context
	r      *Runner
	limits Limits
	env    Env
	thread *starlark.Thread
	out    *outputBuffer
	calls  atomic.Int64
	mu     sync.Mutex // guards notes
	notes  []string
}

// Run executes src and returns its output. It never panics on script errors;
// they are reported in Result.Err.
func (r *Runner) Run(ctx context.Context, src string, env Env) *Result {
	limits := r.Limits.withDefaults()
	ctx, cancel := context.WithTimeout(ctx, limits.Timeout)
	defer cancel()

	st := &run{ctx: ctx, r: r, limits: limits, env: env, out: &outputBuffer{max: limits.MaxOutputBytes}}
	thread := &starlark.Thread{
		Name:  "script",
		Print: func(_ *starlark.Thread, msg string) { st.out.write(msg + "\n") },
		Load: func(*starlark.Thread, string) (starlark.StringDict, error) {
			return nil, errors.New("load() is not available; everything is predeclared (aws, json, math, time)")
		},
	}
	thread.SetMaxExecutionSteps(limits.MaxSteps)
	st.thread = thread

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				thread.Cancel(fmt.Sprintf("script exceeded the wall-time limit of %v", limits.Timeout))
			} else {
				thread.Cancel("script cancelled by the client")
			}
		case <-done:
		}
	}()

	predeclared := starlark.StringDict{
		"aws":  st.awsModule(),
		"json": json.Module,
		"math": math.Module,
		"time": startime.Module,
	}
	res := &Result{}
	err := st.exec(src, predeclared)
	if n := len(st.notes); n > 0 {
		for _, note := range st.notes {
			st.out.write(note + "\n")
		}
	}
	res.Output = st.out.String()
	res.Truncated = st.out.truncated
	res.Calls = int(st.calls.Load())
	res.Err = friendlyError(err, limits)
	return res
}

func (st *run) exec(src string, predeclared starlark.StringDict) error {
	f, err := fileOptions.Parse("script.star", src, 0)
	if err != nil {
		return err
	}
	// If the script ends with an expression, capture its value as the
	// script's result (like a notebook cell).
	captured := false
	if n := len(f.Stmts); n > 0 {
		if es, ok := f.Stmts[n-1].(*syntax.ExprStmt); ok {
			start, _ := es.X.Span()
			f.Stmts[n-1] = &syntax.AssignStmt{
				OpPos: start,
				Op:    syntax.EQ,
				LHS:   &syntax.Ident{NamePos: start, Name: resultVar},
				RHS:   es.X,
			}
			captured = true
		}
	}
	prog, err := starlark.FileProgram(f, predeclared.Has)
	if err != nil {
		return err
	}
	globals, err := prog.Init(st.thread, predeclared)
	if err != nil {
		return err
	}
	if captured {
		if v, ok := globals[resultVar]; ok && v != starlark.None {
			st.out.write(formatValue(v) + "\n")
		}
	}
	return nil
}

// formatValue renders a final value: strings as-is, JSON-compatible values as
// compact JSON, anything else by its Starlark repr.
func formatValue(v starlark.Value) string {
	if s, ok := v.(starlark.String); ok {
		return string(s)
	}
	if b, err := toJSON(v); err == nil {
		return string(b)
	}
	return v.String()
}

func friendlyError(err error, limits Limits) error {
	if err == nil {
		return nil
	}
	var ee *starlark.EvalError
	if errors.As(err, &ee) {
		msg := ee.Backtrace()
		if strings.Contains(ee.Msg, "too many steps") {
			msg += fmt.Sprintf("\n(the script exceeded the limit of %d execution steps)", limits.MaxSteps)
		}
		return errors.New(msg)
	}
	return err
}

// outputBuffer collects output up to max bytes.
type outputBuffer struct {
	mu        sync.Mutex
	b         strings.Builder
	max       int
	total     int
	truncated bool
}

func (o *outputBuffer) write(s string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.total += len(s)
	if o.truncated {
		return
	}
	room := o.max - o.b.Len()
	if len(s) <= room {
		o.b.WriteString(s)
		return
	}
	if room > 0 {
		o.b.WriteString(s[:room])
	}
	o.truncated = true
}

func (o *outputBuffer) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.truncated {
		return o.b.String()
	}
	return o.b.String() + fmt.Sprintf("\n[output truncated: the script produced %d bytes, the limit is %d. Print less: select fields, aggregate, or slice lists.]\n", o.total, o.max)
}
