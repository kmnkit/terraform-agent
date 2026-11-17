package tfdocs

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/terraform-agent/internal/model"
	"github.com/terraform-agent/internal/tfmcp"
)

// ErrDocNotFound means the search returned no entry whose title exactly
// matches the resource.
var ErrDocNotFound = errors.New("tfdocs: no exact documentation match")

// Excerpt is the part of a resource's documentation relevant to a change.
type Excerpt struct {
	Text      string
	Truncated bool
	DocURL    string
}

type docKey struct {
	ref          model.ProviderRef
	resourceType string
}

// Lookuper fetches resource docs and caches the full document per
// (provider version, resource type), so repeated lookups for the same
// resource with different attributes cost no further MCP calls. A failed
// fetch is never cached.
type Lookuper struct {
	mu    sync.Mutex
	cache map[docKey]string
}

func NewLookuper() *Lookuper {
	return &Lookuper{cache: map[docKey]string{}}
}

// Lookup returns the documentation excerpt for attrs of resourceType at the
// pinned provider version, limited to maxBytes of text.
func (l *Lookuper) Lookup(ctx context.Context, c tfmcp.Caller, p model.ProviderRef, resourceType string,
	attrs []string, maxBytes int) (Excerpt, error) {
	if p.Version == "" {
		return Excerpt{}, fmt.Errorf("tfdocs: lookup %s: provider version is empty", resourceType)
	}
	slug := resourceSlug(p.Name, resourceType)
	doc, err := l.document(ctx, c, p, resourceType, slug)
	if err != nil {
		return Excerpt{}, err
	}
	text, truncated := truncateUTF8(excerptText(doc, attrs), maxBytes)
	return Excerpt{Text: text, Truncated: truncated, DocURL: docURL(p, slug)}, nil
}

func (l *Lookuper) document(ctx context.Context, c tfmcp.Caller, p model.ProviderRef, resourceType, slug string) (string, error) {
	key := docKey{p, resourceType}
	l.mu.Lock()
	doc, ok := l.cache[key]
	l.mu.Unlock()
	if ok {
		return doc, nil
	}
	doc, err := fetchDoc(ctx, c, p, resourceType, slug)
	if err != nil {
		return "", err
	}
	l.mu.Lock()
	l.cache[key] = doc
	l.mu.Unlock()
	return doc, nil
}

func fetchDoc(ctx context.Context, c tfmcp.Caller, p model.ProviderRef, resourceType, slug string) (string, error) {
	found, err := c.Call(ctx, ToolSearchProviderDocs, map[string]any{
		"provider_name":          p.Name,
		"provider_namespace":     p.Namespace,
		"provider_version":       p.Version,
		"service_slug":           slug,
		"provider_document_type": "resources",
	})
	if err != nil {
		return "", fmt.Errorf("tfdocs: search docs for %s: %w", resourceType, err)
	}
	id := docIDForTitle(found, slug)
	if id == "" {
		return "", fmt.Errorf("%w for %s %s", ErrDocNotFound, resourceType, p.Version)
	}
	doc, err := c.Call(ctx, ToolProviderDetails, map[string]any{"provider_doc_id": id})
	if err != nil {
		return "", fmt.Errorf("tfdocs: fetch docs for %s: %w", resourceType, err)
	}
	return doc, nil
}

// resourceSlug strips the provider prefix: google_compute_instance with
// provider google gives compute_instance. When the type does not start with
// "<provider>_" (for example provider google-beta) the first segment is dropped.
func resourceSlug(providerName, resourceType string) string {
	if s, ok := strings.CutPrefix(resourceType, providerName+"_"); ok {
		return s
	}
	_, s, _ := strings.Cut(resourceType, "_")
	return s
}

func docURL(p model.ProviderRef, slug string) string {
	return "https://registry.terraform.io/providers/" + p.Namespace + "/" + p.Name + "/" + p.Version + "/docs/resources/" + slug
}

// docIDForTitle parses the search listing ("- providerDocID: N" followed by
// "- Title: T") and returns the ID of the entry whose title equals slug.
func docIDForTitle(listing, slug string) string {
	id := ""
	for _, line := range strings.Split(listing, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "- providerDocID:"); ok {
			id = strings.TrimSpace(v)
		} else if v, ok := strings.CutPrefix(line, "- Title:"); ok && id != "" {
			if strings.TrimSpace(v) == slug {
				return id
			}
			id = ""
		}
	}
	return ""
}

var bulletName = regexp.MustCompile("^\\* `([^`]+)`")

// excerptText keeps the argument/attribute bullet entries named by attrs
// (every segment of dotted paths matches) and every line that
// mentions deprecation, in document order. An entry with any deprecated line is kept whole.
func excerptText(doc string, attrs []string) string {
	wanted := wantedNames(attrs)
	lines := strings.Split(doc, "\n")
	entryOf := make([]int, len(lines))
	var keepEntry []bool
	inRef, cur := false, -1
	for i, line := range lines {
		if h, ok := strings.CutPrefix(line, "## "); ok {
			inRef = h == "Argument Reference" || h == "Attributes Reference"
			cur = -1
		}
		if strings.HasPrefix(line, "<a name") {
			cur = -1
		}
		if m := bulletName.FindStringSubmatch(line); m != nil {
			cur = -1
			if inRef {
				keepEntry = append(keepEntry, wanted[m[1]])
				cur = len(keepEntry) - 1
			}
		}
		entryOf[i] = cur
		if cur >= 0 && isDeprecated(line) {
			keepEntry[cur] = true
		}
	}
	keep := make([]bool, len(lines))
	for i, line := range lines {
		keep[i] = (entryOf[i] >= 0 && keepEntry[entryOf[i]]) || isDeprecated(line)
	}
	return joinKept(lines, keep)
}

func isDeprecated(line string) bool {
	return strings.Contains(strings.ToLower(line), "deprecated")
}

func wantedNames(attrs []string) map[string]bool {
	out := map[string]bool{}
	for _, a := range attrs {
		for _, seg := range strings.Split(a, ".") {
			out[seg] = true
		}
	}
	return out
}

// joinKept joins the kept lines, separating non-adjacent groups by a blank
// line and dropping blank lines at group edges.
func joinKept(lines []string, keep []bool) string {
	var b strings.Builder
	prev := -2
	for i, line := range lines {
		if !keep[i] || strings.TrimSpace(line) == "" {
			continue
		}
		if b.Len() > 0 {
			sep := "\n"
			if !adjacent(lines, prev, i) {
				sep = "\n\n"
			}
			b.WriteString(sep)
		}
		b.WriteString(line)
		prev = i
	}
	return b.String()
}

// adjacent reports whether only blank lines separate kept lines prev and next
// that both belong to the same kept run.
func adjacent(lines []string, prev, next int) bool {
	for i := prev + 1; i < next; i++ {
		if strings.TrimSpace(lines[i]) != "" {
			return false
		}
	}
	return true
}

// truncateUTF8 cuts s to at most maxBytes without splitting a rune.
func truncateUTF8(s string, maxBytes int) (string, bool) {
	if maxBytes < 0 {
		maxBytes = 0
	}
	if len(s) <= maxBytes {
		return s, false
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}
