package render

import "testing"

func TestMarker(t *testing.T) {
	got := Marker("plan", "prod")
	want := "<!-- tf-review:v1:plan:prod -->"
	if got != want {
		t.Fatalf("Marker = %q, want %q", got, want)
	}
	if got := Marker("code", "-"); got != "<!-- tf-review:v1:code:- -->" {
		t.Fatalf("code marker = %q", got)
	}
}

func TestHasMarker(t *testing.T) {
	prod := Marker("plan", "prod")
	prod2 := Marker("plan", "prod-2")
	tests := []struct {
		name  string
		body  string
		kind  string
		scope string
		want  bool
	}{
		{"first line", prod + "\n## 제목", "plan", "prod", true},
		{"later own line", "## 제목\n" + prod + "\n본문", "plan", "prod", true},
		{"crlf line endings", prod + "\r\n본문", "plan", "prod", true},
		{"prefix scope does not match", prod2 + "\n본문", "plan", "prod", false},
		{"longer scope wanted, shorter present", prod + "\n본문", "plan", "prod-2", false},
		{"kind mismatch", prod + "\n본문", "code", "prod", false},
		{"mid-line is not a marker", "text " + prod + " text", "plan", "prod", false},
		{"quoted inside a line", "> " + prod, "plan", "prod", false},
		{"empty body", "", "plan", "prod", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasMarker(tc.body, tc.kind, tc.scope); got != tc.want {
				t.Fatalf("HasMarker = %v, want %v", got, tc.want)
			}
		})
	}
}
