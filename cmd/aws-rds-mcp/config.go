package main

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/statiolake/aws-mcp/internal/mcpx"
)

// Config is the server configuration, from flags with AWS_RDS_MCP_*
// environment variables as defaults.
type Config struct {
	AWSCLI         string
	Region         string
	Profile        string
	CallTimeout    time.Duration
	MaxOutputBytes int
}

func parseConfig(args []string, getenv func(string) string, stderr io.Writer) (*Config, bool, error) {
	fs := flag.NewFlagSet("aws-rds-mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	e := mcpx.Env{Prefix: "AWS_RDS_MCP_", Getenv: getenv}
	c := &Config{}
	var version bool
	fs.StringVar(&c.AWSCLI, "aws-cli", e.String("AWS_CLI", "aws"), "path to the AWS CLI v2 executable [AWS_RDS_MCP_AWS_CLI]")
	fs.StringVar(&c.Region, "region", e.String("REGION", ""), "default region; empty uses the CLI's own resolution (AWS_REGION, profile) [AWS_RDS_MCP_REGION]")
	fs.StringVar(&c.Profile, "profile", e.String("PROFILE", ""), "default profile; empty uses the CLI's own resolution (AWS_PROFILE) [AWS_RDS_MCP_PROFILE]")
	fs.DurationVar(&c.CallTimeout, "call-timeout", e.Duration("CALL_TIMEOUT", 60*time.Second), "time limit for one Data API call [AWS_RDS_MCP_CALL_TIMEOUT]")
	fs.IntVar(&c.MaxOutputBytes, "max-output", e.Int("MAX_OUTPUT", 100000), "output size limit per tool call in bytes [AWS_RDS_MCP_MAX_OUTPUT]")
	fs.BoolVar(&version, "version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: aws-rds-mcp [flags]\n\nAn MCP server (stdio) for read-only SQL on Aurora / RDS through the RDS Data API.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, false, err
	}
	if fs.NArg() > 0 {
		return nil, false, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if c.MaxOutputBytes <= 0 {
		return nil, false, fmt.Errorf("--max-output must be positive")
	}
	return c, version, nil
}
