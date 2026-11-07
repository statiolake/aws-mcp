package awsdocs

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// ToMarkdown converts an HTML page to compact Markdown: only the main
// content is kept (the docs' #main-col-body, else <main>, <article> or
// <body>), without navigation, headers, footers, feedback widgets, scripts,
// styles, forms or images. Relative links are resolved against base. It
// returns the page title and the Markdown.
func ToMarkdown(doc *html.Node, base *url.URL) (title, md string) {
	title = strings.TrimSpace(collapse(textContent(find(doc, func(n *html.Node) bool { return n.DataAtom == atom.Title }))))
	root := find(doc, func(n *html.Node) bool { return attr(n, "id") == "main-col-body" })
	for _, pick := range []func(*html.Node) bool{
		func(n *html.Node) bool { return n.DataAtom == atom.Main },
		func(n *html.Node) bool { return attr(n, "id") == "main-content" },
		func(n *html.Node) bool { return n.DataAtom == atom.Article },
		func(n *html.Node) bool { return n.DataAtom == atom.Body },
	} {
		if root != nil {
			break
		}
		root = find(doc, pick)
	}
	if root == nil {
		root = doc
	}
	c := &converter{base: base}
	md = strings.Join(c.blocks(root), "\n\n")
	md = strings.TrimSpace(blankRuns.ReplaceAllString(md, "\n\n"))
	if title != "" && !strings.HasPrefix(md, "# ") {
		md = "# " + title + "\n\n" + md
	}
	return title, md
}

var blankRuns = regexp.MustCompile(`\n{3,}`)

type converter struct {
	base *url.URL
}

// skipTags are never rendered.
var skipTags = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Noscript: true, atom.Template: true, atom.Svg: true,
	atom.Iframe: true, atom.Form: true, atom.Button: true, atom.Input: true, atom.Select: true,
	atom.Textarea: true, atom.Nav: true, atom.Header: true, atom.Footer: true, atom.Aside: true,
	atom.Img: true, atom.Picture: true, atom.Video: true, atom.Audio: true, atom.Canvas: true,
	atom.Link: true, atom.Meta: true, atom.Head: true, atom.Object: true,
}

// skipMarkers are substrings of class or id values of page chrome.
var skipMarkers = []string{"breadcrumb", "feedback", "prev-next", "page-utilities", "page-header", "page-footer",
	"copyright", "language-selector", "thanks", "cookie", "awsdocs-filter", "skip-link"}

func skip(n *html.Node) bool {
	if n.Type == html.CommentNode || n.Type == html.DoctypeNode {
		return true
	}
	if n.Type != html.ElementNode {
		return false
	}
	if skipTags[n.DataAtom] || hasAttr(n, "hidden") || attr(n, "aria-hidden") == "true" || attr(n, "role") == "navigation" {
		return true
	}
	cls := strings.ToLower(attr(n, "class") + " " + attr(n, "id"))
	for _, m := range skipMarkers {
		if strings.Contains(cls, m) {
			return true
		}
	}
	return false
}

// blockTags start a block; everything else is rendered inline.
var blockTags = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Section: true, atom.Article: true, atom.Main: true, atom.Body: true, atom.Html: true,
	atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true,
	atom.Pre: true, atom.Ul: true, atom.Ol: true, atom.Li: true, atom.Dl: true, atom.Dt: true, atom.Dd: true,
	atom.Table: true, atom.Blockquote: true, atom.Hr: true, atom.Figure: true, atom.Figcaption: true,
	atom.Details: true, atom.Summary: true, atom.Address: true, atom.Center: true,
}

func isBlock(n *html.Node) bool {
	return n.Type == html.ElementNode && blockTags[n.DataAtom] || n.Type == html.DocumentNode
}

// blocks renders the children of n as a list of Markdown blocks.
func (c *converter) blocks(n *html.Node) []string {
	var out []string
	var para strings.Builder
	flush := func() {
		if s := strings.TrimSpace(collapseSpaces(para.String())); s != "" {
			out = append(out, s)
		}
		para.Reset()
	}
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if skip(ch) {
			continue
		}
		if isBlock(ch) {
			flush()
			out = append(out, c.block(ch)...)
			continue
		}
		para.WriteString(c.inline(ch))
	}
	flush()
	return out
}

// block renders one block element.
func (c *converter) block(n *html.Node) []string {
	switch n.DataAtom {
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		t := strings.TrimSpace(collapseSpaces(c.inlineChildren(n)))
		if t == "" {
			return nil
		}
		return []string{strings.Repeat("#", int(n.Data[1]-'0')) + " " + strings.ReplaceAll(t, "\n", " ")}
	case atom.Pre:
		return []string{fence(textContent(n), codeLang(n))}
	case atom.Hr:
		return []string{"---"}
	case atom.Ul, atom.Ol:
		return c.list(n)
	case atom.Dl:
		return c.definitions(n)
	case atom.Table:
		return c.table(n)
	case atom.Blockquote:
		inner := strings.Join(c.blocks(n), "\n\n")
		if inner == "" {
			return nil
		}
		return []string{prefixLines(inner, "> ", "> ")}
	}
	return c.blocks(n)
}

