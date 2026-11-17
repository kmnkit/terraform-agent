package render

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitize(t *testing.T) {
	const zw = "​"
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"mention neutralized", "cc @alice please", 100, "cc @" + zw + "alice please"},
		{"slash at line start", "/merge", 100, zw + "/merge"},
		{"slash after indent", "ok\n  /approve", 100, "ok\n  " + zw + "/approve"},
		{"leading indent trimmed", "  /approve", 100, zw + "/approve"},
		{"slash on later line", "ok\n/merge now", 100, "ok\n" + zw + "/merge now"},
		{"slash mid-line untouched", "use a/b path", 100, "use a/b path"},
		{"link keeps text", "see [docs](http://x.example/y) now", 100, "see docs now"},
		{"image dropped", "a ![alt](http://x.example/i.png) b", 100, "a  b"},
		{"details tag stripped", "<details>hidden</details>", 100, "hidden"},
		{"img tag stripped", "x<img src=y onerror=z>y", 100, "xy"},
		{"comment stripped", "<!-- c -->text", 100, "text"},
		{"stray comment close", "a --> b", 100, "a  b"},
		{"stray comment open", "a <!-- b", 100, "a  b"},
		{"marker stripped", "x <!-- tf-review:v1:code:- --> y", 100, "x  y"},
		{"marker only", Marker("plan", "prod"), 100, ""},
		{"marker rebuilt after strip", "<<!-- a -->!-- tf-review:v1:code:- -->", 100, ""},
		{"plain text untouched", "리소스 삭제 위험", 100, "리소스 삭제 위험"},
		{"no truncation at limit", "가나다라마", 5, "가나다라마"},
		{"truncated with ellipsis", "가나다라마바", 5, "가나다라…"},
		{"truncate keeps runes whole", "😀😀😀😀", 3, "😀😀…"},
		{"limit one", "abc", 1, "…"},
		{"limit zero", "abc", 0, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Sanitize(tc.in, tc.max)
			if got != tc.want {
				t.Fatalf("Sanitize(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("invalid UTF-8: %q", got)
			}
			if utf8.RuneCountInString(got) > tc.max {
				t.Fatalf("length %d exceeds %d", utf8.RuneCountInString(got), tc.max)
			}
		})
	}
}

func TestSanitizeLeavesNoMarkerDelimiters(t *testing.T) {
	got := Sanitize("a <!-- tf-review:v1:code:- --> b -->c <!--d", 200)
	if strings.Contains(got, "<!--") || strings.Contains(got, "-->") {
		t.Fatalf("delimiters remain: %q", got)
	}
}
