// Package awsdocs searches and reads the AWS documentation at
// docs.aws.amazon.com.
package awsdocs

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
)

// DocsHost is the only host Read fetches from.
const DocsHost = "docs.aws.amazon.com"

// DefaultSearchURL is the search service behind the search box of
// docs.aws.amazon.com.
const DefaultSearchURL = "https://proxy.search.docs.aws.amazon.com/search"

// Options configure a Client; zero fields take their defaults.
type Options struct {
	// SearchURL is the search endpoint; DefaultSearchURL by default.
	SearchURL string
	// HTTP performs requests. Its CheckRedirect is replaced so that Read
	// never follows a redirect off DocsHost.
	HTTP *http.Client
	// UserAgent is sent with every request.
	UserAgent string
	// MaxPageBytes bounds a fetched page (default 8 MiB).
	MaxPageBytes int64
}

// Client searches and reads the documentation. Create it with New.
type Client struct {
	Options
	session string
	mu      sync.Mutex
	cache   []cached // most recent last
}

type cached struct {
	url, title, md string
}

const cacheSize = 8

// New returns a Client.
func New(o Options) *Client {
	c := &Client{Options: o}
	if c.SearchURL == "" {
		c.SearchURL = DefaultSearchURL
	}
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 30 * time.Second}
	} else {
		h := *c.HTTP
		c.HTTP = &h
	}
	c.HTTP.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "https" || req.URL.Hostname() != DocsHost {
			return fmt.Errorf("redirected off %s to %s", DocsHost, req.URL)
		}
		return nil
	}
	if c.UserAgent == "" {
		c.UserAgent = "aws-mcp"
	}
	if c.MaxPageBytes <= 0 {
		c.MaxPageBytes = 8 << 20
	}
	var b [16]byte
	rand.Read(b[:])
	c.session = hex.EncodeToString(b[:])
	return c
}

// Result is one search hit.
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
}

// Search queries the documentation search service and returns up to limit
// results.
//
// The request is the one the docs site's own search box sends: a POST of
//
//	{"textQuery": {"input": "<query>"},
//	 "contextAttributes": [{"key": "domain", "value": "docs.aws.amazon.com"}],
//	 "acceptSuggestionBody": "RawText", "locales": ["en_us"]}
//
// answered with {"suggestions": [{"textExcerptSuggestion": {"link", "title",
// "summary", "suggestionBody", ...}}, ...]}.
func (c *Client) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("query is empty")
	}
	body, _ := json.Marshal(map[string]any{
		"textQuery":            map[string]string{"input": query},
		"contextAttributes":    []map[string]string{{"key": "domain", "value": DocsHost}},
		"acceptSuggestionBody": "RawText",
		"locales":              []string{"en_us"},
	})
	u := c.SearchURL + "?session=" + c.session
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search request failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("reading search response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search service answered %s: %s", resp.Status, truncate(string(data), 300))
	}
	var r struct {
		Suggestions []struct {
			TextExcerptSuggestion *struct {
				Link           string `json:"link"`
				Title          string `json:"title"`
				Summary        string `json:"summary"`
				SuggestionBody string `json:"suggestionBody"`
			} `json:"textExcerptSuggestion"`
		} `json:"suggestions"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("unexpected search response: %v: %s", err, truncate(string(data), 300))
	}
	var out []Result
	for _, s := range r.Suggestions {
		t := s.TextExcerptSuggestion
		if t == nil || t.Link == "" {
			continue
		}
		snip := t.Summary
		if strings.TrimSpace(snip) == "" {
			snip = t.SuggestionBody
		}
		out = append(out, Result{
			Title:   strings.TrimSpace(collapse(t.Title)),
			URL:     t.Link,
			Snippet: truncate(strings.TrimSpace(collapse(snip)), 400),
		})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// Page is a slice of a documentation page converted to Markdown.
type Page struct {
	URL        string
	Title      string
	Content    string
	Start      int // offset of Content in characters
	Total      int // length of the whole page in characters
	NextOffset int // where the next slice starts; 0 if this is the end
}

// CheckURL validates a documentation URL: https (http is upgraded) on
// DocsHost only. The fragment is dropped.
func CheckURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %v", err)
	}
	if u.Scheme == "http" {
		u.Scheme = "https"
	}
	if u.Scheme != "https" || u.Hostname() != DocsHost || u.Port() != "" || u.User != nil {
		return nil, fmt.Errorf("only https://%s/ pages can be read, not %q", DocsHost, raw)
	}
	u.Fragment = ""
	return u, nil
}

// Read fetches a documentation page and returns maxLen characters of its
// Markdown starting at start. Recently read pages are cached, so reading a
// long page slice by slice fetches it once.
func (c *Client) Read(ctx context.Context, rawURL string, start, maxLen int) (*Page, error) {
	u, err := CheckURL(rawURL)
	if err != nil {
		return nil, err
	}
	title, md, err := c.markdown(ctx, u)
	if err != nil {
		return nil, err
	}
	return slice(u.String(), title, md, start, maxLen), nil
}

func (c *Client) markdown(ctx context.Context, u *url.URL) (string, string, error) {
	key := u.String()
	c.mu.Lock()
	for i, e := range c.cache {
		if e.url == key {
			c.cache = append(append(c.cache[:i:i], c.cache[i+1:]...), e)
			c.mu.Unlock()
			return e.title, e.md, nil
		}
	}
	c.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, key, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Accept", "text/html,text/markdown;q=0.9,text/plain;q=0.8")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("fetching %s: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("fetching %s: %s", key, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.MaxPageBytes+1))
	if err != nil {
		return "", "", fmt.Errorf("fetching %s: %w", key, err)
	}
	if int64(len(data)) > c.MaxPageBytes {
		return "", "", fmt.Errorf("%s is larger than %d bytes", key, c.MaxPageBytes)
	}
	var title, md string
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch {
	case mt == "text/html" || mt == "application/xhtml+xml" || mt == "":
		doc, err := html.Parse(bytes.NewReader(data))
		if err != nil {
			return "", "", fmt.Errorf("parsing %s: %w", key, err)
		}
		title, md = ToMarkdown(doc, resp.Request.URL)
	case strings.HasPrefix(mt, "text/"):
		md = strings.TrimSpace(string(data))
	default:
		return "", "", fmt.Errorf("%s is %s, not a page", key, mt)
	}

	c.mu.Lock()
	c.cache = append(c.cache, cached{key, title, md})
	if len(c.cache) > cacheSize {
		c.cache = c.cache[len(c.cache)-cacheSize:]
	}
	c.mu.Unlock()
	return title, md, nil
}

// slice cuts md at character offsets. When the slice does not reach the end
// it is shortened to the last line break in its final fifth, so a slice
// rarely ends mid-sentence.
func slice(u, title, md string, start, maxLen int) *Page {
	r := []rune(md)
	p := &Page{URL: u, Title: title, Total: len(r)}
	if start < 0 {
		start = 0
	}
	if start > len(r) {
		start = len(r)
	}
	end := len(r)
	if maxLen > 0 && start+maxLen < len(r) {
		end = start + maxLen
		for i := end; i > start+maxLen*4/5; i-- {
			if r[i-1] == '\n' {
				end = i
				break
			}
		}
		p.NextOffset = end
	}
	p.Start = start
	p.Content = string(r[start:end])
	return p
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "..."
}
