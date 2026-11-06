// Package awsname converts between the spellings of AWS service and operation
// names used by boto3 (snake_case), the API itself (PascalCase) and the AWS
// CLI (kebab-case).
package awsname

import (
	"regexp"
	"strings"
)

// cliServiceNames maps boto3 client names to AWS CLI command names where the
// two differ. The CLI's "s3" command is the high-level file interface; the
// one-to-one API commands live under "s3api".
var cliServiceNames = map[string]string{
	"s3":         "s3api",
	"config":     "configservice",
	"codedeploy": "deploy",
}

// CLIService returns the AWS CLI command name for a boto3-style service name.
func CLIService(service string) string {
	s := strings.ToLower(strings.TrimSpace(service))
	if c, ok := cliServiceNames[s]; ok {
		return c
	}
	return s
}

// CanonicalService returns the CLI spelling of a service name; it is used as
// the key for per-service rules so "s3" and "s3api" are treated alike.
func CanonicalService(service string) string { return CLIService(service) }

var (
	// A capital followed by lower-case letters starts a new word: "DBInstances"
	// -> "DB_Instances".
	firstCap = regexp.MustCompile(`(.)([A-Z][a-z]+)`)
	// A lower-case letter or digit followed by a capital: "describeDB" ->
	// "describe_DB".
	endCap = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	// A trailing plural acronym such as "ARNs" or "ACLs" stays one word.
	pluralAcronym = regexp.MustCompile(`[A-Z]{2,}s$`)
)

// Words splits an operation name in any of the three spellings into its
// lower-case words: "DescribeDBInstances", "describe_db_instances" and
// "describe-db-instances" all give ["describe", "db", "instances"].
func Words(op string) []string {
	op = strings.TrimSpace(op)
	if strings.ContainsAny(op, "_-") {
		f := strings.FieldsFunc(strings.ToLower(op), func(r rune) bool { return r == '_' || r == '-' })
		return f
	}
	if m := pluralAcronym.FindString(op); m != "" && len(m) < len(op) {
		op = op[:len(op)-len(m)] + "_" + strings.ToLower(m)
	}
	s := firstCap.ReplaceAllString(op, "${1}_${2}")
	s = endCap.ReplaceAllString(s, "${1}_${2}")
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return r == '_' })
}

// CLIOperation returns the kebab-case CLI spelling of an operation name.
func CLIOperation(op string) string { return strings.Join(Words(op), "-") }

// SnakeOperation returns the boto3 (snake_case) spelling of an operation name.
func SnakeOperation(op string) string { return strings.Join(Words(op), "_") }

// PascalOperation returns a best-effort PascalCase spelling, used only for
// messages. Acronyms cannot be recovered from snake_case, so
// "describe_db_instances" becomes "DescribeDbInstances".
func PascalOperation(op string) string {
	if !strings.ContainsAny(op, "_-") && op != "" && op[0] >= 'A' && op[0] <= 'Z' {
		return op
	}
	var b strings.Builder
	for _, w := range Words(op) {
		b.WriteString(strings.ToUpper(w[:1]))
		b.WriteString(w[1:])
	}
	return b.String()
}
