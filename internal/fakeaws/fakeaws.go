// Package fakeaws is a stand-in for the aws executable in tests. A test binary
// calls MaybeRun from TestMain; when it is started with FAKE_AWS=1 in its
// environment it behaves like a minimal AWS CLI, answering from canned rules
// and logging every invocation.
package fakeaws

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Rule is one canned answer. The first rule whose Service, Operation and
// Match all fit the invocation is used.
type Rule struct {
	Service   string `json:"service"`   // CLI service name, e.g. "s3api"
	Operation string `json:"operation"` // CLI operation name, e.g. "list-buckets"
	// Match is a subset of the --cli-input-json object that must be present
	// (deep equality per key).
	Match map[string]any `json:"match,omitempty"`
	// Absent lists input keys that must not be present.
	Absent []string `json:"absent,omitempty"`
	// Region, when set, must equal --region.
	Region string `json:"region,omitempty"`

	Stdout  any    `json:"stdout,omitempty"` // JSON value printed on stdout
	Stderr  string `json:"stderr,omitempty"`
	Exit    int    `json:"exit,omitempty"`
	SleepMS int    `json:"sleep_ms,omitempty"`
	// Body is written to the trailing positional output file (get-object).
	Body string `json:"body,omitempty"`
	// Echo prints {"input":..., "region":..., "profile":...} instead of Stdout.
	Echo bool `json:"echo,omitempty"`
	// Skeleton is printed for --generate-cli-skeleton input.
	Skeleton any `json:"skeleton,omitempty"`
	// Help is printed for "aws <service> <operation> help", but only when
	// PAGER is cat (the real CLI would otherwise start less) and AWS_PROFILE
	// is unset.
	Help string `json:"help,omitempty"`
}

// Invocation is one logged run of the fake CLI.
type Invocation struct {
	Args      []string       `json:"args"`
	Service   string         `json:"service"`
	Operation string         `json:"operation"`
	Input     map[string]any `json:"input"`
	Region    string         `json:"region"`
	Profile   string         `json:"profile"`
	Start     int64          `json:"start"`
	End       int64          `json:"end"`
}

// MaybeRun acts as the fake CLI and exits if FAKE_AWS=1; otherwise it returns.
func MaybeRun() {
	if os.Getenv("FAKE_AWS") != "1" {
		return
	}
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	start := time.Now().UnixNano()
	inv := Invocation{Args: args}
	var positional []string
	var rawInput, skeleton string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--cli-input-json", "--output", "--region", "--profile", "--cli-binary-format", "--generate-cli-skeleton":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "argument %s: expected one argument\n", a)
				return 252
			}
			v := args[i+1]
			i++
			switch a {
			case "--cli-input-json":
				rawInput = v
			case "--region":
				inv.Region = v
			case "--profile":
				inv.Profile = v
			case "--generate-cli-skeleton":
				skeleton = v
			}
		case "--no-paginate", "--no-cli-pager":
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) < 2 {
		fmt.Fprintln(os.Stderr, "usage: aws <service> <operation>")
		return 252
	}
	inv.Service, inv.Operation = positional[0], positional[1]
	if strings.HasPrefix(rawInput, "file://") {
		b, err := os.ReadFile(strings.TrimPrefix(rawInput, "file://"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 255
		}
		rawInput = string(b)
	}
	if rawInput != "" {
		if err := json.Unmarshal([]byte(rawInput), &inv.Input); err != nil {
			fmt.Fprintf(os.Stderr, "Error parsing parameter 'cli-input-json': Invalid JSON received.\n")
			return 252
		}
	}

	rules, err := loadRules()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 255
	}
	rule := find(rules, &inv)
	code := 0
	if rule == nil {
		fmt.Fprintf(os.Stderr, "\naws: error: argument operation: Invalid choice, valid choices are:\n\nfoo | bar\n")
		code = 252
	} else if len(positional) > 2 && positional[2] == "help" {
		if rule.Help == "" || os.Getenv("PAGER") != "cat" || os.Getenv("AWS_PROFILE") != "" {
			fmt.Fprintln(os.Stderr, "fakeaws: help needs Rule.Help, PAGER=cat and no AWS_PROFILE")
			code = 255
		} else {
			fmt.Print(rule.Help)
		}
	} else if skeleton != "" {
		if rule.Skeleton == nil || skeleton != "input" {
			fmt.Fprintln(os.Stderr, "fakeaws: no skeleton")
			code = 255
		} else {
			b, _ := json.MarshalIndent(rule.Skeleton, "", "    ")
			os.Stdout.Write(append(b, '\n'))
		}
	} else {
		if rule.SleepMS > 0 {
			time.Sleep(time.Duration(rule.SleepMS) * time.Millisecond)
		}
		if rule.Body != "" && len(positional) > 2 {
			os.WriteFile(positional[2], []byte(rule.Body), 0o600)
		}
		switch {
		case rule.Echo:
			b, _ := json.MarshalIndent(map[string]any{"input": inv.Input, "region": inv.Region, "profile": inv.Profile}, "", "    ")
			os.Stdout.Write(append(b, '\n'))
		case rule.Stdout != nil:
			b, _ := json.MarshalIndent(rule.Stdout, "", "    ")
			os.Stdout.Write(append(b, '\n'))
		}
		if rule.Stderr != "" {
			fmt.Fprint(os.Stderr, rule.Stderr)
		}
		code = rule.Exit
	}
	inv.End = time.Now().UnixNano()
	inv.Start = start
	if lp := os.Getenv("FAKE_AWS_LOG"); lp != "" {
		b, _ := json.Marshal(inv)
		f, err := os.OpenFile(lp, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			f.Write(append(b, '\n'))
			f.Close()
		}
	}
	return code
}

