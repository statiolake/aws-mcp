package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/statiolake/aws-mcp/internal/mcpx"
	"github.com/statiolake/aws-mcp/internal/script"
)

// Config is the server configuration, from flags with AWS_MCP_* environment
// variables as defaults.
type Config struct {
	AWSCLI      string
	Region      string
	Profile     string
	ReadOnly    bool
	CallTimeout time.Duration
	Limits      script.Limits
	MaxBody     int64
}

func parseConfig(args []string, getenv func(string) string, stderr io.Writer) (*Config, bool, error) {
	fs := flag.NewFlagSet("aws-mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	e := mcpx.Env{Prefix: "AWS_MCP_", Getenv: getenv}
	env, envBool, envDur, envInt := e.String, e.Bool, e.Duration, e.Int
	d := script.DefaultLimits
	c := &Config{}
	var allowWrites, version bool
	var maxSteps int
	fs.StringVar(&c.AWSCLI, "aws-cli", env("AWS_CLI", "aws"), "path to the AWS CLI v2 executable [AWS_MCP_AWS_CLI]")
	fs.StringVar(&c.Region, "region", env("REGION", ""), "default region; empty uses the CLI's own resolution (AWS_REGION, profile) [AWS_MCP_REGION]")
	fs.StringVar(&c.Profile, "profile", env("PROFILE", ""), "default profile; empty uses the CLI's own resolution (AWS_PROFILE) [AWS_MCP_PROFILE]")
	fs.BoolVar(&c.ReadOnly, "read-only", envBool("READ_ONLY", true), "allow only read-only operations (Describe*, List*, Get*, ...) [AWS_MCP_READ_ONLY]")
	fs.BoolVar(&allowWrites, "allow-writes", envBool("ALLOW_WRITES", false), "allow every operation; same as --read-only=false [AWS_MCP_ALLOW_WRITES]")
	fs.DurationVar(&c.CallTimeout, "call-timeout", envDur("CALL_TIMEOUT", 60*time.Second), "time limit for one AWS call [AWS_MCP_CALL_TIMEOUT]")
	fs.DurationVar(&c.Limits.Timeout, "script-timeout", envDur("SCRIPT_TIMEOUT", d.Timeout), "wall-time limit for one script [AWS_MCP_SCRIPT_TIMEOUT]")
	fs.IntVar(&maxSteps, "max-steps", envInt("MAX_STEPS", int(d.MaxSteps)), "Starlark execution step limit per script [AWS_MCP_MAX_STEPS]")
	fs.IntVar(&c.Limits.MaxOutputBytes, "max-output", envInt("MAX_OUTPUT", d.MaxOutputBytes), "output size limit per tool call in bytes [AWS_MCP_MAX_OUTPUT]")
	fs.IntVar(&c.Limits.MaxCalls, "max-calls", envInt("MAX_CALLS", d.MaxCalls), "AWS API call limit per script [AWS_MCP_MAX_CALLS]")
	fs.IntVar(&c.Limits.MaxConcurrency, "concurrency", envInt("CONCURRENCY", d.MaxConcurrency), "parallel AWS CLI processes (aws.batch and overall) [AWS_MCP_CONCURRENCY]")
	fs.IntVar(&c.Limits.MaxPages, "max-pages", envInt("MAX_PAGES", d.MaxPages), "default page limit for paginate [AWS_MCP_MAX_PAGES]")
	var maxBody int
	fs.IntVar(&maxBody, "max-body", envInt("MAX_BODY", 1<<20), "bytes of a streamed body (S3 GetObject) returned [AWS_MCP_MAX_BODY]")
	fs.BoolVar(&version, "version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: aws-mcp [flags]\n\nAn MCP server (stdio) for calling AWS APIs through the AWS CLI v2.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, false, err
	}
	if fs.NArg() > 0 {
		return nil, false, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if allowWrites {
		c.ReadOnly = false
	}
	if maxSteps <= 0 || c.Limits.MaxOutputBytes <= 0 || c.Limits.MaxCalls <= 0 || c.Limits.MaxConcurrency <= 0 || c.Limits.MaxPages <= 0 || maxBody <= 0 {
		return nil, false, fmt.Errorf("limits must be positive")
	}
	c.Limits.MaxSteps = uint64(maxSteps)
	c.MaxBody = int64(maxBody)
	return c, version, nil
}

func defaultGetenv(k string) string { return os.Getenv(k) }
