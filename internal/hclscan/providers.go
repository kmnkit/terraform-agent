package hclscan

import (
	"maps"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

const publicRegistryHost = "registry.terraform.io/"

// ProviderDecls is keyed by local provider name. Sources are normalized to
// "namespace/name" with the public registry host stripped; a source on any
// other host is kept as-is ("host/namespace/name") so callers can skip it.
type ProviderDecls struct {
	Sources     map[string]string
	Constraints map[string]string
}

// ParseProviderDecls reads terraform { required_providers { ... } } blocks
// from the given .tf files (other names are skipped).
func ParseProviderDecls(files map[string][]byte) (ProviderDecls, error) {
	decls := ProviderDecls{Sources: map[string]string{}, Constraints: map[string]string{}}
	for _, path := range slices.Sorted(maps.Keys(files)) {
		if !strings.HasSuffix(path, ".tf") {
			continue
		}
		body, err := parseHCL(path, files[path])
		if err != nil {
			return ProviderDecls{}, err
		}
		for _, tf := range body.Blocks {
			if tf.Type != "terraform" {
				continue
			}
			for _, rp := range tf.Body.Blocks {
				if rp.Type == "required_providers" {
					addDecls(&decls, rp.Body)
				}
			}
		}
	}
	return decls, nil
}

func addDecls(decls *ProviderDecls, body *hclsyntax.Body) {
	for local, attr := range body.Attributes {
		if obj, ok := attr.Expr.(*hclsyntax.ObjectConsExpr); ok {
			addObjectDecl(decls, local, obj)
		} else if s := literalString(attr.Expr); s != "" {
			decls.Constraints[local] = s
		}
	}
}

// addObjectDecl evaluates only the source and version keys, so other keys
// (e.g. configuration_aliases traversals) cannot invalidate the declaration.
func addObjectDecl(decls *ProviderDecls, local string, obj *hclsyntax.ObjectConsExpr) {
	for _, item := range obj.Items {
		key, diags := item.KeyExpr.Value(nil)
		if diags.HasErrors() || key.IsNull() || key.Type() != cty.String {
			continue
		}
		switch key.AsString() {
		case "source":
			if s := literalString(item.ValueExpr); s != "" {
				decls.Sources[local] = strings.TrimPrefix(s, publicRegistryHost)
			}
		case "version":
			if s := literalString(item.ValueExpr); s != "" {
				decls.Constraints[local] = s
			}
		}
	}
}

// IsPublicRegistry reports whether a namespace returned by ProviderFor belongs
// to the public registry. Registry namespaces cannot contain "/", so a "/"
// means the source carried a non-public host.
func IsPublicRegistry(namespace string) bool {
	return !strings.Contains(namespace, "/")
}

// LocalName returns the resource's local provider name, the key of
// ProviderDecls: providerMetaArg without any alias suffix, else the resource
// type prefix up to the first "_".
func LocalName(rc ResourceChange, providerMetaArg string) string {
	local, _, _ := strings.Cut(providerMetaArg, ".")
	if local == "" {
		local, _, _ = strings.Cut(rc.Type, "_")
	}
	return local
}

// ProviderFor maps a resource to its provider's registry namespace and name.
// The local name is providerMetaArg without any alias suffix, else the
// resource type prefix up to the first "_". A declared source wins; otherwise
// the namespace defaults to "hashicorp". For a non-public host source the
// namespace contains the host ("host/namespace"); callers must use
// IsPublicRegistry to decide whether to skip it.
func ProviderFor(rc ResourceChange, providerMetaArg string, decls ProviderDecls) (namespace, name string) {
	local := LocalName(rc, providerMetaArg)
	src, ok := decls.Sources[local]
	if !ok {
		return "hashicorp", local
	}
	i := strings.LastIndex(src, "/")
	if i <= 0 {
		return "hashicorp", local
	}
	return src[:i], src[i+1:]
}
