package tfdocs

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/terraform-agent/internal/model"
)

var google600 = model.ProviderRef{Namespace: "hashicorp", Name: "google", Version: "6.0.0"}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func fixtureCaller(t *testing.T) *fakeCaller {
	t.Helper()
	return &fakeCaller{replies: map[string]string{
		ToolSearchProviderDocs: readFixture(t, "compute_instance_search.txt"),
		ToolProviderDetails:    readFixture(t, "compute_instance_doc.md"),
	}}
}

func TestLookup_PicksExactTitleAndPinnedVersion(t *testing.T) {
	c := fixtureCaller(t)
	ex, err := NewLookuper().Lookup(context.Background(), c, google600, "google_compute_instance", []string{"machine_type"}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.calls) != 2 {
		t.Fatalf("calls = %v", c.calls)
	}
	s := c.calls[0]
	if s.Name != ToolSearchProviderDocs || s.Args["service_slug"] != "compute_instance" ||
		s.Args["provider_version"] != "6.0.0" || s.Args["provider_name"] != "google" ||
		s.Args["provider_namespace"] != "hashicorp" || s.Args["provider_document_type"] != "resources" {
		t.Errorf("search args = %v", s.Args)
	}
	d := c.calls[1]
	if d.Name != ToolProviderDetails || d.Args["provider_doc_id"] != "6299288" {
		t.Errorf("details call = %v (compute_instance doc id is 6299288, not compute_instance_group's)", d)
	}
	want := "https://registry.terraform.io/providers/hashicorp/google/6.0.0/docs/resources/compute_instance"
	if ex.DocURL != want {
		t.Errorf("DocURL = %q", ex.DocURL)
	}
}

func TestLookup_ExcerptKeepsOnlyChangedAttrs(t *testing.T) {
	c := fixtureCaller(t)
	ex, err := NewLookuper().Lookup(context.Background(), c, google600, "google_compute_instance", []string{"machine_type"}, 100000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ex.Text, "`machine_type` - (Required) The machine type to create.") ||
		!strings.Contains(ex.Text, "allow_stopping_for_update") {
		t.Errorf("machine_type entry missing:\n%s", ex.Text)
	}
	for _, other := range []string{"`min_cpu_platform`", "`scratch_disk`", "`tags` -", "## Example Usage"} {
		if strings.Contains(ex.Text, other) {
			t.Errorf("excerpt leaked %s", other)
		}
	}
	if ex.Truncated {
		t.Error("Truncated should be false")
	}
}

func TestLookup_DottedPathMatchesFirstAndLastSegments(t *testing.T) {
	c := fixtureCaller(t)
	attrs := []string{"network_interface.access_config.nat_ip"}
	ex, err := NewLookuper().Lookup(context.Background(), c, google600, "google_compute_instance", attrs, 100000)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"`network_interface`", "`access_config`", "`nat_ip`"} {
		if !strings.Contains(ex.Text, want) {
			t.Errorf("missing bullet %s", want)
		}
	}
	if strings.Contains(ex.Text, "`machine_type`") {
		t.Error("unrelated bullet included")
	}
}

const deprecatedDoc = `# google_x_thing

## Argument Reference

* ` + "`old`" + ` - (Optional, Deprecated) Use new instead.
* ` + "`new`" + ` - (Optional) The replacement.
* ` + "`other`" + ` - (Optional) Unrelated.

## Attributes Reference

* ` + "`id`" + ` - The id.
* ` + "`legacy_id`" + ` - DEPRECATED: do not use.
`

func deprecatedCaller() *fakeCaller {
	return &fakeCaller{replies: map[string]string{
		ToolSearchProviderDocs: "- providerDocID: 42\n- Title: x_thing\n- Category: resources\n- Description: d\n",
		ToolProviderDetails:    deprecatedDoc,
	}}
}

func TestLookup_KeepsDeprecationLines(t *testing.T) {
	ex, err := NewLookuper().Lookup(context.Background(), deprecatedCaller(), google600, "google_x_thing", []string{"new"}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ex.Text, "`new`") || !strings.Contains(ex.Text, "Use new instead.") ||
		!strings.Contains(ex.Text, "DEPRECATED: do not use.") {
		t.Errorf("deprecation lines missing:\n%s", ex.Text)
	}
	if strings.Contains(ex.Text, "Unrelated") {
		t.Error("unrelated bullet included")
	}
	if strings.Count(ex.Text, "Use new instead.") != 1 {
		t.Error("deprecated line duplicated")
	}
}

