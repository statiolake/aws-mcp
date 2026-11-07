package caller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/statiolake/aws-mcp/internal/fakeaws"
)

// bold renders s the way grotty does for bold text in -T ascii mode.
func bold(s string) string {
	var b strings.Builder
	for _, r := range s {
		b.WriteString(string(r) + "\b" + string(r))
	}
	return b.String()
}

// sampleHelp is shaped like `aws <svc> <op> help` output rendered by groff.
var sampleHelp = "GET-WIDGET()                                                      GET-WIDGET()\n\n\n\n" +
	bold("NAME") + "\n       get-widget -\n\n" +
	bold("DESCRIPTION") + "\n       Returns one widget.\n\n       See also: AWS API Documentation\n\n" +
	bold("SYNOPSIS") + "\n            get-widget\n          --widget-id <value>\n\n" +
	bold("OPTIONS") + "\n       " + bold("--widget-id") + " (string)\n          The widget's ID.\n\n\n\n" +
	"\x1b[1mGLOBAL OPTIONS\x1b[0m\n       " + bold("--debug") + " (boolean)\n\n" +
	bold("EXAMPLES") + "\n       To get a widget\n\n          aws widgets get-widget --widget-id w-1\n\n" +
	bold("OUTPUT") + "\n       Widget -> (structure)\n          The widget.\n\n\n\n" +
	"                                                                  GET-WIDGET()\n"

func TestTrimHelp(t *testing.T) {
	got := TrimHelp(sampleHelp)
	want := `NAME
get-widget -

DESCRIPTION
Returns one widget.

See also: AWS API Documentation

OPTIONS
--widget-id (string)
   The widget's ID.

OUTPUT
Widget -> (structure)
   The widget.

EXAMPLES
To get a widget

   aws widgets get-widget --widget-id w-1`
	if got != want {
		t.Fatalf("TrimHelp =\n%s\n---\nwant\n%s", got, want)
	}
}

func TestSkeletonAndHelp(t *testing.T) {
	f := fakeaws.New(t, []fakeaws.Rule{{Service: "widgets", Operation: "get-widget",
		Skeleton: map[string]any{"WidgetId": "", "IncludeParts": true}, Help: sampleHelp}})
	c := newCLI(f, func(c *CLI) { c.Env = append(c.Env, "AWS_PROFILE=dev") })
	ctx := context.Background()
	sk, err := c.Skeleton(ctx, "widgets", "GetWidget")
	if err != nil || string(sk) != `{"IncludeParts":true,"WidgetId":""}` {
		t.Fatalf("Skeleton = %s, %v", sk, err)
	}
	h, err := c.Help(ctx, "widgets", "get_widget")
	if err != nil || !strings.HasPrefix(h, "NAME\nget-widget") || strings.Contains(h, "GLOBAL") || strings.Contains(h, "\b") {
		t.Fatalf("Help = %q, %v", h, err)
	}
	inv := f.Invocations()
	if len(inv) != 2 || strings.Join(inv[0].Args, " ") != "widgets get-widget --generate-cli-skeleton input" ||
		strings.Join(inv[1].Args, " ") != "widgets get-widget help" {
		t.Fatalf("invocations = %+v", inv)
	}

	var ae *APIError
	if _, err := c.Skeleton(ctx, "widgets", "DeleteWidget"); !errors.As(err, &ae) || ae.Code != CodeInvalidOperation {
		t.Fatalf("unknown op: %v", err)
	}
	if _, err := c.Help(ctx, "widgets; rm", "x"); !errors.As(err, &ae) || ae.Code != CodeInvalidOperation {
		t.Fatalf("bad name: %v", err)
	}
}