func loadRules() ([]Rule, error) {
	p := os.Getenv("FAKE_AWS_RULES")
	if p == "" {
		return nil, nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var rules []Rule
	return rules, json.Unmarshal(b, &rules)
}

func find(rules []Rule, inv *Invocation) *Rule {
next:
	for i := range rules {
		r := &rules[i]
		if r.Service != inv.Service || r.Operation != inv.Operation {
			continue
		}
		if r.Region != "" && r.Region != inv.Region {
			continue
		}
		for k, want := range r.Match {
			got, ok := inv.Input[k]
			if !ok || !jsonEqual(got, want) {
				continue next
			}
		}
		for _, k := range r.Absent {
			if _, ok := inv.Input[k]; ok {
				continue next
			}
		}
		return r
	}
	return nil
}

func jsonEqual(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	var x, y any
	json.Unmarshal(ja, &x)
	json.Unmarshal(jb, &y)
	return reflect.DeepEqual(x, y)
}

// Fake describes a configured fake CLI for one test.
type Fake struct {
	Path string   // executable to use as the aws CLI
	Env  []string // environment to add when running it
	log  string
	t    testing.TB
}

// New writes rules to a temporary directory and returns a Fake that runs the
// current test binary as the aws CLI. The test binary's TestMain must call
// MaybeRun.
func New(t testing.TB, rules []Rule) *Fake {
	t.Helper()
	dir := t.TempDir()
	rp := filepath.Join(dir, "rules.json")
	b, err := json.Marshal(rules)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	lp := filepath.Join(dir, "log.jsonl")
	return &Fake{
		Path: exe,
		Env:  []string{"FAKE_AWS=1", "FAKE_AWS_RULES=" + rp, "FAKE_AWS_LOG=" + lp},
		log:  lp,
		t:    t,
	}
}

// Invocations returns every logged run so far, in completion order.
func (f *Fake) Invocations() []Invocation {
	f.t.Helper()
	b, err := os.ReadFile(f.log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		f.t.Fatal(err)
	}
	var out []Invocation
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var inv Invocation
		if err := json.Unmarshal([]byte(line), &inv); err != nil {
			f.t.Fatalf("bad log line %q: %v", line, err)
		}
		out = append(out, inv)
	}
	return out
}
