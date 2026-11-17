// Package render builds MR comment bodies and cleans LLM-originated text.
package render

import "strings"

// Marker returns the hidden marker that identifies a bot comment of the given
// kind ("code" or "plan") and scope (workspace name, or "-" for code).
func Marker(kind, scope string) string {
	return "<!-- tf-review:v1:" + kind + ":" + scope + " -->"
}

// HasMarker reports whether body contains the exact marker for kind and scope
// on a line of its own. Matching whole lines keeps scope "prod" from matching
// the marker of scope "prod-2".
func HasMarker(body, kind, scope string) bool {
	want := Marker(kind, scope)
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSuffix(line, "\r") == want {
			return true
		}
	}
	return false
}
