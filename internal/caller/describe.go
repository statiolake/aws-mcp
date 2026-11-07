package caller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strings"

	"github.com/statiolake/aws-mcp/internal/awsname"
)

// Skeleton returns the operation's input skeleton as the installed CLI
// prints it with
//
//	aws <service> <operation> --generate-cli-skeleton input
//
// i.e. every request member with a placeholder value of its type. The CLI
// builds it from its bundled service model without calling AWS, so it needs
// no credentials and always matches the installed CLI version.
func (c *CLI) Skeleton(ctx context.Context, service, operation string) (json.RawMessage, error) {
	svc, op, err := c.cliNames(service, operation)
	if err != nil {
		return nil, err
	}
	stdout, stderr, err := c.run(ctx, []string{svc, op, "--generate-cli-skeleton", "input"}, nil)
	if err != nil {
		return nil, c.describeError(service, operation, stderr, err)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, bytes.TrimSpace(stdout.Bytes())); err != nil {
		return nil, &APIError{Service: service, Operation: awsname.PascalOperation(operation), Code: CodeCLI,
			Message: "the CLI printed an invalid skeleton: " + truncate(stdout.String(), 300)}
	}
	return buf.Bytes(), nil
}

// Help returns the CLI's reference page for the operation (aws <service>
// <operation> help) as plain text.
//
// The AWS CLI v2 renders help without checking for a terminal: on Linux and
// macOS it formats the page with groff or mandoc (and fails if neither is
// installed) and pipes it to MANPAGER, PAGER or "less -R"; on Windows it
// produces plain text and pipes it to "more". Here the pager is cat on
// POSIX systems, the bold/underline sequences of the formatter are removed,
// and the CLI config file is hidden so a profile's cli_help_output=browser
// setting cannot open a browser (help needs no configuration or
// credentials). The SYNOPSIS and GLOBAL OPTIONS sections are dropped and
// EXAMPLES is moved last.
func (c *CLI) Help(ctx context.Context, service, operation string) (string, error) {
	svc, op, err := c.cliNames(service, operation)
	if err != nil {
		return "", err
	}
	env := []string{"AWS_CONFIG_FILE=" + os.DevNull, "AWS_PROFILE=", "AWS_DEFAULT_PROFILE="}
	if runtime.GOOS != "windows" {
		env = append(env, "PAGER=cat", "MANPAGER=cat", "GROFF_NO_SGR=1")
	}
	stdout, stderr, err := c.run(ctx, []string{svc, op, "help"}, env)
	if err != nil {
		if strings.Contains(stderr, "groff") {
			stderr += " (the AWS CLI needs groff or mandoc installed to render help)"
		}
		return "", c.describeError(service, operation, stderr, err)
	}
	return TrimHelp(stdout.String()), nil
}

func (c *CLI) cliNames(service, operation string) (string, string, error) {
	svc, op := awsname.CLIService(service), awsname.CLIOperation(operation)
	if !validName(svc) || !validName(op) {
		return "", "", &APIError{Service: service, Operation: operation, Code: CodeInvalidOperation,
			Message: fmt.Sprintf("invalid service or operation name %q %q", service, operation)}
	}
	return svc, op, nil
}

func (c *CLI) describeError(service, operation, stderr string, err error) error {
	if le, ok := err.(localError); ok {
		return &APIError{Service: service, Operation: awsname.PascalOperation(operation), Code: le.code, Message: le.msg}
	}
	e := ParseCLIError(stderr)
	e.Service = service
	e.Operation = awsname.PascalOperation(operation)
	return e
}

var (
	// "X\bX" (bold) and "_\bX" (underline) overstrikes from grotty.
	overstrikeRE = regexp.MustCompile(".\x08")
	// SGR and other CSI escape sequences.
	csiRE = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]")
	// A section heading of a rendered man page: an upper-case line at
	// column 0.
	headingRE = regexp.MustCompile(`^[A-Z][A-Z ]+$`)
	// The man page's header/footer line: "DESCRIBE-INSTANCES()   DESCRIBE-INSTANCES()".
	pageTitleRE = regexp.MustCompile(`^\s*[A-Z0-9-]+\(\)(\s+[A-Z0-9-]+\(\))?\s*$`)
)

// TrimHelp turns rendered CLI help into compact plain text; see Help.
func TrimHelp(s string) string {
	s = csiRE.ReplaceAllString(s, "")
	s = overstrikeRE.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r", "")

	type section struct {
		name  string
		lines []string
	}
	var sections []*section
	cur := &section{}
	sections = append(sections, cur)
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, " \t")
		if pageTitleRE.MatchString(line) {
			continue
		}
		if headingRE.MatchString(line) {
			cur = &section{name: line}
			sections = append(sections, cur)
			continue
		}
		cur.lines = append(cur.lines, line)
	}

	var examples *section
	var b strings.Builder
	write := func(sec *section) {
		body := dedent(sec.lines)
		if body == "" {
			return
		}
		if sec.name != "" {
			b.WriteString(sec.name + "\n")
		}
		b.WriteString(body + "\n\n")
	}
	for _, sec := range sections {
		switch sec.name {
		case "SYNOPSIS", "GLOBAL OPTIONS":
		case "EXAMPLES":
			examples = sec
		default:
			write(sec)
		}
	}
	if examples != nil {
		write(examples)
	}
	return strings.TrimSpace(b.String())
}

// dedent removes the common indentation of lines and collapses runs of blank
// lines.
func dedent(lines []string) string {
	min := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " "))
		if min < 0 || n < min {
			min = n
		}
	}
	var out []string
	blank := true
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			if !blank {
				out = append(out, "")
			}
			blank = true
			continue
		}
		blank = false
		out = append(out, l[min:])
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