func (c *converter) list(n *html.Node) []string {
	var items []string
	i := 1
	for li := n.FirstChild; li != nil; li = li.NextSibling {
		if li.Type != html.ElementNode || skip(li) {
			continue
		}
		var body []string
		if li.DataAtom == atom.Li {
			body = c.blocks(li)
		} else {
			body = c.block(li)
		}
		if len(body) == 0 {
			continue
		}
		marker := "- "
		if n.DataAtom == atom.Ol {
			marker = strconv.Itoa(i) + ". "
			i++
		}
		items = append(items, listItem(marker, body))
	}
	if len(items) == 0 {
		return nil
	}
	return []string{strings.Join(items, "\n")}
}

// definitions renders <dl> as a list: each <dt> is a bold item and the
// following <dd> blocks are indented under it (the API references describe
// parameters this way).
func (c *converter) definitions(n *html.Node) []string {
	var items []string
	var term string
	var body []string
	emit := func() {
		if term == "" && len(body) == 0 {
			return
		}
		parts := body
		if term != "" {
			parts = append([]string{"**" + term + "**"}, body...)
		}
		items = append(items, listItem("- ", parts))
		term, body = "", nil
	}
	var walk func(*html.Node)
	walk = func(p *html.Node) {
		for ch := p.FirstChild; ch != nil; ch = ch.NextSibling {
			if ch.Type != html.ElementNode || skip(ch) {
				continue
			}
			switch ch.DataAtom {
			case atom.Dt:
				if term != "" || len(body) > 0 {
					emit()
				}
				term = strings.TrimSpace(collapseSpaces(strings.ReplaceAll(c.inlineChildren(ch), "\n", " ")))
			case atom.Dd:
				body = append(body, c.blocks(ch)...)
			case atom.Div: // <div> wrappers around dt/dd groups are allowed
				walk(ch)
			}
		}
	}
	walk(n)
	emit()
	if len(items) == 0 {
		return nil
	}
	return []string{strings.Join(items, "\n")}
}

func (c *converter) table(n *html.Node) []string {
	var rows [][]string
	var walk func(*html.Node)
	walk = func(p *html.Node) {
		for ch := p.FirstChild; ch != nil; ch = ch.NextSibling {
			if ch.Type != html.ElementNode || skip(ch) {
				continue
			}
			switch ch.DataAtom {
			case atom.Tr:
				var row []string
				for td := ch.FirstChild; td != nil; td = td.NextSibling {
					if td.Type == html.ElementNode && (td.DataAtom == atom.Td || td.DataAtom == atom.Th) {
						cell := strings.Join(c.blocks(td), " ")
						cell = strings.ReplaceAll(cell, "\n", " ")
						row = append(row, strings.ReplaceAll(collapseSpaces(cell), "|", `\|`))
					}
				}
				if len(row) > 0 {
					rows = append(rows, row)
				}
			case atom.Thead, atom.Tbody, atom.Tfoot:
				walk(ch)
			}
		}
	}
	walk(n)
	if len(rows) == 0 {
		return nil
	}
	width := 0
	for _, r := range rows {
		width = max(width, len(r))
	}
	var b strings.Builder
	line := func(r []string) {
		for len(r) < width {
			r = append(r, "")
		}
		b.WriteString("| " + strings.Join(r, " | ") + " |\n")
	}
	line(rows[0])
	sep := make([]string, width)
	for i := range sep {
		sep[i] = "---"
	}
	line(sep)
	for _, r := range rows[1:] {
		line(r)
	}
	var out []string
	if capt := find(n, func(x *html.Node) bool { return x.DataAtom == atom.Caption }); capt != nil {
		if t := strings.TrimSpace(collapse(textContent(capt))); t != "" {
			out = append(out, "**"+t+"**")
		}
	}
	return append(out, strings.TrimRight(b.String(), "\n"))
}

