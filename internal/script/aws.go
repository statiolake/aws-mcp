package script

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"

	"github.com/statiolake/aws-mcp/internal/awsname"
	"github.com/statiolake/aws-mcp/internal/caller"
)

func (st *run) awsModule() *starlarkstruct.Module {
	optStr := func(s string) starlark.Value {
		if s == "" {
			return starlark.None
		}
		return starlark.String(s)
	}
	return &starlarkstruct.Module{
		Name: "aws",
		Members: starlark.StringDict{
			"client":   starlark.NewBuiltin("aws.client", st.builtinClient),
			"batch":    starlark.NewBuiltin("aws.batch", st.builtinBatch),
			"try_call": starlark.NewBuiltin("aws.try_call", st.builtinTryCall),
			"region":   optStr(st.env.Region),
			"profile":  optStr(st.env.Profile),
		},
	}
}

func (st *run) builtinClient(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var service string
	var region, profile starlark.Value = starlark.None, starlark.None
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "service", &service, "region?", &region, "profile?", &profile); err != nil {
		return nil, err
	}
	c := &client{run: st, service: strings.TrimSpace(service), region: st.env.Region, profile: st.env.Profile}
	if c.service == "" {
		return nil, fmt.Errorf("%s: service name is empty", b.Name())
	}
	if s, ok := region.(starlark.String); ok {
		c.region = string(s)
	} else if region != starlark.None {
		return nil, fmt.Errorf("%s: region must be a string or None", b.Name())
	}
	if s, ok := profile.(starlark.String); ok {
		c.profile = string(s)
	} else if profile != starlark.None {
		return nil, fmt.Errorf("%s: profile must be a string or None", b.Name())
	}
	return c, nil
}

// call performs one counted API call.
func (st *run) call(c *client, op string, params []byte) (starlark.Value, error) {
	raw, err := st.rawCall(c, op, params)
	if err != nil {
		return nil, err
	}
	return fromJSON(raw)
}

func (st *run) reserveCalls(n int) error {
	if got := st.calls.Add(int64(n)); got > int64(st.limits.MaxCalls) {
		st.calls.Add(int64(-n))
		return &limitError{fmt.Sprintf("the script exceeded the limit of %d AWS API calls (each page counts); fetch less or use filters", st.limits.MaxCalls)}
	}
	return nil
}

func (st *run) rawCall(c *client, op string, params []byte) (json.RawMessage, error) {
	if err := st.reserveCalls(1); err != nil {
		return nil, err
	}
	return st.r.Caller.Call(st.ctx, &caller.Request{
		Service: c.service, Operation: op, Params: params, Region: c.region, Profile: c.profile,
	})
}

// limitError is a script limit; aws.try_call does not catch it.
type limitError struct{ msg string }

func (e *limitError) Error() string { return e.msg }

// kwargsToJSON turns call keyword arguments into the request object.
func kwargsToJSON(kwargs []starlark.Tuple) ([]byte, error) {
	d := starlark.NewDict(len(kwargs))
	for _, kv := range kwargs {
		d.SetKey(kv[0], kv[1])
	}
	return toJSON(d)
}

// builtinBatch runs several calls concurrently.
//
//	aws.batch([(ec2, "describe_vpcs"), (s3, "list_buckets", {})], return_errors=False)
func (st *run) builtinBatch(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var calls starlark.Iterable
	returnErrors := false
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "calls", &calls, "return_errors?", &returnErrors); err != nil {
		return nil, err
	}
	type job struct {
		c      *client
		op     string
		params []byte
	}
	var jobs []job
	it := calls.Iterate()
	defer it.Done()
	var x starlark.Value
	for i := 0; it.Next(&x); i++ {
		t, ok := x.(starlark.Tuple)
		if !ok {
			if l, isList := x.(*starlark.List); isList {
				t = make(starlark.Tuple, l.Len())
				for j := range t {
					t[j] = l.Index(j)
				}
			} else {
				return nil, fmt.Errorf("%s: item %d must be a tuple (client, operation[, params]), got %s", b.Name(), i, x.Type())
			}
		}
		if len(t) < 2 || len(t) > 3 {
			return nil, fmt.Errorf("%s: item %d must be (client, operation[, params])", b.Name(), i)
		}
		c, ok := t[0].(*client)
		if !ok {
			return nil, fmt.Errorf("%s: item %d: first element must be an aws.client, got %s", b.Name(), i, t[0].Type())
		}
		op, ok := starlark.AsString(t[1])
		if !ok {
			return nil, fmt.Errorf("%s: item %d: operation must be a string", b.Name(), i)
		}
		params := []byte("{}")
		if len(t) == 3 && t[2] != starlark.None {
			d, ok := t[2].(*starlark.Dict)
			if !ok {
				return nil, fmt.Errorf("%s: item %d: params must be a dict", b.Name(), i)
			}
			p, err := toJSON(d)
			if err != nil {
				return nil, fmt.Errorf("%s: item %d: %v", b.Name(), i, err)
			}
			params = p
		}
		jobs = append(jobs, job{c, op, params})
	}
	if err := st.reserveCalls(len(jobs)); err != nil {
		return nil, err
	}

	raws := make([]json.RawMessage, len(jobs))
	errs := make([]error, len(jobs))
	sem := make(chan struct{}, st.limits.MaxConcurrency)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			raws[i], errs[i] = st.r.Caller.Call(st.ctx, &caller.Request{
				Service: j.c.service, Operation: j.op, Params: j.params, Region: j.c.region, Profile: j.c.profile,
			})
		}()
	}
	wg.Wait()

	out := make([]starlark.Value, len(jobs))
	for i := range jobs {
		if errs[i] != nil {
			if !returnErrors {
				return nil, fmt.Errorf("batch item %d: %w", i, errs[i])
			}
			out[i] = errorResponse(errs[i])
			continue
		}
		v, err := fromJSON(raws[i])
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return starlark.NewList(out), nil
}

