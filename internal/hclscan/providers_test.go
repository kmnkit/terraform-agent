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
