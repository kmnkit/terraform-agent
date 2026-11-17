package render

import (
	"regexp"
	"strings"
)

// zeroWidthSpace breaks GitLab mention and quick-action parsing without
// changing how the text reads.
const zeroWidthSpace = "​"

const ellipsis = "…"

var (
	htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	mdImage     = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	mdLink      = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	htmlTag     = regexp.MustCompile(`<[!?/A-Za-z][^>]*>`)
	mention     = regexp.MustCompile(`@(\w)`)
	lineSlash   = regexp.MustCompile(`(?m)^([ \t]*)/`)
)

// Sanitize makes LLM output safe to post as an MR note: it strips links,
// images, HTML and marker text, neutralizes @mentions and leading "/" quick
// actions, and truncates to at most maxLen runes (ending in "…" if cut).
func Sanitize(text string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	text = stripMarkup(text)
	text = mention.ReplaceAllString(text, "@"+zeroWidthSpace+"$1")
	text = lineSlash.ReplaceAllString(text, "$1"+zeroWidthSpace+"/")
	return truncateRunes(strings.TrimSpace(text), maxLen)
}

// stripMarkup removes markup until the text stops changing, so that text
// reassembled from the pieces left by one pass (for example a marker split by
// a nested comment) is removed by the next.
func stripMarkup(text string) string {
	for {
		next := htmlComment.ReplaceAllString(text, "")
		next = mdImage.ReplaceAllString(next, "")
		next = mdLink.ReplaceAllString(next, "$1")
		next = htmlTag.ReplaceAllString(next, "")
		next = strings.ReplaceAll(next, "<!--", "")
		next = strings.ReplaceAll(next, "-->", "")
		if next == text {
			return text
		}
		text = next
	}
}

func truncateRunes(text string, maxLen int) string {
	runes := []rune(text)
	if len(runes) <= maxLen {
		return text
	}
	return string(runes[:maxLen-1]) + ellipsis
}
