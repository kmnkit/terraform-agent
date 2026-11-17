// Package hclscan extracts which Terraform resources, attributes and module
// calls an MR changed. Output carries names only, never attribute values.
package hclscan

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/terraform-agent/internal/gitlab"
	"github.com/zclconf/go-cty/cty"
)

// FileReader reads a file at the MR head ref.
type FileReader interface {
	ReadFile(ctx context.Context, path string) ([]byte, error)
}

type Changes struct {
	Resources []ResourceChange
	Modules   []ModuleCall
}

// ResourceChange describes one changed resource. ProviderMeta is the raw
// `provider = x.y` traversal text (e.g. "google-beta", "google.west") or "".
type ResourceChange struct {
	File, Type, Name string
	ChangedAttrs     []string
	Removed          bool
	ProviderMeta     string
}

// ModuleCall holds only string-literal source/version; non-literals stay empty.
type ModuleCall struct {
	File, Name, Source, Version string
}

type resourceKey struct{ typ, name string }

var (
	hunkHeader   = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)
	resourceDecl = regexp.MustCompile(`^\s*resource\s+"([^"]+)"\s+"([^"]+)"\s*\{`)
)

// ChangedResources scans only .tf files; everything else is ignored without a
// head read. Deleted files are judged from their removed lines alone.
func ChangedResources(ctx context.Context, diffs []gitlab.FileDiff, head FileReader) (Changes, error) {
	var out Changes
	for _, d := range diffs {
		path := d.NewPath
		if d.DeletedFile {
			path = d.OldPath
		}
		if !strings.HasSuffix(path, ".tf") {
			continue
		}
		added, removed := parseHunks(d.Diff)
		if d.DeletedFile {
			out.Resources = append(out.Resources, removedChanges(path, removed, nil)...)
			continue
		}
		src, err := head.ReadFile(ctx, path)
		if err != nil {
			return Changes{}, fmt.Errorf("hclscan: read %s: %w", path, err)
		}
		fc, err := scanFile(path, src, added, removed)
		if err != nil {
			return Changes{}, err
		}
		out.Resources = append(out.Resources, fc.Resources...)
		out.Modules = append(out.Modules, fc.Modules...)
	}
	return out, nil
}

