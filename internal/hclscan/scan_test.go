package hclscan

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/terraform-agent/internal/gitlab"
)

type fakeReader struct {
	files map[string][]byte
	err   error
	calls []string
}

func (f *fakeReader) ReadFile(_ context.Context, path string) ([]byte, error) {
	f.calls = append(f.calls, path)
	if f.err != nil {
		return nil, f.err
	}
	b, ok := f.files[path]
	if !ok {
		return nil, errors.New("not found")
	}
	return b, nil
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func lines(s ...string) string { return strings.Join(s, "\n") + "\n" }

func instanceReader(t *testing.T) *fakeReader {
	return &fakeReader{files: map[string][]byte{"main.tf": fixture(t, "instance.tf")}}
}

func diffFor(diff string) []gitlab.FileDiff {
	return []gitlab.FileDiff{{OldPath: "main.tf", NewPath: "main.tf", Diff: diff}}
}

func TestChangedResources_ModifiedAttrsIncludingNested(t *testing.T) {
	diff := lines(
		"@@ -2,5 +2,5 @@ resource \"google_compute_instance\" \"web\" {",
		"   name         = \"web\"",
		"-  machine_type = \"e2-small\"",
		"+  machine_type = \"e2-medium\"",
		"   provider     = google-beta",
		" ",
		"   network_interface {",
		"@@ -9,3 +9,3 @@",
		"     access_config {",
		"-      nat_ip = \"5.6.7.8\"",
		"+      nat_ip = \"1.2.3.4\"",
		"     }",
	)
	got, err := ChangedResources(context.Background(), diffFor(diff), instanceReader(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []ResourceChange{{
		File: "main.tf", Type: "google_compute_instance", Name: "web",
		ChangedAttrs: []string{"machine_type", "network_interface.access_config.nat_ip"},
		ProviderMeta: "google-beta",
	}}
	if !reflect.DeepEqual(got.Resources, want) {
		t.Fatalf("got %+v, want %+v", got.Resources, want)
	}
	if len(got.Modules) != 0 {
		t.Fatalf("unexpected modules: %+v", got.Modules)
	}
}

func TestChangedResources_NewResourceReportsAllAttrs(t *testing.T) {
	diff := lines(
		"@@ -14,0 +15,4 @@",
		"+resource \"google_storage_bucket\" \"logs\" {",
		"+  name     = \"logs\"",
		"+  location = \"US\"",
		"+}",
	)
	got, err := ChangedResources(context.Background(), diffFor(diff), instanceReader(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []ResourceChange{{
		File: "main.tf", Type: "google_storage_bucket", Name: "logs",
		ChangedAttrs: []string{"location", "name"},
	}}
	if !reflect.DeepEqual(got.Resources, want) {
		t.Fatalf("got %+v, want %+v", got.Resources, want)
	}
}

func TestChangedResources_ModuleCalls(t *testing.T) {
	diff := lines(
		"@@ -20,5 +20,5 @@",
		" module \"vpc\" {",
		"   source  = \"terraform-google-modules/network/google\"",
		"-  version = \"~> 8.0\"",
		"+  version = \"~> 9.0\"",
		" }",
		"@@ -25,3 +25,3 @@",
		" module \"dyn\" {",
		"-  source = var.old",
		"+  source = var.src",
		" }",
	)
	got, err := ChangedResources(context.Background(), diffFor(diff), instanceReader(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []ModuleCall{
		{File: "main.tf", Name: "vpc", Source: "terraform-google-modules/network/google", Version: "~> 9.0"},
		{File: "main.tf", Name: "dyn"},
	}
	if !reflect.DeepEqual(got.Modules, want) {
		t.Fatalf("got %+v, want %+v", got.Modules, want)
	}
	if len(got.Resources) != 0 {
		t.Fatalf("unexpected resources: %+v", got.Resources)
	}
}

func TestChangedResources_RemovedResource(t *testing.T) {
	diff := lines(
		"@@ -14,5 +14,0 @@",
		"-resource \"google_storage_bucket\" \"old\" {",
		"-  name = \"old\"",
		"-}",
	)
	got, err := ChangedResources(context.Background(), diffFor(diff), instanceReader(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []ResourceChange{{File: "main.tf", Type: "google_storage_bucket", Name: "old", Removed: true}}
	if !reflect.DeepEqual(got.Resources, want) {
		t.Fatalf("got %+v, want %+v", got.Resources, want)
	}
}

func TestChangedResources_RemovedHeaderStillInHeadIsNotRemoved(t *testing.T) {
	diff := lines(
		"@@ -15,1 +15,1 @@",
		"-resource   \"google_storage_bucket\"   \"logs\" {",
		"+resource \"google_storage_bucket\" \"logs\" {",
	)
	got, err := ChangedResources(context.Background(), diffFor(diff), instanceReader(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range got.Resources {
		if r.Removed {
			t.Fatalf("resource still in head reported removed: %+v", r)
		}
	}
}

func TestChangedResources_DeletedFileDoesNotReadHead(t *testing.T) {
	reader := &fakeReader{}
	diffs := []gitlab.FileDiff{{
		OldPath: "gone.tf", NewPath: "gone.tf", DeletedFile: true,
		Diff: lines(
			"@@ -1,3 +0,0 @@",
			"-resource \"google_pubsub_topic\" \"t\" {",
			"-  name = \"t\"",
			"-}",
		),
	}}
	got, err := ChangedResources(context.Background(), diffs, reader)
	if err != nil {
		t.Fatal(err)
	}
	want := []ResourceChange{{File: "gone.tf", Type: "google_pubsub_topic", Name: "t", Removed: true}}
	if !reflect.DeepEqual(got.Resources, want) {
		t.Fatalf("got %+v, want %+v", got.Resources, want)
	}
	if len(reader.calls) != 0 {
		t.Fatalf("head read for deleted file: %v", reader.calls)
	}
}

func TestChangedResources_IgnoresNonTF(t *testing.T) {
	reader := &fakeReader{}
	body := lines("@@ -1,1 +1,1 @@", "-a = 1", "+a = 2")
	diffs := []gitlab.FileDiff{
		{OldPath: "prod.tfvars", NewPath: "prod.tfvars", Diff: body},
		{OldPath: "main.tf.json", NewPath: "main.tf.json", Diff: body},
		{OldPath: "README.md", NewPath: "README.md", Diff: body},
		{OldPath: "old.tf", NewPath: "new.tfvars", RenamedFile: true, Diff: body},
	}
	got, err := ChangedResources(context.Background(), diffs, reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Resources) != 0 || len(got.Modules) != 0 {
		t.Fatalf("expected empty, got %+v", got)
	}
	if len(reader.calls) != 0 {
		t.Fatalf("FileReader must not be called, got %v", reader.calls)
	}
}

func TestChangedResources_ParseErrorNamesFileWithoutContent(t *testing.T) {
	reader := &fakeReader{files: map[string][]byte{"bad.tf": fixture(t, "broken.tf")}}
	diffs := []gitlab.FileDiff{{
		OldPath: "bad.tf", NewPath: "bad.tf",
		Diff: lines("@@ -1,1 +1,1 @@", "+  name = \"secret-value\""),
	}}
	_, err := ChangedResources(context.Background(), diffs, reader)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "bad.tf") {
		t.Fatalf("error should name file: %v", err)
	}
	if strings.Contains(err.Error(), "secret-value") || strings.Contains(err.Error(), "oops") {
		t.Fatalf("error leaks content: %v", err)
	}
}

func TestChangedResources_ReadErrorIsWrapped(t *testing.T) {
	cause := errors.New("boom")
	reader := &fakeReader{err: cause}
	_, err := ChangedResources(context.Background(), diffFor(lines("@@ -1,1 +1,1 @@", "+a")), reader)
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "main.tf") {
		t.Fatalf("got %v", err)
	}
}

func TestChangedResources_NoOutputContainsValues(t *testing.T) {
	diff := lines("@@ -3,1 +3,1 @@", "-  machine_type = \"x\"", "+  machine_type = \"e2-medium\"")
	got, err := ChangedResources(context.Background(), diffFor(diff), instanceReader(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range got.Resources {
		for _, a := range r.ChangedAttrs {
			if strings.Contains(a, "e2-medium") {
				t.Fatalf("value leaked: %q", a)
			}
		}
	}
}
