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
	mdRefImage  = regexp.MustCompile(`!\[[^\]]*\]\[[^\]]*\]`)
	mdRefLink   = regexp.MustCompile(`\[([^\]]*)\]\[[^\]]*\]`)
	mdRefDef    = regexp.MustCompile(`(?m)^[ \t]{0,3}\[[^\]]+\]:[ \t]*\S.*$`)
	bareURL     = regexp.MustCompile(`(?i)https?://[^\s<>)\]]*`)
	ampersand   = regexp.MustCompile(`&(amp;|lt;|gt;|#91;|#93;)?`)
	wwwHost     = regexp.MustCompile(`(?i)www\.\x{200b}?`)
	urlScheme   = regexp.MustCompile(`:\x{200b}?//`)
	mailScheme  = regexp.MustCompile(`(?i)\b(mailto|xmpp):\x{200b}?`)
	htmlTag     = regexp.MustCompile(`<[!?/A-Za-z][^>]*>`)
	mention     = regexp.MustCompile(`@(\w)`)
	lineSlash   = regexp.MustCompile(`(?m)^([ \t]*)/`)
)

// Sanitize makes LLM output safe to post as an MR note: it neutralizes the
// text (see neutralize) and truncates to at most maxLen runes (ending in "…"
// if cut). render neutralizes untrusted text itself, so Sanitize is for callers
// that also want a length cap; it is idempotent.
func Sanitize(text string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	return truncateRunes(strings.TrimSpace(neutralize(text)), maxLen)
}

var lineEndings = strings.NewReplacer("\r\n", "\n", "\r", "\n")

// neutralize makes untrusted text inert in GitLab markdown: it strips links
// (inline, reference and bare URLs), images, HTML, comments and marker text,
// escapes "&" so entities such as &#64; render literally, and breaks @mentions
// and line-start "/" quick actions with a zero-width space.
func neutralize(text string) string {
	text = lineEndings.Replace(text)
	text = stripMarkup(text)
	text = breakAutolinks(text)
	text = bracketEscaper.Replace(ampersand.ReplaceAllStringFunc(text, escapeAmpersand))
	text = mention.ReplaceAllString(text, "@"+zeroWidthSpace+"$1")
	return lineSlash.ReplaceAllString(text, "$1"+zeroWidthSpace+"/")
}

// bracketEscaper turns characters that can form link or HTML syntax into
// entities, so no inline, reference or shortcut link can be built from text.
var bracketEscaper = strings.NewReplacer("[", "&#91;", "]", "&#93;", "<", "&lt;", ">", "&gt;")

// breakAutolinks inserts a zero-width space after "www.", between ":" and
// "//" of every scheme://, and after mailto: and xmpp:, so GitLab does not
// autolink them. Breaking every "://" (no scheme list, no word boundary)
// covers schemes GitLab accepts such as sftp or ssh. Existing zero-width
// spaces are not doubled, which keeps neutralize idempotent. The bareURL strip
// stays (now case-insensitive) because it removes http(s) links outright.
func breakAutolinks(text string) string {
	text = wwwHost.ReplaceAllString(text, "www."+zeroWidthSpace)
	text = urlScheme.ReplaceAllString(text, ":"+zeroWidthSpace+"//")
	return mailScheme.ReplaceAllString(text, "${1}:"+zeroWidthSpace)
}

// escapeAmpersand turns "&" into "&amp;" but leaves "&amp;" and the entities
// bracketEscaper produces alone. They only render as literal characters, and
// keeping them makes neutralize idempotent.
func escapeAmpersand(m string) string {
	if m != "&" {
		return m
	}
	return "&amp;"
}

// stripMarkup removes markup until the text stops changing, so that text
// reassembled from the pieces left by one pass (for example a marker split by
// a nested comment) is removed by the next.
func stripMarkup(text string) string {
	for {
		next := htmlComment.ReplaceAllString(text, "")
		next = mdImage.ReplaceAllString(next, "")
		next = mdRefImage.ReplaceAllString(next, "")
		next = mdLink.ReplaceAllString(next, "$1")
		next = mdRefLink.ReplaceAllString(next, "$1")
		next = mdRefDef.ReplaceAllString(next, "")
		next = bareURL.ReplaceAllString(next, "")
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
