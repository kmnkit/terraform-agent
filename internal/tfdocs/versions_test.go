package tfdocs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"testing"

	"github.com/terraform-agent/internal/model"
)

type call struct {
	Name string
	Args map[string]any
}

// fakeCaller answers by logical tool name and records every call.
type fakeCaller struct {
	replies map[string]string
	err     error
	calls   []call
}

func (f *fakeCaller) Call(_ context.Context, name string, args map[string]any) (string, error) {
	f.calls = append(f.calls, call{name, args})
	if f.err != nil {
		return "", f.err
	}
	return f.replies[name], nil
}

type fakeFiles map[string]string

func (f fakeFiles) ReadFile(_ context.Context, p string) ([]byte, error) {
	s, ok := f[p]
	if !ok {
		return nil, fmt.Errorf("read %s: %w", p, fs.ErrNotExist)
	}
	return []byte(s), nil
}

type errFiles struct{ err error }

func (e errFiles) ReadFile(context.Context, string) ([]byte, error) { return nil, e.err }

func lockFor(version string) string {
	return `provider "registry.terraform.io/hashicorp/google" {
  version     = "` + version + `"
  constraints = "~> 5.0"
  hashes = ["h1:abc"]
}
`
}

func TestResolveVersion_LockWins(t *testing.T) {
	c := &fakeCaller{}
	files := fakeFiles{"envs/prod/.terraform.lock.hcl": lockFor("5.3.0")}
	ref, basis, err := ResolveVersion(context.Background(), c, "envs/prod/main.tf", files, "hashicorp", "google", "~> 5.0")
	if err != nil {
		t.Fatal(err)
	}
	want := model.ProviderRef{Namespace: "hashicorp", Name: "google", Version: "5.3.0"}
	if ref != want || basis != BasisLock {
		t.Errorf("got %+v %q", ref, basis)
	}
	if len(c.calls) != 0 {
		t.Errorf("MCP called despite lock: %v", c.calls)
	}
}

func TestResolveVersion_NearestLockAcrossRootModules(t *testing.T) {
	files := fakeFiles{
		".terraform.lock.hcl":               lockFor("4.0.0"),
		"envs/dev/.terraform.lock.hcl":      lockFor("5.1.0"),
		"envs/prod/.terraform.lock.hcl":     lockFor("5.9.0"),
		"envs/prod/mod/.terraform.lock.hcl": "",
	}
	ctx := context.Background()
	ref, _, err := ResolveVersion(ctx, &fakeCaller{}, "envs/dev/network/main.tf", files, "hashicorp", "google", "")
	if err != nil || ref.Version != "5.1.0" {
		t.Errorf("dev: %+v %v", ref, err)
	}
	ref, _, err = ResolveVersion(ctx, &fakeCaller{}, "other/main.tf", files, "hashicorp", "google", "")
	if err != nil || ref.Version != "4.0.0" {
		t.Errorf("root fallback: %+v %v", ref, err)
	}
}

func TestResolveVersion_NearestLockWithoutProviderKeepsWalking(t *testing.T) {
	files := fakeFiles{
		"a/.terraform.lock.hcl": `provider "registry.terraform.io/hashicorp/aws" { version = "5.0.0" }`,
		".terraform.lock.hcl":   lockFor("4.7.0"),
	}
	ref, basis, err := ResolveVersion(context.Background(), &fakeCaller{}, "a/main.tf", files, "hashicorp", "google", "")
	if err != nil || ref.Version != "4.7.0" || basis != BasisLock {
		t.Errorf("got %+v %q %v", ref, basis, err)
	}
}

func TestResolveVersion_NoLockLatestSatisfiesConstraint(t *testing.T) {
	c := &fakeCaller{replies: map[string]string{ToolLatestProviderVersion: "5.44.2\n"}}
	ref, basis, err := ResolveVersion(context.Background(), c, "main.tf", fakeFiles{}, "hashicorp", "google", "~> 5.0")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Version != "5.44.2" || basis != BasisConstraint {
		t.Errorf("got %+v %q", ref, basis)
	}
	if len(c.calls) != 1 || c.calls[0].Name != ToolLatestProviderVersion {
		t.Fatalf("calls = %v", c.calls)
	}
	if c.calls[0].Args["namespace"] != "hashicorp" || c.calls[0].Args["name"] != "google" {
		t.Errorf("args = %v", c.calls[0].Args)
	}
}

func TestResolveVersion_NoneCases(t *testing.T) {
	tests := []struct {
		name       string
		constraint string
		latest     string
	}{
		{"latest violates constraint", "~> 5.0", "6.1.0"},
		{"no constraint", "", "6.1.0"},
		{"unparseable constraint", "~~ banana", "6.1.0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &fakeCaller{replies: map[string]string{ToolLatestProviderVersion: tc.latest}}
			ref, basis, err := ResolveVersion(context.Background(), c, "main.tf", fakeFiles{}, "hashicorp", "google", tc.constraint)
			if err != nil {
				t.Fatal(err)
			}
			if basis != BasisNone || ref.Version != "" {
				t.Errorf("got %+v %q", ref, basis)
			}
			if ref.Namespace != "hashicorp" || ref.Name != "google" {
				t.Errorf("ref identity lost: %+v", ref)
			}
		})
	}
}

func TestResolveVersion_NonPublicSkipsEverything(t *testing.T) {
	c := &fakeCaller{}
	files := fakeFiles{".terraform.lock.hcl": lockFor("5.0.0")}
	ref, basis, err := ResolveVersion(context.Background(), c, "main.tf", files, "gitlab.example.com/acme", "google", "~> 5.0")
	if err != nil || basis != BasisNone || ref.Version != "" {
		t.Errorf("got %+v %q %v", ref, basis, err)
	}
	if len(c.calls) != 0 {
		t.Errorf("MCP called for non-public provider")
	}
}

func TestResolveVersion_Errors(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("disk on fire")
	if _, _, err := ResolveVersion(ctx, &fakeCaller{}, "main.tf", errFiles{boom}, "hashicorp", "google", ""); !errors.Is(err, boom) {
		t.Errorf("non-NotExist read error must propagate, got %v", err)
	}
	bad := fakeFiles{".terraform.lock.hcl": `provider "x" {`}
	if _, _, err := ResolveVersion(ctx, &fakeCaller{}, "main.tf", bad, "hashicorp", "google", ""); err == nil {
		t.Error("want parse error for malformed lock file")
	}
	mcpErr := errors.New("mcp down")
	c := &fakeCaller{err: mcpErr}
	if _, _, err := ResolveVersion(ctx, c, "main.tf", fakeFiles{}, "hashicorp", "google", "~> 5.0"); !errors.Is(err, mcpErr) {
		t.Errorf("MCP error must be wrapped, got %v", err)
	}
	c = &fakeCaller{replies: map[string]string{ToolLatestProviderVersion: "not a version"}}
	if _, _, err := ResolveVersion(ctx, c, "main.tf", fakeFiles{}, "hashicorp", "google", "~> 5.0"); err == nil {
		t.Error("want error for unparseable latest version")
	}
}