// inline renders an inline node. Whitespace is collapsed later, except for
// the newlines produced by <br>.
func (c *converter) inline(n *html.Node) string {
	switch n.Type {
	case html.TextNode:
		return collapse(n.Data)
	case html.ElementNode:
	default:
		return ""
	}
	if skip(n) {
		return ""
	}
	if isBlock(n) {
		// A block inside an inline element (invalid but common): render it
		// in place.
		return " " + strings.Join(c.block(n), " ") + " "
	}
	switch n.DataAtom {
	case atom.Br:
		return "\n"
	case atom.Code, atom.Kbd, atom.Samp, atom.Tt:
		t := strings.TrimSpace(collapse(textContent(n)))
		if t == "" {
			return ""
		}
		tick := "`"
		if strings.Contains(t, "`") {
			tick = "``"
		}
		return tick + t + tick
	case atom.Strong, atom.B:
		return wrap(c.inlineChildren(n), "**")
	case atom.Em, atom.I, atom.Var, atom.Cite:
		return wrap(c.inlineChildren(n), "*")
	case atom.A:
		text := c.inlineChildren(n)
		href := strings.TrimSpace(attr(n, "href"))
		if strings.TrimSpace(text) == "" {
			return ""
		}
		if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(strings.ToLower(href), "javascript:") {
			return text
		}
		if u, err := c.base.Parse(href); err == nil {
			href = u.String()
		}
		lead, core, trail := splitSpace(text)
		return lead + "[" + strings.ReplaceAll(core, "\n", " ") + "](" + href + ")" + trail
	}
	return c.inlineChildren(n)
}

func (c *converter) inlineChildren(n *html.Node) string {
	var b strings.Builder
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		b.WriteString(c.inline(ch))
	}
	return b.String()
}

// wrap puts mark around s, keeping surrounding spaces outside.
func wrap(s, mark string) string {
	lead, core, trail := splitSpace(s)
	if core == "" {
		return s
	}
	return lead + mark + core + mark + trail
}

func splitSpace(s string) (lead, core, trail string) {
	core = strings.TrimSpace(s)
	if core == "" {
		return s, "", ""
	}
	i := strings.Index(s, core)
	return s[:i], core, s[i+len(core):]
}

// listItem prefixes the first block with marker and indents the rest so
// they stay inside the item.
func listItem(marker string, body []string) string {
	pad := strings.Repeat(" ", len(marker))
	var parts []string
	for _, b := range body {
		if b == "" {
			continue
		}
		if len(parts) == 0 {
			parts = append(parts, prefixLines(b, marker, pad))
		} else {
			parts = append(parts, prefixLines(b, pad, pad))
		}
	}
	if len(parts) == 0 {
		return strings.TrimRight(marker, " ")
	}
	return strings.Join(parts, "\n")
}

func prefixLines(s, first, rest string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		p := rest
		if i == 0 {
			p = first
		}
		if l == "" {
			lines[i] = strings.TrimRight(p, " ")
		} else {
			lines[i] = p + l
		}
	}
	return strings.Join(lines, "\n")
}

func fence(code, lang string) string {
	code = strings.Trim(code, "\n")
	f := "```"
	for strings.Contains(code, f) {
		f += "`"
	}
	return f + lang + "\n" + code + "\n" + f
}

var langRE = regexp.MustCompile(`^[a-z][a-z0-9+#-]*$`)

// codeLang guesses the language from the class of <pre> or its <code>
// (the docs use e.g. class="programlisting" with <code class="json">).
func codeLang(pre *html.Node) string {
	cands := []string{attr(pre, "class")}
	if code := find(pre, func(n *html.Node) bool { return n.DataAtom == atom.Code }); code != nil {
		cands = append([]string{attr(code, "class")}, cands...)
	}
	for _, cls := range cands {
		for _, w := range strings.Fields(cls) {
			w = strings.TrimPrefix(strings.TrimPrefix(w, "language-"), "lang-")
			switch w {
			case "programlisting", "screen", "nohighlight", "hljs", "code", "userinput", "computeroutput":
				continue
			}
			if langRE.MatchString(w) {
				return w
			}
		}
	}
	return ""
}

var spaceRE = regexp.MustCompile(`[ \t\r\f\v\x{a0}]+`)

// collapse turns every whitespace run into one space (for text nodes).
func collapse(s string) string {
	return spaceRE.ReplaceAllString(strings.ReplaceAll(s, "\n", " "), " ")
}

// collapseSpaces merges spaces but keeps explicit newlines, trimming the
// spaces around them.
func collapseSpaces(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(spaceRE.ReplaceAllString(l, " "))
	}
	return strings.Join(lines, "\n")
}

func textContent(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		if n.Type == html.ElementNode && n.DataAtom == atom.Br {
			b.WriteString("\n")
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			if ch.Type == html.ElementNode && (ch.DataAtom == atom.Script || ch.DataAtom == atom.Style) {
				continue
			}
			walk(ch)
		}
	}
	walk(n)
	return b.String()
}

// find returns the first node in document order that matches.
func find(n *html.Node, match func(*html.Node) bool) *html.Node {
	if n == nil {
		return nil
	}
	if n.Type == html.ElementNode && match(n) {
		return n
	}
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if f := find(ch, match); f != nil {
			return f
		}
	}
	return nil
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func hasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Key == key {
			return true
		}
	}
	return false
}
