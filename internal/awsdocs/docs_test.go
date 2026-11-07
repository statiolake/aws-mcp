package awsdocs

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

const wantPage = "# Creating a widget\n\n" +
	"A *widget* holds your **parts**. Use `CreateWidget` or see [the API reference](https://docs.aws.amazon.com/widgets/latest/api/API_CreateWidget.html).\n\n" +
	"Note\n\n" +
	"Widget names are unique per Region.\nThey cannot be changed.\n\n" +
	"## Steps\n\n" +
	"1. Open the console.\n" +
	"2. Choose **Create**.\n" +
	"   - Pick a size.\n" +
	"   - Pick a colour.\n\n" +
	"```json\n{\n    \"WidgetName\": \"my-widget\",\n    \"Size\": 3\n}\n```\n\n" +
	"## Request parameters\n\n" +
	"- **`WidgetName`**\n  The widget's name.\n  Type: String\n" +
	"- **Size**\n  Number of parts.\n\n" +
	"| Limit | Value |\n| --- | --- |\n| Widgets per account | 100 \\| soft |"

func TestToMarkdown(t *testing.T) {
	f, err := os.Open("testdata/page.html")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	doc, err := html.Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse("https://docs.aws.amazon.com/widgets/latest/guide/create.html")
	title, md := ToMarkdown(doc, base)
	if title != "Creating a widget - Example Widget Service" {
		t.Errorf("title = %q", title)
	}
	if md != wantPage {
		t.Errorf("markdown =\n%s\n---\nwant\n%s", md, wantPage)
	}
	for _, bad := range []string{"tracking", "Terms of use", "Did this page", "Introduction", "Previous", "JavaScript", "PDF", "diagram"} {
		if strings.Contains(md, bad) {
			t.Errorf("markdown contains %q", bad)
		}
	}
}

func TestToMarkdownFallbacks(t *testing.T) {
	doc, _ := html.Parse(strings.NewReader(`<html><head><title>T</title></head><body><nav>menu</nav><main><p>Body <a href="#x">anchor</a> <a href="javascript:void(0)">js</a></p><blockquote><p>q1</p><p>q2</p></blockquote><pre>a ` + "```" + ` b</pre></main></body></html>`))
	base, _ := url.Parse("https://docs.aws.amazon.com/x/")
	_, md := ToMarkdown(doc, base)
	want := "# T\n\nBody anchor js\n\n> q1\n>\n> q2\n\n````\na ``` b\n````"
	if md != want {
		t.Errorf("markdown =\n%q\nwant\n%q", md, want)
	}
}

// fakeTransport routes every request to h, so the real URLs (and the host
// check) are exercised without network access.
type fakeTransport struct{ h http.Handler }

func (f fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, r)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	return New(Options{HTTP: &http.Client{Transport: fakeTransport{h}}})
}

func TestSearch(t *testing.T) {
	var got map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Host != "proxy.search.docs.aws.amazon.com" || r.URL.Path != "/search" || r.URL.Query().Get("session") == "" {
			t.Errorf("request %s %s", r.Method, r.URL)
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		io.WriteString(w, `{"suggestions":[
			{"textExcerptSuggestion":{"link":"https://docs.aws.amazon.com/a.html","title":"A  title","summary":"First\n summary","suggestionBody":"body a"}},
			{"textQuerySuggestion":{"text":"ignored"}},
			{"textExcerptSuggestion":{"link":"https://docs.aws.amazon.com/b.html","title":"B","suggestionBody":"only body"}},
			{"textExcerptSuggestion":{"link":"https://docs.aws.amazon.com/c.html","title":"C"}}]}`)
	})
	res, err := c.Search(context.Background(), " widget limits ", 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []Result{{"A title", "https://docs.aws.amazon.com/a.html", "First summary"}, {"B", "https://docs.aws.amazon.com/b.html", "only body"}}
	if len(res) != 2 || res[0] != want[0] || res[1] != want[1] {
		t.Fatalf("results = %+v", res)
	}
	b, _ := json.Marshal(got)
	if string(b) != `{"acceptSuggestionBody":"RawText","contextAttributes":[{"key":"domain","value":"docs.aws.amazon.com"}],"locales":["en_us"],"textQuery":{"input":"widget limits"}}` {
		t.Fatalf("request body = %s", b)
	}

	c = newTestClient(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", http.StatusForbidden) })
	if _, err := c.Search(context.Background(), "x", 5); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v", err)
	}
}

func TestRead(t *testing.T) {
	page, _ := os.ReadFile("testdata/page.html")
	fetches := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/widgets/latest/guide/create.html":
			fetches++
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(page)
		case "/moved.html":
			http.Redirect(w, r, "https://example.com/evil.html", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()
	p, err := c.Read(ctx, "http://docs.aws.amazon.com/widgets/latest/guide/create.html#steps", 0, 0)
	if err != nil || p.Content != wantPage || p.NextOffset != 0 || p.URL != "https://docs.aws.amazon.com/widgets/latest/guide/create.html" {
		t.Fatalf("page = %+v, %v", p, err)
	}

	// Slices concatenate to the whole page; the page is fetched once.
	var all strings.Builder
	start := 0
	for i := 0; ; i++ {
		p, err := c.Read(ctx, "https://docs.aws.amazon.com/widgets/latest/guide/create.html", start, 120)
		if err != nil || p.Start != start || len([]rune(p.Content)) > 120 {
			t.Fatalf("slice at %d: %+v, %v", start, p, err)
		}
		all.WriteString(p.Content)
		if p.NextOffset == 0 {
			break
		}
		start = p.NextOffset
		if i > 100 {
			t.Fatal("no progress")
		}
	}
	if all.String() != wantPage || fetches != 1 {
		t.Fatalf("joined slices differ or %d fetches", fetches)
	}

	for _, u := range []string{"https://example.com/x.html", "https://docs.aws.amazon.com.evil.com/x", "ftp://docs.aws.amazon.com/x",
		"https://user@docs.aws.amazon.com/x", "https://docs.aws.amazon.com:8443/x", "not a url"} {
		if _, err := c.Read(ctx, u, 0, 0); err == nil || !strings.Contains(err.Error(), "only https://docs.aws.amazon.com/") {
			t.Errorf("%s: err = %v", u, err)
		}
	}
	if _, err := c.Read(ctx, "https://docs.aws.amazon.com/moved.html", 0, 0); err == nil || !strings.Contains(err.Error(), "redirected off") {
		t.Errorf("redirect: err = %v", err)
	}
	if _, err := c.Read(ctx, "https://docs.aws.amazon.com/missing.html", 0, 0); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("404: err = %v", err)
	}
}