// errorResponse mirrors the shape of a boto3 ClientError response:
// {"Error": {"Code": ..., "Message": ...}}.
func errorResponse(err error) starlark.Value {
	e := starlark.NewDict(4)
	var ae *caller.APIError
	if errors.As(err, &ae) {
		e.SetKey(starlark.String("Code"), starlark.String(ae.Code))
		e.SetKey(starlark.String("Message"), starlark.String(ae.Message))
		e.SetKey(starlark.String("Service"), starlark.String(ae.Service))
		e.SetKey(starlark.String("Operation"), starlark.String(ae.Operation))
	} else {
		e.SetKey(starlark.String("Code"), starlark.String("Error"))
		e.SetKey(starlark.String("Message"), starlark.String(err.Error()))
	}
	d := starlark.NewDict(1)
	d.SetKey(starlark.String("Error"), e)
	return d
}

// builtinTryCall calls fn and returns (value, None), or (None, error) if an
// AWS call inside it failed. Starlark has no try/except.
func (st *run) builtinTryCall(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("%s: missing function argument", b.Name())
	}
	fn, ok := args[0].(starlark.Callable)
	if !ok {
		return nil, fmt.Errorf("%s: first argument must be callable, got %s", b.Name(), args[0].Type())
	}
	v, err := starlark.Call(thread, fn, args[1:], kwargs)
	if err == nil {
		return starlark.Tuple{v, starlark.None}, nil
	}
	var ae *caller.APIError
	var le *limitError
	if errors.As(err, &le) || !errors.As(err, &ae) {
		return nil, err
	}
	resp := errorResponse(ae).(*starlark.Dict)
	inner, _, _ := resp.Get(starlark.String("Error"))
	return starlark.Tuple{starlark.None, inner}, nil
}

// client is the value returned by aws.client(...). Any attribute that is not
// one of its fixed members is an API operation.
type client struct {
	run             *run
	service         string
	region, profile string
}

var _ starlark.HasAttrs = (*client)(nil)

func (c *client) String() string {
	s := fmt.Sprintf("<aws.client %s", c.service)
	if c.region != "" {
		s += " region=" + c.region
	}
	if c.profile != "" {
		s += " profile=" + c.profile
	}
	return s + ">"
}
func (c *client) Type() string          { return "aws.client" }
func (c *client) Freeze()               {}
func (c *client) Truth() starlark.Bool  { return true }
func (c *client) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable: aws.client") }

var clientFixed = []string{"call", "paginate", "profile", "region", "service"}

func (c *client) AttrNames() []string {
	n := append([]string(nil), clientFixed...)
	sort.Strings(n)
	return n
}

func (c *client) Attr(name string) (starlark.Value, error) {
	switch name {
	case "service":
		return starlark.String(c.service), nil
	case "region":
		return optString(c.region), nil
	case "profile":
		return optString(c.profile), nil
	case "call":
		return starlark.NewBuiltin(c.service+".call", c.builtinCall), nil
	case "paginate":
		return starlark.NewBuiltin(c.service+".paginate", c.builtinPaginate), nil
	}
	if strings.HasPrefix(name, "_") {
		return nil, nil
	}
	op := name
	return starlark.NewBuiltin(c.service+"."+name, func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		if len(args) > 0 {
			return nil, fmt.Errorf("%s: API parameters must be keyword arguments, e.g. %s(InstanceIds=[...])", b.Name(), b.Name())
		}
		params, err := kwargsToJSON(kwargs)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", b.Name(), err)
		}
		return c.run.call(c, op, params)
	}), nil
}