// parseHunks returns the added line numbers (new file) and the resource
// headers found on removed lines.
func parseHunks(diff string) (map[int]bool, []resourceKey) {
	added := map[int]bool{}
	var removed []resourceKey
	inHunk, newLine := false, 0
	for _, line := range strings.Split(diff, "\n") {
		if m := hunkHeader.FindStringSubmatch(line); m != nil {
			newLine, _ = strconv.Atoi(m[1])
			inHunk = true
			continue
		}
		if !inHunk || strings.HasPrefix(line, `\`) {
			continue
		}
		switch {
		case strings.HasPrefix(line, "+"):
			added[newLine] = true
			newLine++
		case strings.HasPrefix(line, "-"):
			if m := resourceDecl.FindStringSubmatch(line[1:]); m != nil {
				removed = append(removed, resourceKey{m[1], m[2]})
			}
		default:
			newLine++
		}
	}
	return added, removed
}

func scanFile(path string, src []byte, added map[int]bool, removed []resourceKey) (Changes, error) {
	body, err := parseHCL(path, src)
	if err != nil {
		return Changes{}, err
	}
	var out Changes
	existing := map[resourceKey]bool{}
	for _, b := range body.Blocks {
		switch {
		case b.Type == "resource" && len(b.Labels) == 2:
			existing[resourceKey{b.Labels[0], b.Labels[1]}] = true
			if rc, ok := resourceChange(path, b, added); ok {
				out.Resources = append(out.Resources, rc)
			}
		case b.Type == "module" && len(b.Labels) == 1 && touches(b.Range(), added):
			out.Modules = append(out.Modules, moduleCall(path, b))
		}
	}
	out.Resources = append(out.Resources, removedChanges(path, removed, existing)...)
	return out, nil
}

// removedChanges reports keys absent from existing, each once.
func removedChanges(path string, removed []resourceKey, existing map[resourceKey]bool) []ResourceChange {
	var out []ResourceChange
	seen := map[resourceKey]bool{}
	for _, k := range removed {
		if existing[k] || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, ResourceChange{File: path, Type: k.typ, Name: k.name, Removed: true})
	}
	return out
}

func resourceChange(path string, b *hclsyntax.Block, added map[int]bool) (ResourceChange, bool) {
	headerAdded := added[b.TypeRange.Start.Line]
	attrs := collectAttrs(b.Body, "", added, headerAdded)
	if !headerAdded && len(attrs) == 0 {
		return ResourceChange{}, false
	}
	slices.Sort(attrs)
	return ResourceChange{
		File: path, Type: b.Labels[0], Name: b.Labels[1],
		ChangedAttrs: slices.Compact(attrs),
		ProviderMeta: providerMeta(b.Body),
	}, true
}

// collectAttrs gathers dotted attribute paths whose source lines were added;
// all=true (new block header) takes every attribute.
func collectAttrs(body *hclsyntax.Body, prefix string, added map[int]bool, all bool) []string {
	var out []string
	for name, a := range body.Attributes {
		if all || touches(a.SrcRange, added) {
			out = append(out, prefix+name)
		}
	}
	for _, nb := range body.Blocks {
		nestedAll := all || added[nb.TypeRange.Start.Line]
		out = append(out, collectAttrs(nb.Body, prefix+nb.Type+".", added, nestedAll)...)
	}
	return out
}

func touches(r hcl.Range, added map[int]bool) bool {
	for l := r.Start.Line; l <= r.End.Line; l++ {
		if added[l] {
			return true
		}
	}
	return false
}

func providerMeta(body *hclsyntax.Body) string {
	attr, ok := body.Attributes["provider"]
	if !ok {
		return ""
	}
	trav, diags := hcl.AbsTraversalForExpr(attr.Expr)
	if diags.HasErrors() {
		return ""
	}
	parts := []string{trav.RootName()}
	for _, step := range trav[1:] {
		if s, ok := step.(hcl.TraverseAttr); ok {
			parts = append(parts, s.Name)
		}
	}
	return strings.Join(parts, ".")
}

func moduleCall(path string, b *hclsyntax.Block) ModuleCall {
	return ModuleCall{
		File: path, Name: b.Labels[0],
		Source:  stripUserinfo(stringLiteral(b.Body, "source")),
		Version: stringLiteral(b.Body, "version"),
	}
}

// stringLiteral returns the attribute's value only when it is a plain string
// literal (no references or function calls).
func stringLiteral(body *hclsyntax.Body, name string) string {
	attr, ok := body.Attributes[name]
	if !ok {
		return ""
	}
	return literalString(attr.Expr)
}

func literalString(expr hclsyntax.Expression) string {
	v, diags := expr.Value(nil)
	if diags.HasErrors() || v.IsNull() || !v.IsKnown() || v.Type() != cty.String {
		return ""
	}
	return v.AsString()
}

var (
	urlUserinfo = regexp.MustCompile(`://[^/@\s]*@`)
	scpUserinfo = regexp.MustCompile(`^(git::)?[^/@:\s]+@`)
)

// stripUserinfo removes credentials/usernames from a module source because it
// is forwarded to the LLM.
func stripUserinfo(src string) string {
	src = urlUserinfo.ReplaceAllString(src, "://")
	return scpUserinfo.ReplaceAllString(src, "$1")
}

// parseHCL parses src; errors name the file and diagnostic lines but never
// include file content.
func parseHCL(path string, src []byte) (*hclsyntax.Body, error) {
	f, diags := hclsyntax.ParseConfig(src, path, hcl.InitialPos)
	if diags.HasErrors() {
		var msgs []string
		for _, d := range diags {
			if d.Severity != hcl.DiagError {
				continue
			}
			line := 0
			if d.Subject != nil {
				line = d.Subject.Start.Line
			}
			msgs = append(msgs, fmt.Sprintf("line %d: %s", line, d.Summary))
		}
		return nil, fmt.Errorf("hclscan: parse %s: %s", path, strings.Join(msgs, "; "))
	}
	body, ok := f.Body.(*hclsyntax.Body)
	if !ok {
		return nil, fmt.Errorf("hclscan: parse %s: unexpected body type", path)
	}
	return body, nil
}
