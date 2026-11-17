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
		{"sftp broken", "sftp://e/x", 100, "sftp:" + zw + "//e/x"},
		{"ssh broken", "ssh://e/x", 100, "ssh:" + zw + "//e/x"},
		{"git broken", "git://e/x", 100, "git:" + zw + "//e/x"},
		{"underscore prefix scheme broken", "_ftp://e", 100, "_ftp:" + zw + "//e"},
		{"digit prefix scheme broken", "1ftp://e", 100, "1ftp:" + zw + "//e"},
		{"uppercase https removed", "HTTPS://EVIL.example", 100, ""},
		{"xmpp broken", "xmpp:a@b.c", 100, "xmpp:" + zw + "a@" + zw + "b.c"},
		{"www host broken", "go www.evil.example now", 100, "go www." + zw + "evil.example now"},
		{"ftp broken", "ftp://e/x", 100, "ftp:" + zw + "//e/x"},
		{"mailto broken", "mailto:a@b.c", 100, "mailto:" + zw + "a@" + zw + "b.c"},
		{"bare email broken", "a@b.co", 100, "a@" + zw + "b.co"},
		{"brackets escaped", "see [ref] now", 100, "see &#91;ref&#93; now"},
		{"angle brackets escaped", "a < b > c", 100, "a &lt; b &gt; c"},
		{"split reference not a link", "[ref]:\nhttp://e\n\n[ref]", 100, "&#91;ref&#93;:\n\n\n&#91;ref&#93;"},
		{"shortcut reference with definition", "see [ref]\n\n[ref]: http://e", 100, "see &#91;ref&#93;"},
		{"lone CR before slash", "a\r/merge", 100, "a\n" + zw + "/merge"},
		{"decimal entity mention", "&#64;user", 100, "&amp;#64;user"},
		{"hex entity mention", "&#x40;user", 100, "&amp;#x40;user"},
		{"named entity mention", "&commat;user", 100, "&amp;commat;user"},
		{"entity slash at line start", "ok\n&#47;merge", 100, "ok\n&amp;#47;merge"},
		{"ampersand escaped", "R&D", 100, "R&amp;D"},
		{"already escaped amp kept", "R&amp;D", 100, "R&amp;D"},
		{"bare url removed", "see https://evil.example/x?a=1 now", 100, "see  now"},
		{"bare http url removed", "http://evil.example", 100, ""},
		{"reference definition removed", "a\n[ref]: https://evil.example/x\nb", 100, "a\n\nb"},
		{"reference link keeps text", "see [docs][ref] now", 100, "see docs now"},
		{"reference image dropped", "a ![alt][ref] b", 100, "a  b"},
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

func TestSanitizeIsIdempotent(t *testing.T) {
	in := "R&D &#64;user @bob\n/merge [x][r] [s] a<b www.e.example ftp://e mailto:a@b.c https://e.example"
	once := Sanitize(in, 200)
	if twice := Sanitize(once, 200); twice != once {
		t.Fatalf("not idempotent: %q vs %q", once, twice)
	}
}