func TestLookup_TruncatesOnRuneBoundary(t *testing.T) {
	doc := "## Argument Reference\n\n* `name` - " + strings.Repeat("한", 100) + "\n"
	c := &fakeCaller{replies: map[string]string{
		ToolSearchProviderDocs: "- providerDocID: 1\n- Title: x_thing\n",
		ToolProviderDetails:    doc,
	}}
	ex, err := NewLookuper().Lookup(context.Background(), c, google600, "google_x_thing", []string{"name"}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if !ex.Truncated || len(ex.Text) > 50 || !utf8.ValidString(ex.Text) {
		t.Errorf("truncated=%v len=%d valid=%v", ex.Truncated, len(ex.Text), utf8.ValidString(ex.Text))
	}
}

func TestLookup_CachesPerProviderAndResource(t *testing.T) {
	c := fixtureCaller(t)
	l := NewLookuper()
	ctx := context.Background()
	if _, err := l.Lookup(ctx, c, google600, "google_compute_instance", []string{"machine_type"}, 4096); err != nil {
		t.Fatal(err)
	}
	before := len(c.calls)
	ex, err := l.Lookup(ctx, c, google600, "google_compute_instance", []string{"tags"}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.calls) != before {
		t.Errorf("repeat lookup called Caller %d more times", len(c.calls)-before)
	}
	if !strings.Contains(ex.Text, "`tags`") || strings.Contains(ex.Text, "`machine_type`") {
		t.Errorf("excerpt must follow the new attrs:\n%s", ex.Text)
	}
	other := google600
	other.Version = "6.1.0"
	if _, err := l.Lookup(ctx, c, other, "google_compute_instance", nil, 4096); err != nil {
		t.Fatal(err)
	}
	if len(c.calls) == before {
		t.Error("different version must not hit the cache")
	}
}

func TestLookup_Errors(t *testing.T) {
	ctx := context.Background()
	l := NewLookuper()
	if _, err := l.Lookup(ctx, fixtureCaller(t), google600, "google_no_such_thing", nil, 100); !errors.Is(err, ErrDocNotFound) {
		t.Errorf("want ErrDocNotFound, got %v", err)
	}
	if _, err := l.Lookup(ctx, fixtureCaller(t), model.ProviderRef{Namespace: "hashicorp", Name: "google"}, "google_compute_instance", nil, 100); err == nil {
		t.Error("want error for empty version")
	}
	boom := errors.New("mcp down")
	if _, err := l.Lookup(ctx, &fakeCaller{err: boom}, google600, "google_compute_instance", nil, 100); !errors.Is(err, boom) {
		t.Errorf("want wrapped MCP error, got %v", err)
	}
}

func TestLookup_ErrorsDoNotLeakDocContentAndAreNotCached(t *testing.T) {
	c := &fakeCaller{replies: map[string]string{
		ToolSearchProviderDocs: "- providerDocID: 7\n- Title: other\nSECRET-DOC-BODY\n",
	}}
	l := NewLookuper()
	_, err := l.Lookup(context.Background(), c, google600, "google_compute_instance", nil, 100)
	if err == nil || strings.Contains(err.Error(), "SECRET-DOC-BODY") {
		t.Fatalf("err = %v", err)
	}
	c.replies[ToolSearchProviderDocs] = readFixture(t, "compute_instance_search.txt")
	c.replies[ToolProviderDetails] = readFixture(t, "compute_instance_doc.md")
	if _, err := l.Lookup(context.Background(), c, google600, "google_compute_instance", nil, 100); err != nil {
		t.Errorf("failed lookup was cached: %v", err)
	}
}

func TestLookup_KeepsWholeDeprecatedBullet(t *testing.T) {
	doc := "## Argument Reference\n\n* `old` - (Optional) Does a thing.\n    **Deprecated**: use new.\n* `other` - (Optional) Unrelated.\n"
	c := &fakeCaller{replies: map[string]string{
		ToolSearchProviderDocs: "- providerDocID: 1\n- Title: x_thing\n",
		ToolProviderDetails:    doc,
	}}
	ex, err := NewLookuper().Lookup(context.Background(), c, google600, "google_x_thing", []string{"zzz"}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ex.Text, "* `old` - (Optional) Does a thing.") || !strings.Contains(ex.Text, "use new.") {
		t.Errorf("bullet name dropped:\n%s", ex.Text)
	}
	if strings.Contains(ex.Text, "Unrelated") {
		t.Error("unrelated bullet included")
	}
}

func TestLookup_DuplicateExactTitlesFirstWins(t *testing.T) {
	c := &fakeCaller{replies: map[string]string{
		ToolSearchProviderDocs: "- providerDocID: 111\n- Title: x_thing\n---\n- providerDocID: 222\n- Title: x_thing\n",
		ToolProviderDetails:    "doc",
	}}
	if _, err := NewLookuper().Lookup(context.Background(), c, google600, "google_x_thing", nil, 10); err != nil {
		t.Fatal(err)
	}
	if got := c.calls[1].Args["provider_doc_id"]; got != "111" {
		t.Errorf("doc id = %v, want first match 111", got)
	}
}