func optString(s string) starlark.Value {
	if s == "" {
		return starlark.None
	}
	return starlark.String(s)
}

// builtinCall is client.call("DescribeInstances", **params): the operation
// name may be in any spelling.
func (c *client) builtinCall(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("%s: want exactly one positional argument (the operation name)", b.Name())
	}
	op, ok := starlark.AsString(args[0])
	if !ok {
		return nil, fmt.Errorf("%s: operation must be a string", b.Name())
	}
	params, err := kwargsToJSON(kwargs)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", b.Name(), err)
	}
	return c.run.call(c, op, params)
}

// pagination options accepted by paginate in addition to API parameters.
const (
	optMaxPages    = "_max_pages"
	optInputToken  = "_input_token"
	optOutputToken = "_output_token"
)

// builtinPaginate is client.paginate("describe_instances", **params). It
// fetches pages until the response carries no continuation token, or
// _max_pages pages, and returns the list of pages.
func (c *client) builtinPaginate(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("%s: want exactly one positional argument (the operation name)", b.Name())
	}
	op, ok := starlark.AsString(args[0])
	if !ok {
		return nil, fmt.Errorf("%s: operation must be a string", b.Name())
	}
	maxPages := c.run.limits.MaxPages
	var spec *tokenSpec
	var inTok, outTok []string
	var rest []starlark.Tuple
	for _, kv := range kwargs {
		k := string(kv[0].(starlark.String))
		switch k {
		case optMaxPages:
			n, err := starlark.AsInt32(kv[1])
			if err != nil || n < 1 {
				return nil, fmt.Errorf("%s: %s must be a positive int", b.Name(), k)
			}
			maxPages = n
		case optInputToken, optOutputToken:
			names, err := stringOrList(kv[1])
			if err != nil {
				return nil, fmt.Errorf("%s: %s: %v", b.Name(), k, err)
			}
			if k == optInputToken {
				inTok = names
			} else {
				outTok = names
			}
		default:
			rest = append(rest, kv)
		}
	}
	if (inTok == nil) != (outTok == nil) || len(inTok) != len(outTok) {
		if inTok != nil || outTok != nil {
			return nil, fmt.Errorf("%s: give both %s and %s with the same number of names", b.Name(), optInputToken, optOutputToken)
		}
	}
	if inTok != nil {
		spec = &tokenSpec{in: inTok, out: outTok}
	}

	params := map[string]json.RawMessage{}
	var order []string
	for _, kv := range rest {
		v, err := toJSON(kv[1])
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %v", b.Name(), kv[0], err)
		}
		k := string(kv[0].(starlark.String))
		params[k] = v
		order = append(order, k)
	}

	var pages []starlark.Value
	var prev []string
	for page := 0; ; page++ {
		if page >= maxPages {
			c.run.note(fmt.Sprintf("[paginate %s.%s: stopped after %d pages; more are available — pass _max_pages=N to fetch more]",
				c.service, awsname.SnakeOperation(op), maxPages))
			break
		}
		body, err := encodeParams(order, params)
		if err != nil {
			return nil, err
		}
		raw, err := c.run.rawCall(c, op, body)
		if err != nil {
			return nil, err
		}
		v, err := fromJSON(raw)
		if err != nil {
			return nil, err
		}
		pages = append(pages, v)
		var out map[string]json.RawMessage
		if err := json.Unmarshal(raw, &out); err != nil {
			break
		}
		s := spec
		if s == nil {
			s = detectToken(out)
		}
		if s == nil {
			break
		}
		vals, ok := s.values(out)
		if !ok || equalStrings(vals, prev) {
			break
		}
		prev = vals
		for i, in := range s.in {
			if _, seen := params[in]; !seen {
				order = append(order, in)
			}
			params[in] = json.RawMessage(vals[i])
		}
	}
	return starlark.NewList(pages), nil
}

func (st *run) note(s string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.notes = append(st.notes, s)
}

func encodeParams(order []string, params map[string]json.RawMessage) ([]byte, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range order {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		b.Write(params[k])
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

func stringOrList(v starlark.Value) ([]string, error) {
	if s, ok := starlark.AsString(v); ok {
		return []string{s}, nil
	}
	l, ok := v.(starlark.Indexable)
	if !ok {
		return nil, fmt.Errorf("want a string or a list of strings")
	}
	var out []string
	for i := 0; i < l.Len(); i++ {
		s, ok := starlark.AsString(l.Index(i))
		if !ok {
			return nil, fmt.Errorf("want a string or a list of strings")
		}
		out = append(out, s)
	}
	return out, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
