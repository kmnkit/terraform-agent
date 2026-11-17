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
		v, diags := attr.Expr.Value(nil)
		if diags.HasErrors() || v.IsNull() || !v.IsKnown() {
			continue
		}
		switch {
		case v.Type() == cty.String:
			decls.Constraints[local] = v.AsString()
		case v.Type().IsObjectType():
			if s := objectString(v, "source"); s != "" {
				decls.Sources[local] = strings.TrimPrefix(s, publicRegistryHost)
			}
			if s := objectString(v, "version"); s != "" {
				decls.Constraints[local] = s
			}
		}
	}
}

func objectString(obj cty.Value, key string) string {
	if !obj.Type().HasAttribute(key) {
		return ""
	}
	v := obj.GetAttr(key)
	if v.IsNull() || !v.IsKnown() || v.Type() != cty.String {
		return ""
	}
	return v.AsString()
}

// ProviderFor maps a resource to its provider's registry namespace and name.
// The local name is providerMetaArg without any alias suffix, else the
// resource type prefix up to the first "_". A declared source wins; otherwise
// the namespace defaults to "hashicorp". For a non-public host source the
// namespace contains the host ("host/namespace"), so a "." in it marks a
// provider the caller should skip.
func ProviderFor(rc ResourceChange, providerMetaArg string, decls ProviderDecls) (namespace, name string) {
	local, _, _ := strings.Cut(providerMetaArg, ".")
	if local == "" {
		local, _, _ = strings.Cut(rc.Type, "_")
	}
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
