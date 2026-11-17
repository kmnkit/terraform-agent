package hclscan

import (
	"context"
	"reflect"
	"testing"

	"github.com/terraform-agent/internal/gitlab"
)

func multilineReader(t *testing.T) *fakeReader {
	return &fakeReader{files: map[string][]byte{"main.tf": fixture(t, "multiline.tf")}}
}

func TestChangedResources_MultiLineAttributes(t *testing.T) {
	tests := []struct {
		name, diff string
		want       []string
	}{
		{"heredoc body line", lines("@@ -3,1 +3,1 @@", "-    echo hi", "+    echo hi"), []string{"metadata_startup_script"}},
		{"multi-line object inner line", lines("@@ -8,1 +8,1 @@", "-    env  = \"qa\"", "+    env  = \"dev\""), []string{"labels"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ChangedResources(context.Background(), diffFor(tt.diff), multilineReader(t))
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Resources) != 1 || !reflect.DeepEqual(got.Resources[0].ChangedAttrs, tt.want) {
				t.Fatalf("got %+v, want attrs %v", got.Resources, tt.want)
			}
		})
	}
}

func TestChangedResources_DiffQuirks(t *testing.T) {
	tests := []struct {
		name, diff string
	}{
		{"no newline marker", lines("@@ -6,1 +6,1 @@", "-  name = \"o\"", "\\ No newline at end of file", "+  name = \"n\"", "\\ No newline at end of file")},
		{"omitted hunk count", lines("@@ -6 +6 @@", "-  name = \"o\"", "+  name = \"n\"")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ChangedResources(context.Background(), diffFor(tt.diff), multilineReader(t))
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Resources) != 1 || !reflect.DeepEqual(got.Resources[0].ChangedAttrs, []string{"name"}) {
				t.Fatalf("got %+v", got.Resources)
			}
		})
	}
}

func TestChangedResources_RenamedTFReadsNewPath(t *testing.T) {
	reader := multilineReader(t)
	diffs := []gitlab.FileDiff{{
		OldPath: "old.tf", NewPath: "main.tf", RenamedFile: true,
		Diff: lines("@@ -6,1 +6,1 @@", "-  name = \"o\"", "+  name = \"n\""),
	}}
	got, err := ChangedResources(context.Background(), diffs, reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Resources) != 1 || got.Resources[0].File != "main.tf" {
		t.Fatalf("got %+v", got.Resources)
	}
	if !reflect.DeepEqual(reader.calls, []string{"main.tf"}) {
		t.Fatalf("reader calls: %v", reader.calls)
	}
}

func TestChangedResources_ModuleSourceStripsUserinfo(t *testing.T) {
	tests := []struct{ in, want string }{
		{"git::https://user:token@host.example/x.git", "git::https://host.example/x.git"},
		{"git::ssh://git@host.example/x.git", "git::ssh://host.example/x.git"},
		{"git@github.com:org/repo.git", "github.com:org/repo.git"},
		{"terraform-google-modules/network/google", "terraform-google-modules/network/google"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			src := "module \"m\" {\n  source = \"" + tt.in + "\"\n}\n"
			reader := &fakeReader{files: map[string][]byte{"main.tf": []byte(src)}}
			got, err := ChangedResources(context.Background(), diffFor(lines("@@ -2,1 +2,1 @@", "-  x", "+  y")), reader)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Modules) != 1 || got.Modules[0].Source != tt.want {
				t.Fatalf("got %+v, want source %q", got.Modules, tt.want)
			}
		})
	}
}
