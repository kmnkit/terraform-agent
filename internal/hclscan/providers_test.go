package hclscan

import (
	"reflect"
	"strings"
	"testing"
)

func decls(t *testing.T) ProviderDecls {
	t.Helper()
	d, err := ParseProviderDecls(map[string][]byte{
		"versions.tf": fixture(t, "versions.tf"),
		"prod.tfvars": []byte("this is not hcl ==="),
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestParseProviderDecls(t *testing.T) {
	d := decls(t)
	wantSources := map[string]string{
		"google":  "hashicorp/google",
		"acme":    "acme-corp/acme",
		"private": "registry.example.com/corp/private",
	}
	if !reflect.DeepEqual(d.Sources, wantSources) {
		t.Fatalf("sources: got %v, want %v", d.Sources, wantSources)
	}
	wantConstraints := map[string]string{"google": ">= 5.0", "legacy": "~> 2.0"}
	if !reflect.DeepEqual(d.Constraints, wantConstraints) {
		t.Fatalf("constraints: got %v, want %v", d.Constraints, wantConstraints)
	}
}

func TestParseProviderDecls_ErrorNamesFile(t *testing.T) {
	_, err := ParseProviderDecls(map[string][]byte{"broken.tf": fixture(t, "broken.tf")})
	if err == nil || !strings.Contains(err.Error(), "broken.tf") || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("got %v", err)
	}
}

func TestParseProviderDecls_ConfigurationAliasesKeepSource(t *testing.T) {
	d, err := ParseProviderDecls(map[string][]byte{"aliases.tf": fixture(t, "aliases.tf")})
	if err != nil {
		t.Fatal(err)
	}
	if d.Sources["google"] != "acme-corp/google" || d.Constraints["google"] != "~> 6.0" {
		t.Fatalf("got source %q constraint %q", d.Sources["google"], d.Constraints["google"])
	}
	ns, name := ProviderFor(ResourceChange{Type: "google_compute_instance"}, "google.west", d)
	if ns != "acme-corp" || name != "google" {
		t.Fatalf("got %s/%s", ns, name)
	}
}

func TestIsPublicRegistry(t *testing.T) {
	d, err := ParseProviderDecls(map[string][]byte{"aliases.tf": fixture(t, "aliases.tf")})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		local string
		want  bool
	}{
		{"loc", false}, {"ex", false}, {"pub", true}, {"hc", true},
	}
	for _, tt := range tests {
		ns, _ := ProviderFor(ResourceChange{Type: "x_y"}, tt.local, d)
		if got := IsPublicRegistry(ns); got != tt.want {
			t.Errorf("%s: namespace %q public=%v, want %v", tt.local, ns, got, tt.want)
		}
	}
}

func TestProviderFor(t *testing.T) {
	d := decls(t)
	tests := []struct {
		name, typ, meta, wantNS, wantName string
	}{
		{"default hashicorp", "google_compute_instance", "", "hashicorp", "google"},
		{"no declaration for type prefix", "aws_s3_bucket", "", "hashicorp", "aws"},
		{"meta arg wins", "google_compute_instance", "google-beta", "hashicorp", "google-beta"},
		{"alias stripped", "google_compute_instance", "google.west", "hashicorp", "google"},
		{"declared namespace", "acme_widget", "", "acme-corp", "acme"},
		{"declared via meta arg", "other_thing", "acme.east", "acme-corp", "acme"},
		{"non-public host kept", "private_thing", "", "registry.example.com/corp", "private"},
		{"type without underscore", "random", "", "hashicorp", "random"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns, name := ProviderFor(ResourceChange{Type: tt.typ}, tt.meta, d)
			if ns != tt.wantNS || name != tt.wantName {
				t.Fatalf("got %s/%s, want %s/%s", ns, name, tt.wantNS, tt.wantName)
			}
		})
	}
}

func TestLocalName(t *testing.T) {
	tests := []struct{ typ, meta, want string }{
		{"google_storage_bucket", "", "google"},
		{"google_storage_bucket", "google-beta", "google-beta"},
		{"google_storage_bucket", "google.west", "google"},
		{"random", "", "random"},
	}
	for _, tt := range tests {
		if got := LocalName(ResourceChange{Type: tt.typ}, tt.meta); got != tt.want {
			t.Errorf("LocalName(%q, %q) = %q, want %q", tt.typ, tt.meta, got, tt.want)
		}
	}
}
