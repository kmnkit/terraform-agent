// Package tfdocs decides which provider version applies to a change and
// fetches the matching official documentation through the Terraform MCP server.
package tfdocs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/terraform-agent/internal/hclscan"
	"github.com/terraform-agent/internal/model"
	"github.com/terraform-agent/internal/tfmcp"
)

// Logical tool names: keys of config.McpToolAllowlist.
const (
	ToolSearchProviderDocs    = "searchProviderDocs"
	ToolProviderDetails       = "getProviderDetails"
	ToolLatestProviderVersion = "getLatestProviderVersion"
)

const lockFileName = ".terraform.lock.hcl"

// Basis says where a resolved provider version came from.
type Basis string

const (
	BasisLock       Basis = "lock"
	BasisConstraint Basis = "constraint"
	BasisNone       Basis = "none"
)

// ResolveVersion picks the provider version whose docs apply to changedFile.
//
// It walks from the file's directory up to the repo root and uses the first
// .terraform.lock.hcl that pins the provider. A lock file that exists but does
// not list this provider is skipped (the walk continues upward), because a
// partially populated lock says nothing about this provider. Without a lock,
// the registry's latest version is used only if it satisfies constraint;
// otherwise the result is BasisNone with an empty Version, meaning "review
// without documentation". Non-public providers and constraints that cannot be
// parsed also yield BasisNone without error.
func ResolveVersion(ctx context.Context, c tfmcp.Caller, changedFile string, head hclscan.FileReader,
	namespace, name, constraint string) (model.ProviderRef, Basis, error) {
	ref := model.ProviderRef{Namespace: namespace, Name: name}
	if !hclscan.IsPublicRegistry(namespace) {
		return ref, BasisNone, nil
	}
	locked, err := lockedVersion(ctx, head, changedFile, namespace, name)
	if err != nil {
		return ref, BasisNone, err
	}
	if locked != "" {
		ref.Version = locked
		return ref, BasisLock, nil
	}
	return resolveFromConstraint(ctx, c, ref, constraint)
}

func resolveFromConstraint(ctx context.Context, c tfmcp.Caller, ref model.ProviderRef,
	constraint string) (model.ProviderRef, Basis, error) {
	if constraint == "" {
		return ref, BasisNone, nil
	}
	cons, err := version.NewConstraint(constraint)
	if err != nil {
		return ref, BasisNone, nil
	}
	text, err := c.Call(ctx, ToolLatestProviderVersion, map[string]any{"namespace": ref.Namespace, "name": ref.Name})
	if err != nil {
		return ref, BasisNone, fmt.Errorf("tfdocs: latest version of %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	latest, err := version.NewVersion(strings.TrimSpace(text))
	if err != nil {
		return ref, BasisNone, fmt.Errorf("tfdocs: latest version of %s/%s is not a version", ref.Namespace, ref.Name)
	}
	if !cons.Check(latest) {
		return ref, BasisNone, nil
	}
	ref.Version = latest.Original()
	return ref, BasisConstraint, nil
}

// lockedVersion returns the pinned version from the nearest lock file that
// lists the provider, or "" when none does.
func lockedVersion(ctx context.Context, head hclscan.FileReader, changedFile, namespace, name string) (string, error) {
	for dir := path.Dir(changedFile); ; dir = path.Dir(dir) {
		lockPath := path.Join(dir, lockFileName)
		src, err := head.ReadFile(ctx, lockPath)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return "", fmt.Errorf("tfdocs: read %s: %w", lockPath, err)
		default:
			v, err := versionInLock(lockPath, src, namespace, name)
			if err != nil || v != "" {
				return v, err
			}
		}
		if dir == "." || dir == "/" || dir == "" {
			return "", nil
		}
	}
}

func versionInLock(lockPath string, src []byte, namespace, name string) (string, error) {
	file, diags := hclsyntax.ParseConfig(src, lockPath, hcl.InitialPos)
	if diags.HasErrors() {
		return "", fmt.Errorf("tfdocs: parse %s: %s", lockPath, diags.Error())
	}
	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return "", nil
	}
	address := "registry.terraform.io/" + namespace + "/" + name
	for _, b := range body.Blocks {
		if b.Type != "provider" || len(b.Labels) != 1 || b.Labels[0] != address {
			continue
		}
		attr, ok := b.Body.Attributes["version"]
		if !ok {
			continue
		}
		val, d := attr.Expr.Value(nil)
		if d.HasErrors() || !val.IsKnown() || val.IsNull() || val.Type().FriendlyName() != "string" {
			return "", fmt.Errorf("tfdocs: %s: version of %s is not a string literal", lockPath, address)
		}
		return val.AsString(), nil
	}
	return "", nil
}
